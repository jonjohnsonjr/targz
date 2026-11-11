package lsp

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/jonjohnsonjr/targz/deflate"
)

func TestCallHierarchyFollowsDeflateDependencies(t *testing.T) {
	source := filepath.Join(t.TempDir(), "repetition.gz")
	var compressed bytes.Buffer
	w := gzip.NewWriter(&compressed)
	if _, err := w.Write(bytes.Repeat([]byte("abc"), 1000)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, compressed.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	listing, err := DisassembleFile(source, false)
	if err != nil {
		t.Fatal(err)
	}

	const uri = "file:///repetition.gz.deflate"
	s := NewServer(bytes.NewReader(nil), &bytes.Buffer{})
	s.docs[uri] = openDocument(uri, string(listing))
	doc := s.docs[uri]
	if doc.loadErr != nil {
		t.Fatal(doc.loadErr)
	}

	matchID := -1
	for _, op := range doc.trace.Ops {
		if op.Kind.String() == "match" {
			matchID = op.ID
			break
		}
	}
	if matchID < 0 {
		t.Fatal("fixture contains no match")
	}
	line := doc.opLine[matchID]
	prepareParams, _ := json.Marshal(map[string]any{
		"textDocument": map[string]any{"uri": uri},
		"position":     map[string]any{"line": line, "character": 1},
	})
	result, err := s.handle("textDocument/prepareCallHierarchy", prepareParams)
	if err != nil {
		t.Fatal(err)
	}
	items := result.([]callHierarchyItem)
	if len(items) != 1 || items[0].Data.Op != matchID {
		t.Fatalf("prepare result = %#v, want match %d", items, matchID)
	}

	itemParams, _ := json.Marshal(map[string]any{"item": items[0]})
	result, err = s.handle("callHierarchy/outgoingCalls", itemParams)
	if err != nil {
		t.Fatal(err)
	}
	outgoing := result.([]outgoingCall)
	if len(outgoing) == 0 {
		t.Fatal("match has no outgoing producer calls")
	}

	producer := outgoing[0].To
	producerParams, _ := json.Marshal(map[string]any{"item": producer})
	result, err = s.handle("callHierarchy/incomingCalls", producerParams)
	if err != nil {
		t.Fatal(err)
	}
	incoming := result.([]incomingCall)
	found := false
	for _, call := range incoming {
		if call.From.Data.Op == matchID {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("producer incoming calls = %#v, want match %d", incoming, matchID)
	}

	result, err = s.handle("textDocument/definition", prepareParams)
	if err != nil {
		t.Fatal(err)
	}
	definitions := result.([]locationLink)
	if len(definitions) != len(outgoing) {
		t.Fatalf("definitions = %d, outgoing calls = %d", len(definitions), len(outgoing))
	}
	for i := range definitions {
		if definitions[i].TargetRange != outgoing[i].To.Range {
			t.Fatalf("definition %d = %#v, outgoing target = %#v", i, definitions[i], outgoing[i].To)
		}
		if definitions[i].OriginSelectionRange != doc.tokens[matchID].Seed {
			t.Fatalf("definition %d origin = %#v, want seed token %#v", i, definitions[i].OriginSelectionRange, doc.tokens[matchID].Seed)
		}
		if definitions[i].TargetSelectionRange != doc.tokens[outgoing[i].To.Data.Op].Output {
			t.Fatalf("definition %d target selection = %#v, want output token %#v", i, definitions[i].TargetSelectionRange, doc.tokens[outgoing[i].To.Data.Op].Output)
		}
	}

	producerLine := doc.opLine[producer.Data.Op]
	producerPosition, _ := json.Marshal(map[string]any{
		"textDocument": map[string]any{"uri": uri},
		"position":     map[string]any{"line": producerLine, "character": 1},
		"context":      map[string]any{"includeDeclaration": false},
	})
	result, err = s.handle("textDocument/references", producerPosition)
	if err != nil {
		t.Fatal(err)
	}
	references := result.([]location)
	found = false
	for _, reference := range references {
		if reference.Range == doc.opRange(matchID) {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("producer references = %#v, want match %d", references, matchID)
	}

	result, err = s.handle("textDocument/implementation", prepareParams)
	if err != nil {
		t.Fatal(err)
	}
	implementations := result.([]locationLink)
	if len(implementations) == 0 {
		t.Fatal("match has no implementations")
	}
	for _, implementation := range implementations {
		id := doc.lineOp[implementation.TargetRange.Start.Line]
		kind := doc.trace.Ops[id].Kind
		if kind != deflate.LiteralOp && kind != deflate.StoredOp {
			t.Fatalf("implementation op %d is %s, want a terminal producer", id, kind)
		}
		if implementation.TargetSelectionRange != doc.tokens[id].Output {
			t.Fatalf("implementation op %d target selection = %#v, want output token %#v", id, implementation.TargetSelectionRange, doc.tokens[id].Output)
		}
	}
}

func TestOpTokenRangesUseUTF16Positions(t *testing.T) {
	line := "%1 literal 'é' out[0:1] bits[0.0:1.0]"
	tokens := opTokenRanges(line, 7)
	// The UTF-8 encoding of é occupies two bytes but one UTF-16 code unit.
	if got, want := tokens.Output.Start.Character, len([]rune("%1 literal 'é' ")); got != want {
		t.Fatalf("output token starts at UTF-16 character %d, want %d", got, want)
	}
	if tokens.Output.Start.Line != 7 || tokens.Output.End.Line != 7 {
		t.Fatalf("output token line = %d:%d, want 7:7", tokens.Output.Start.Line, tokens.Output.End.Line)
	}
}

func TestServeFramesInitializeResponse(t *testing.T) {
	request := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	var input bytes.Buffer
	input.WriteString("Content-Length: ")
	input.WriteString(strconv.Itoa(len(request)))
	input.WriteString("\r\n\r\n")
	input.Write(request)
	var output bytes.Buffer
	if err := NewServer(&input, &output).Serve(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(output.Bytes(), []byte(`"callHierarchyProvider":true`)) {
		t.Fatalf("initialize response does not advertise call hierarchy: %s", output.Bytes())
	}
	for _, capability := range [][]byte{
		[]byte(`"definitionProvider":true`),
		[]byte(`"implementationProvider":true`),
		[]byte(`"referencesProvider":true`),
	} {
		if !bytes.Contains(output.Bytes(), capability) {
			t.Fatalf("initialize response does not advertise %s: %s", capability, output.Bytes())
		}
	}
}
