package lsp

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
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
}
