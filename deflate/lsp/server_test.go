package lsp

import (
	"archive/tar"
	"bytes"
	"compress/flate"
	"compress/gzip"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jonjohnsonjr/targz/deflate"
)

func TestDynamicLiteralDefinitionWalksToRFC(t *testing.T) {
	input := bytes.Repeat([]byte("the quick brown fox jumps over the lazy dog; 0123456789\n"), 300)
	var compressed bytes.Buffer
	w, err := flate.NewWriter(&compressed, flate.BestCompression)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(input); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	trace, err := deflate.ParseRaw(compressed.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	dynamicBlock := -1
	for _, block := range trace.Blocks {
		if block.Kind == deflate.DynamicBlock {
			dynamicBlock = block.ID
			break
		}
	}
	if dynamicBlock < 0 {
		t.Fatalf("fixture blocks = %#v, want a dynamic block", trace.Blocks)
	}
	block := trace.Blocks[dynamicBlock]
	repeated := -1
	for i, run := range block.Dynamic.LengthRuns {
		if run.Repeat > 1 {
			repeated = i
			break
		}
	}
	if repeated < 0 {
		t.Fatal("dynamic block has no repeated code-length run")
	}
	graph := buildEncodingGraph(trace)
	nodes := make(map[string]encodingNode)
	for _, node := range graph.Nodes {
		nodes[node.Label] = node
	}
	run := block.Dynamic.LengthRuns[repeated]
	for slot := run.SlotsStart; slot < run.SlotsEnd; slot++ {
		alphabet, symbol := "litlen", slot
		if slot >= block.Dynamic.NumLiteralLengths {
			alphabet, symbol = "distance", slot-block.Dynamic.NumLiteralLengths
		}
		node, ok := nodes[lengthSlotLabel(block.ID, alphabet, symbol)]
		if !ok {
			t.Fatalf("repeat run slot %d has no expanded assignment", slot)
		}
		if node.Line != 0 || node.Symbol != symbol || node.Parent != lengthRunLabel(block.ID, repeated) {
			t.Fatalf("repeat run slot %d node = %#v", slot, node)
		}
		if node.Text != lengthAssignmentText(run, repeated) {
			t.Fatalf("repeat run slot %d text = %q, want %q", slot, node.Text, lengthAssignmentText(run, repeated))
		}
	}
	runNode := nodes[lengthRunLabel(block.ID, repeated)]
	if runNode.Symbol != repeated || runNode.Parent != codeLengthLabel(block.ID, run.Symbol) ||
		runNode.Text != lengthRunText(trace, block.Dynamic, run) {
		t.Fatalf("repeat run node = %#v", runNode)
	}
	source := filepath.Join(t.TempDir(), "dynamic.deflate")
	if err := os.WriteFile(source, compressed.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	listing, err := DisassembleFileView(source, true, "trace")
	if err != nil {
		t.Fatal(err)
	}

	const uri = "file:///dynamic.deflate"
	s := NewServer(bytes.NewReader(nil), &bytes.Buffer{})
	doc := openDocument(uri, string(listing))
	s.docs[uri] = doc
	if doc.loadErr != nil {
		t.Fatal(doc.loadErr)
	}
	programDoc, err := s.symbolDocument(doc, doc.encoding[lengthRunLabel(block.ID, repeated)])
	if err != nil {
		t.Fatal(err)
	}
	referenceParams, err := json.Marshal(map[string]any{
		"textDocument": map[string]any{"uri": programDoc.uri},
		"position":     map[string]any{"line": repeated, "character": 1},
		"context":      map[string]any{"includeDeclaration": false},
	})
	if err != nil {
		t.Fatal(err)
	}
	referenceResult, err := s.handle("textDocument/references", referenceParams)
	if err != nil {
		t.Fatal(err)
	}
	references := referenceResult.([]location)
	if len(references) != run.Repeat {
		t.Fatalf("length run references = %d, want %d assigned slots", len(references), run.Repeat)
	}
	literalID := -1
	for _, op := range doc.trace.Ops {
		if op.BlockID == dynamicBlock && op.Kind == deflate.LiteralOp {
			literalID = op.ID
			break
		}
	}
	if literalID < 0 {
		t.Fatal("dynamic block has no literal")
	}

	line := doc.opLine[literalID]
	wantLabels := []string{".litlen.", ".lengths.", ".length-program.", ".codelen.", ".hclen."}
	currentURI := uri
	for _, fragment := range wantLabels {
		links := definitionLinksAt(t, s, currentURI, line)
		if len(links) != 1 || strings.HasSuffix(links[0].TargetURI, "/doc/rfc1951.txt") {
			t.Fatalf("definition from %s:%d = %#v, want one generated-document link", currentURI, line, links)
		}
		currentURI = links[0].TargetURI
		line = links[0].TargetSelectionRange.Start.Line
		targetDoc := s.docs[currentURI]
		if targetDoc == nil {
			t.Fatalf("definition target %q is not open", currentURI)
		}
		label := targetDoc.encodingLine[line]
		if !strings.Contains(label, fragment) {
			t.Fatalf("definition target = %q, want fragment %q", label, fragment)
		}
	}
	links := definitionLinksAt(t, s, currentURI, line)
	if len(links) != 1 || !strings.HasSuffix(links[0].TargetURI, "/doc/rfc1951.txt") {
		t.Fatalf("HCLEN definition = %#v, want vendored RFC", links)
	}
	if links[0].TargetSelectionRange.Start.Line != 734 {
		t.Fatalf("RFC selection line = %d, want 734", links[0].TargetSelectionRange.Start.Line)
	}
}

func TestFixedLiteralDefinitionJumpsToRFCTable(t *testing.T) {
	// A final fixed-Huffman block containing the literal 'A' and end-of-block.
	compressed := []byte{0x73, 0x04, 0x00}
	trace, err := deflate.ParseRaw(compressed)
	if err != nil {
		t.Fatal(err)
	}
	if len(trace.Blocks) != 1 || trace.Blocks[0].Kind != deflate.FixedBlock {
		t.Fatalf("blocks = %#v, want one fixed block", trace.Blocks)
	}
	source := filepath.Join(t.TempDir(), "fixed.deflate")
	if err := os.WriteFile(source, compressed, 0o600); err != nil {
		t.Fatal(err)
	}
	listing, err := DisassembleFileView(source, true, "trace")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(listing, []byte("canonical=")) {
		t.Fatalf("trace exposes verbose implementation detail:\n%s", listing)
	}
	wantLines := []string{
		"# DEFLATE trace · gd: value → code → header → RFC 1951",
		"block 0 bits[0.0:2.2] # fixed",
		"01110001  # 'A'",
	}
	for _, want := range wantLines {
		if !bytes.Contains(listing, []byte(want)) {
			t.Fatalf("compact trace missing %q:\n%s", want, listing)
		}
	}

	const uri = "file:///fixed.deflate"
	s := NewServer(bytes.NewReader(nil), &bytes.Buffer{})
	doc := openDocument(uri, string(listing))
	s.docs[uri] = doc
	if doc.loadErr != nil {
		t.Fatal(doc.loadErr)
	}
	first := definitionLinksAt(t, s, uri, doc.opLine[0])
	if len(first) != 1 || !strings.HasSuffix(first[0].TargetURI, ".gzsym") {
		t.Fatalf("literal definition = %#v, want fixed table entry", first)
	}
	entryLine := first[0].TargetSelectionRange.Start.Line
	if entryLine != 65 {
		t.Fatalf("fixed entry line = %d, want symbol 65", entryLine)
	}
	symbolDoc := s.docs[first[0].TargetURI]
	if label := symbolDoc.encodingLine[entryLine]; label != "@b0.litlen.65" {
		t.Fatalf("fixed entry label = %q", label)
	}
	root := definitionLinksAt(t, s, first[0].TargetURI, entryLine)
	if len(root) != 1 || !strings.HasSuffix(root[0].TargetURI, "/doc/rfc1951.txt") {
		t.Fatalf("fixed entry definition = %#v, want vendored RFC", root)
	}
	if root[0].TargetSelectionRange.Start.Line != 662 {
		t.Fatalf("fixed RFC selection line = %d, want 662", root[0].TargetSelectionRange.Start.Line)
	}
}

func definitionLinksAt(t testing.TB, s *Server, uri string, line int) []locationLink {
	return definitionLinksAtCharacter(t, s, uri, line, 1)
}

func definitionLinksAtCharacter(t testing.TB, s *Server, uri string, line, character int) []locationLink {
	t.Helper()
	params, err := json.Marshal(map[string]any{
		"textDocument": map[string]any{"uri": uri},
		"position":     map[string]any{"line": line, "character": character},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := s.handle("textDocument/definition", params)
	if err != nil {
		t.Fatal(err)
	}
	return result.([]locationLink)
}

func TestMatchOperandsHaveIndependentDefinitions(t *testing.T) {
	input := bytes.Repeat([]byte("abc"), 1000)
	var compressed bytes.Buffer
	w, err := flate.NewWriter(&compressed, flate.BestCompression)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(input); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(t.TempDir(), "matches.deflate")
	if err := os.WriteFile(source, compressed.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	listing, err := DisassembleFileView(source, true, "trace")
	if err != nil {
		t.Fatal(err)
	}

	const uri = "file:///matches.deflate"
	s := NewServer(bytes.NewReader(nil), &bytes.Buffer{})
	doc := openDocument(uri, string(listing))
	s.docs[uri] = doc
	if doc.loadErr != nil {
		t.Fatal(doc.loadErr)
	}
	matchID := -1
	for _, op := range doc.trace.Ops {
		if op.Kind == deflate.MatchOp {
			matchID = op.ID
			break
		}
	}
	if matchID < 0 {
		t.Fatal("fixture contains no match")
	}
	tokens := doc.tokens[matchID]
	for name, token := range map[string]lspRange{"length": tokens.Length, "distance": tokens.Distance, "backreference": tokens.Backref} {
		if token.Start == token.End {
			t.Fatalf("%s token is empty", name)
		}
	}

	length := definitionLinksAtCharacter(t, s, uri, tokens.Length.Start.Line, tokens.Length.Start.Character)
	if len(length) != 1 || s.docs[length[0].TargetURI].encodingLine[length[0].TargetSelectionRange.Start.Line] != doc.lengthDefs[matchID] {
		t.Fatalf("length definition = %#v, want %q", length, doc.lengthDefs[matchID])
	}
	distance := definitionLinksAtCharacter(t, s, uri, tokens.Distance.Start.Line, tokens.Distance.Start.Character)
	if len(distance) != 1 || s.docs[distance[0].TargetURI].encodingLine[distance[0].TargetSelectionRange.Start.Line] != doc.distanceDefs[matchID] {
		t.Fatalf("distance definition = %#v, want %q", distance, doc.distanceDefs[matchID])
	}
	if length[0].TargetSelectionRange.Start.Line != doc.trace.Ops[matchID].LengthSymbol {
		t.Fatalf("length table line = %d, want symbol %d", length[0].TargetSelectionRange.Start.Line, doc.trace.Ops[matchID].LengthSymbol)
	}
	if distance[0].TargetSelectionRange.Start.Line != doc.trace.Ops[matchID].DistanceSymbol {
		t.Fatalf("distance table line = %d, want symbol %d", distance[0].TargetSelectionRange.Start.Line, doc.trace.Ops[matchID].DistanceSymbol)
	}
	assignment := definitionLinksAtCharacter(t, s, length[0].TargetURI,
		length[0].TargetSelectionRange.Start.Line, length[0].TargetSelectionRange.Start.Character)
	if len(assignment) != 1 || !strings.Contains(assignment[0].TargetURI, "litlen-lengths.gzsym") {
		t.Fatalf("length symbol parent = %#v, want expanded literal/length assignments", assignment)
	}
	if assignment[0].TargetSelectionRange.Start.Line != doc.trace.Ops[matchID].LengthSymbol {
		t.Fatalf("length assignment line = %d, want symbol %d",
			assignment[0].TargetSelectionRange.Start.Line, doc.trace.Ops[matchID].LengthSymbol)
	}
	assignmentDoc := s.docs[assignment[0].TargetURI]
	assignmentNode := assignmentDoc.encoding[assignmentDoc.encodingLine[assignment[0].TargetSelectionRange.Start.Line]]
	program := definitionLinksAtCharacter(t, s, assignment[0].TargetURI,
		assignment[0].TargetSelectionRange.Start.Line, assignment[0].TargetSelectionRange.Start.Character)
	if len(program) != 1 || !strings.Contains(program[0].TargetURI, "length-program.gzsym") {
		t.Fatalf("length assignment parent = %#v, want compact length program", program)
	}
	programDoc := s.docs[program[0].TargetURI]
	if label := programDoc.encodingLine[program[0].TargetSelectionRange.Start.Line]; label != assignmentNode.Parent {
		t.Fatalf("length program line %d = %q, want %q",
			program[0].TargetSelectionRange.Start.Line, label, assignmentNode.Parent)
	}
	programNode := programDoc.encoding[assignmentNode.Parent]
	codeLength := definitionLinksAtCharacter(t, s, program[0].TargetURI,
		program[0].TargetSelectionRange.Start.Line, program[0].TargetSelectionRange.Start.Character)
	if len(codeLength) != 1 || !strings.Contains(codeLength[0].TargetURI, "codelen.gzsym") {
		t.Fatalf("length program parent = %#v, want code-length symbol table", codeLength)
	}
	codeLengthDoc := s.docs[codeLength[0].TargetURI]
	if label := codeLengthDoc.encodingLine[codeLength[0].TargetSelectionRange.Start.Line]; label != programNode.Parent {
		t.Fatalf("code-length table line %d = %q, want %q",
			codeLength[0].TargetSelectionRange.Start.Line, label, programNode.Parent)
	}
	codeLengthNode := codeLengthDoc.encoding[programNode.Parent]
	hclen := definitionLinksAtCharacter(t, s, codeLength[0].TargetURI,
		codeLength[0].TargetSelectionRange.Start.Line, codeLength[0].TargetSelectionRange.Start.Character)
	if len(hclen) != 1 || !strings.Contains(hclen[0].TargetURI, "hclen.gzsym") {
		t.Fatalf("code-length symbol parent = %#v, want HCLEN vector", hclen)
	}
	hclenDoc := s.docs[hclen[0].TargetURI]
	hclenLine := hclen[0].TargetSelectionRange.Start.Line
	if label := hclenDoc.encodingLine[hclenLine]; label != codeLengthNode.Parent {
		t.Fatalf("HCLEN line %d = %q, want %q", hclenLine, label, codeLengthNode.Parent)
	}
	if hclenDoc.encoding[codeLengthNode.Parent].Symbol != hclenLine {
		t.Fatalf("HCLEN target line %d is not its transmission index", hclenLine)
	}
	for name, first := range map[string]locationLink{"length": length[0], "distance": distance[0]} {
		link := first
		for depth := 0; depth < 6 && !strings.HasSuffix(link.TargetURI, "/doc/rfc1951.txt"); depth++ {
			links := definitionLinksAtCharacter(t, s, link.TargetURI,
				link.TargetSelectionRange.Start.Line, link.TargetSelectionRange.Start.Character)
			if len(links) != 1 {
				t.Fatalf("%s definition depth %d = %#v, want one parent", name, depth, links)
			}
			link = links[0]
		}
		if !strings.HasSuffix(link.TargetURI, "/doc/rfc1951.txt") {
			t.Fatalf("%s definitions terminate at %q, want RFC 1951", name, link.TargetURI)
		}
	}
	backref := definitionLinksAtCharacter(t, s, uri, tokens.Backref.Start.Line, tokens.Backref.Start.Character)
	if len(backref) == 0 {
		t.Fatal("backreference comment has no producer definitions")
	}
	for _, link := range backref {
		if _, ok := doc.lineOp[link.TargetSelectionRange.Start.Line]; !ok {
			t.Fatalf("backreference target is not an operation: %#v", link)
		}
	}

	lineText := strings.Split(string(listing), "\n")[doc.opLine[matchID]]
	if strings.HasPrefix(lineText, "%") || !strings.Contains(lineText, "  # ←") {
		t.Fatalf("match line = %q, want line-number identity and compact backreference comment", lineText)
	}
}

func TestAutoViewDetectsTarAndExposesEntries(t *testing.T) {
	source := filepath.Join(t.TempDir(), "archive.tar.gz")
	var tarBytes bytes.Buffer
	tw := tar.NewWriter(&tarBytes)
	contents := []byte("hello from inside tar\n")
	if err := tw.WriteHeader(&tar.Header{
		Name: "docs/readme.txt", Mode: 0o644, Size: int64(len(contents)),
		ModTime: time.Date(2026, time.November, 11, 12, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(contents); err != nil {
		t.Fatal(err)
	}
	if err := tw.WriteHeader(&tar.Header{Name: "latest", Typeflag: tar.TypeSymlink, Linkname: "docs/readme.txt", Mode: 0o777}); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	var compressed bytes.Buffer
	zw := gzip.NewWriter(&compressed)
	if _, err := zw.Write(tarBytes.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, compressed.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}

	listing, err := DisassembleFile(source, false)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(listing, []byte(`"view":"tar"`)) || !bytes.Contains(listing, []byte("docs/readme.txt")) || !bytes.Contains(listing, []byte("latest → docs/readme.txt")) {
		t.Fatalf("auto tar listing:\n%s", listing)
	}
	traceListing, err := DisassembleFileView(source, false, "trace")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(traceListing, []byte("block 0 bits[")) || bytes.Contains(traceListing, []byte(`"view":"tar"`)) {
		t.Fatalf("forced trace listing:\n%s", traceListing[:min(len(traceListing), 500)])
	}

	const uri = "file:///archive.tar.gz"
	s := NewServer(bytes.NewReader(nil), &bytes.Buffer{})
	s.docs[uri] = openDocument(uri, string(listing))
	doc := s.docs[uri]
	if doc.loadErr != nil {
		t.Fatal(doc.loadErr)
	}
	entry := doc.tar[tarEntryFirstLine]
	if entry.Name != "docs/readme.txt" || entry.Data.Start != 512 || entry.Data.End != 512+deflate.OutPos(len(contents)) {
		t.Fatalf("first tar entry = %#v", entry)
	}

	params, _ := json.Marshal(map[string]any{
		"textDocument": map[string]any{"uri": uri},
		"position":     map[string]any{"line": tarEntryFirstLine, "character": 3},
	})
	result, err := s.handle("textDocument/hover", params)
	if err != nil {
		t.Fatal(err)
	}
	hover := result.(map[string]any)["contents"].(map[string]any)["value"].(string)
	if !strings.Contains(hover, "tar file") || !strings.Contains(hover, "DEFLATE provenance") {
		t.Fatalf("tar hover = %q", hover)
	}

	symbolParams, _ := json.Marshal(map[string]any{"textDocument": map[string]any{"uri": uri}})
	result, err = s.handle("textDocument/documentSymbol", symbolParams)
	if err != nil {
		t.Fatal(err)
	}
	symbols := result.([]documentSymbol)
	if len(symbols) != 2 || symbols[0].Name != "docs/readme.txt" || symbols[1].Name != "latest" {
		t.Fatalf("tar document symbols = %#v", symbols)
	}
}

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

	definitionParams, _ := json.Marshal(map[string]any{
		"textDocument": map[string]any{"uri": uri},
		"position": map[string]any{
			"line": doc.tokens[matchID].Backref.Start.Line, "character": doc.tokens[matchID].Backref.Start.Character,
		},
	})
	result, err = s.handle("textDocument/definition", definitionParams)
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
	line := "101  # 'é'"
	tokens := opTokenRanges(line, 7, deflate.Op{Kind: deflate.LiteralOp})
	// The UTF-8 encoding of é occupies two bytes but one UTF-16 code unit.
	if got, want := tokens.Literal.Start.Character, len([]rune("101  # ")); got != want {
		t.Fatalf("literal token starts at UTF-16 character %d, want %d", got, want)
	}
	if tokens.Literal.Start.Line != 7 || tokens.Literal.End.Line != 7 {
		t.Fatalf("literal token line = %d:%d, want 7:7", tokens.Literal.Start.Line, tokens.Literal.End.Line)
	}
	if got := tokens.Literal.End.Character - tokens.Literal.Start.Character; got != 3 {
		t.Fatalf("literal token is %d UTF-16 units, want 3", got)
	}
}

func TestRenamePreparesLiteralReportsImpactAndRewritesArchive(t *testing.T) {
	source := filepath.Join(t.TempDir(), "rename.gz")
	var compressed bytes.Buffer
	w := gzip.NewWriter(&compressed)
	if _, err := w.Write(bytes.Repeat([]byte("abc"), 400)); err != nil {
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

	const uri = "file:///rename.gz.deflate"
	var output bytes.Buffer
	s := NewServer(bytes.NewReader(nil), &output)
	s.docs[uri] = openDocument(uri, string(listing))
	doc := s.docs[uri]
	var literal deflate.Op
	found := false
	for _, op := range doc.trace.Ops {
		if op.Kind == deflate.LiteralOp && op.Literal == 'a' {
			literal, found = op, true
			break
		}
	}
	if !found {
		t.Fatal("fixture contains no literal 'a'")
	}
	literalRange := doc.tokens[literal.ID].Literal
	params := map[string]any{
		"textDocument": map[string]any{"uri": uri},
		"position": map[string]any{
			"line": literalRange.Start.Line, "character": literalRange.Start.Character,
		},
	}
	prepareParams, _ := json.Marshal(params)
	result, err := s.handle("textDocument/prepareRename", prepareParams)
	if err != nil {
		t.Fatal(err)
	}
	prepared := result.(map[string]any)
	if prepared["range"] != literalRange || prepared["placeholder"] != "'a'" {
		t.Fatalf("prepare rename = %#v, want literal range and placeholder", prepared)
	}

	params["newName"] = "'x'"
	renameParams, _ := json.Marshal(params)
	result, err = s.handle("gzpls/renameImpact", renameParams)
	if err != nil {
		t.Fatal(err)
	}
	impact := result.(renameImpactView)
	if impact.OutputBytes != 400 || impact.Matches == 0 || impact.Members != 1 || !impact.DryRun {
		t.Fatalf("rename impact = %#v, want 400 bytes through matches in one member", impact)
	}

	result, err = s.handle("textDocument/rename", renameParams)
	if err != nil {
		t.Fatal(err)
	}
	edit := result.(map[string]any)
	changes := edit["changes"].(map[string]any)
	if len(changes) != 1 {
		t.Fatalf("rename changes = %#v, want refreshed virtual document", changes)
	}
	rewritten, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	trace, err := deflate.ParseGzip(rewritten)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(trace.Output, bytes.Repeat([]byte("xbc"), 400)) {
		t.Fatalf("rewritten output starts %q, want xbc repeated", trace.Output[:12])
	}
	if !bytes.Contains(output.Bytes(), []byte("400 output bytes")) || !bytes.Contains(output.Bytes(), []byte("re-encoded 1 gzip member")) {
		t.Fatalf("rename notification = %q, want applied blast radius", output.Bytes())
	}
}

func TestParseRenameByte(t *testing.T) {
	for input, want := range map[string]byte{
		"x": 'x', "'x'": 'x', "\"\\n\"": '\n', "0xff": 0xff,
	} {
		got, err := parseRenameByte(input)
		if err != nil {
			t.Errorf("parseRenameByte(%q): %v", input, err)
		} else if got != want {
			t.Errorf("parseRenameByte(%q) = %#x, want %#x", input, got, want)
		}
	}
	for _, input := range []string{"", "ab", "0x100", "💥"} {
		if _, err := parseRenameByte(input); err == nil {
			t.Errorf("parseRenameByte(%q) succeeded, want error", input)
		}
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
		[]byte(`"prepareProvider":true`),
	} {
		if !bytes.Contains(output.Bytes(), capability) {
			t.Fatalf("initialize response does not advertise %s: %s", capability, output.Bytes())
		}
	}
}
