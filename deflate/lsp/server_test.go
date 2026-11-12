package lsp

import (
	"archive/tar"
	"bytes"
	"compress/flate"
	"compress/gzip"
	"encoding/json"
	"fmt"
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
		if node.Text != lengthAssignmentText(run) {
			t.Fatalf("repeat run slot %d text = %q, want %q", slot, node.Text, lengthAssignmentText(run))
		}
		if !strings.HasPrefix(node.Text, "# ") || strings.Contains(node.Text, "run ") || node.Text == "# bits" {
			t.Fatalf("repeat run slot %d text = %q, want only a counted bit comment", slot, node.Text)
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
	hclenNode := s.docs[currentURI].encoding[s.docs[currentURI].encodingLine[line]]
	wantRFC := rfcAnchors[hclenNode.RFCAnchor]
	links := definitionLinksAt(t, s, currentURI, line)
	if len(links) != 1 || !strings.HasSuffix(links[0].TargetURI, "/doc/rfc1951.txt") {
		t.Fatalf("HCLEN definition = %#v, want vendored RFC", links)
	}
	if links[0].TargetSelectionRange != wantRFC.Selection {
		t.Fatalf("RFC selection = %#v, want exact HCLEN token %#v", links[0].TargetSelectionRange, wantRFC.Selection)
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
	if root[0].TargetSelectionRange.Start.Character != 21 || root[0].TargetSelectionRange.End.Character != 28 {
		t.Fatalf("fixed RFC selection = %#v, want exact 0 - 143 range", root[0].TargetSelectionRange)
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
	if length[0].TargetSelectionRange.Start.Line != int(doc.trace.Ops[matchID].LengthSymbol) {
		t.Fatalf("length table line = %d, want symbol %d", length[0].TargetSelectionRange.Start.Line, doc.trace.Ops[matchID].LengthSymbol)
	}
	if distance[0].TargetSelectionRange.Start.Line != int(doc.trace.Ops[matchID].DistanceSymbol) {
		t.Fatalf("distance table line = %d, want symbol %d", distance[0].TargetSelectionRange.Start.Line, doc.trace.Ops[matchID].DistanceSymbol)
	}
	assignment := definitionLinksAtCharacter(t, s, length[0].TargetURI,
		length[0].TargetSelectionRange.Start.Line, length[0].TargetSelectionRange.Start.Character)
	if len(assignment) != 1 || !strings.Contains(assignment[0].TargetURI, "litlen-lengths.gzsym") {
		t.Fatalf("length symbol parent = %#v, want expanded literal/length assignments", assignment)
	}
	if assignment[0].TargetSelectionRange.Start.Line != int(doc.trace.Ops[matchID].LengthSymbol) {
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
	rfc := definitionLinksAtCharacter(t, s, hclen[0].TargetURI,
		hclenLine, hclen[0].TargetSelectionRange.Start.Character)
	if len(rfc) != 1 || rfc[0].TargetSelectionRange != (lspRange{
		Start: position{Line: 736, Character: 65}, End: position{Line: 736, Character: 66},
	}) {
		t.Fatalf("HCLEN[17] RFC definition = %#v, want exact symbol 1 token", rfc)
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

func TestMatchExtraBitsJumpToRFCTableCells(t *testing.T) {
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
	source := filepath.Join(t.TempDir(), "extras.deflate")
	if err := os.WriteFile(source, compressed.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	listing, err := DisassembleFileView(source, true, "trace")
	if err != nil {
		t.Fatal(err)
	}

	const uri = "file:///extras.deflate"
	s := NewServer(bytes.NewReader(nil), &bytes.Buffer{})
	doc := openDocument(uri, string(listing))
	s.docs[uri] = doc
	if doc.loadErr != nil {
		t.Fatal(doc.loadErr)
	}

	lengthChecked, distanceChecked := false, false
	for _, op := range doc.trace.Ops {
		if op.Kind != deflate.MatchOp {
			continue
		}
		tokens := doc.tokens[op.ID]
		if !lengthChecked && op.LengthExtraBits.Len() != 0 {
			if tokens.LengthSymbol == (lspRange{}) || tokens.LengthExtra == (lspRange{}) {
				t.Fatalf("length operand was not split: %#v", tokens)
			}
			symbol := definitionLinksAtCharacter(t, s, uri, tokens.LengthSymbol.Start.Line, tokens.LengthSymbol.Start.Character)
			if len(symbol) != 1 || !strings.HasSuffix(symbol[0].TargetURI, ".gzsym") {
				t.Fatalf("length symbol definition = %#v, want Huffman symbol document", symbol)
			}
			extra := definitionLinksAtCharacter(t, s, uri, tokens.LengthExtra.Start.Line, tokens.LengthExtra.Start.Character)
			want := rfcAnchors[lengthExtraRFCAnchor(int(op.LengthSymbol))].Selection
			if len(extra) != 1 || !strings.HasSuffix(extra[0].TargetURI, "/doc/rfc1951.txt") || extra[0].TargetSelectionRange != want {
				t.Fatalf("length extra definition = %#v, want RFC cell %#v", extra, want)
			}
			lengthChecked = true
		}
		if !distanceChecked && op.DistanceExtraBits.Len() != 0 {
			if tokens.DistanceSymbol == (lspRange{}) || tokens.DistanceExtra == (lspRange{}) {
				t.Fatalf("distance operand was not split: %#v", tokens)
			}
			symbol := definitionLinksAtCharacter(t, s, uri, tokens.DistanceSymbol.Start.Line, tokens.DistanceSymbol.Start.Character)
			if len(symbol) != 1 || !strings.HasSuffix(symbol[0].TargetURI, ".gzsym") {
				t.Fatalf("distance symbol definition = %#v, want Huffman symbol document", symbol)
			}
			extra := definitionLinksAtCharacter(t, s, uri, tokens.DistanceExtra.Start.Line, tokens.DistanceExtra.Start.Character)
			want := rfcAnchors[distanceExtraRFCAnchor(int(op.DistanceSymbol))].Selection
			if len(extra) != 1 || !strings.HasSuffix(extra[0].TargetURI, "/doc/rfc1951.txt") || extra[0].TargetSelectionRange != want {
				t.Fatalf("distance extra definition = %#v, want RFC cell %#v", extra, want)
			}
			distanceChecked = true
		}
	}
	if !lengthChecked || !distanceChecked {
		t.Fatalf("fixture extra-bit coverage: length=%v distance=%v", lengthChecked, distanceChecked)
	}
}

func TestCodeLengthRepeatExtraBitsJumpToRFCRules(t *testing.T) {
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
	source := filepath.Join(t.TempDir(), "repeat.deflate")
	if err := os.WriteFile(source, compressed.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	listing, err := DisassembleFileView(source, true, "trace")
	if err != nil {
		t.Fatal(err)
	}

	const uri = "file:///repeat.deflate"
	s := NewServer(bytes.NewReader(nil), &bytes.Buffer{})
	doc := openDocument(uri, string(listing))
	s.docs[uri] = doc
	for _, block := range doc.trace.Blocks {
		if block.Dynamic == nil {
			continue
		}
		for runIndex, run := range block.Dynamic.LengthRuns {
			if run.ExtraBits.Len() == 0 {
				continue
			}
			node := doc.encoding[lengthRunLabel(block.ID, runIndex)]
			program, err := s.symbolDocument(doc, node)
			if err != nil {
				t.Fatal(err)
			}
			tokens := program.encodingTok[node.Label]
			if tokens.Primary == (lspRange{}) || tokens.Extra == (lspRange{}) {
				t.Fatalf("repeat instruction was not split: %#v", tokens)
			}
			symbol := definitionLinksAtCharacter(t, s, program.uri, tokens.Primary.Start.Line, tokens.Primary.Start.Character)
			if len(symbol) != 1 || !strings.Contains(symbol[0].TargetURI, "codelen.gzsym") {
				t.Fatalf("repeat symbol definition = %#v, want code-length table", symbol)
			}
			extra := definitionLinksAtCharacter(t, s, program.uri, tokens.Extra.Start.Line, tokens.Extra.Start.Character)
			want := rfcAnchors[codeLengthExtraRFCAnchor(run.Symbol)].Selection
			if len(extra) != 1 || !strings.HasSuffix(extra[0].TargetURI, "/doc/rfc1951.txt") || extra[0].TargetSelectionRange != want {
				t.Fatalf("repeat extra definition = %#v, want RFC rule %#v", extra, want)
			}
			return
		}
	}
	t.Fatal("fixture has no code-length repeat with extra bits")
}

func TestBlockHeaderFieldsNavigateAndHover(t *testing.T) {
	input := bytes.Repeat([]byte("header fields should be inspectable\n"), 500)
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
	source := filepath.Join(t.TempDir(), "headers.deflate")
	if err := os.WriteFile(source, compressed.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	listing, err := DisassembleFileView(source, true, "trace")
	if err != nil {
		t.Fatal(err)
	}
	const uri = "file:///headers.deflate"
	s := NewServer(bytes.NewReader(nil), &bytes.Buffer{})
	doc := openDocument(uri, string(listing))
	s.docs[uri] = doc
	if doc.loadErr != nil {
		t.Fatal(doc.loadErr)
	}

	block := doc.trace.Blocks[0]
	fields := map[string]string{"bfinal": "term-bfinal", "btype": "term-btype"}
	if block.Dynamic != nil {
		fields["hlit"] = "term-hlit"
		fields["hdist"] = "term-hdist"
		fields["hclen"] = "term-hclen"
	}
	for field, anchorName := range fields {
		node := doc.encoding[headerLabel(block.ID, field)]
		if node.Label == "" {
			t.Fatalf("missing %s header node", field)
		}
		selection := doc.encodingSelection(node)
		links := definitionLinksAtCharacter(t, s, uri, selection.Start.Line, selection.Start.Character)
		if len(links) != 1 || links[0].TargetSelectionRange != rfcAnchors[anchorName].Selection {
			t.Fatalf("%s definition = %#v", field, links)
		}
		params, _ := json.Marshal(map[string]any{
			"textDocument": map[string]any{"uri": uri},
			"position":     selection.Start,
		})
		hoverResult, err := s.handle("textDocument/hover", params)
		if err != nil {
			t.Fatal(err)
		}
		hover := hoverResult.(map[string]any)["contents"].(map[string]any)["value"].(string)
		if !strings.Contains(hover, node.Name) || !strings.Contains(hover, "RFC 1951") {
			t.Fatalf("%s hover = %q", field, hover)
		}
	}
}

func TestStoredHeaderLengthFieldsNavigate(t *testing.T) {
	var compressed bytes.Buffer
	w, err := flate.NewWriter(&compressed, flate.NoCompression)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("stored block")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(t.TempDir(), "stored.deflate")
	if err := os.WriteFile(source, compressed.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	listing, err := DisassembleFileView(source, true, "trace")
	if err != nil {
		t.Fatal(err)
	}
	const uri = "file:///stored.deflate"
	s := NewServer(bytes.NewReader(nil), &bytes.Buffer{})
	doc := openDocument(uri, string(listing))
	s.docs[uri] = doc
	if doc.trace.Blocks[0].Kind != deflate.StoredBlock {
		t.Fatalf("block kind = %s, want stored", doc.trace.Blocks[0].Kind)
	}
	for field, anchorName := range map[string]string{"len": "term-len", "nlen": "term-nlen"} {
		node := doc.encoding[headerLabel(0, field)]
		selection := doc.encodingSelection(node)
		links := definitionLinksAtCharacter(t, s, uri, selection.Start.Line, selection.Start.Character)
		if len(links) != 1 || links[0].TargetSelectionRange != rfcAnchors[anchorName].Selection {
			t.Fatalf("%s definition = %#v", field, links)
		}
	}
}

func TestRFCDefinitionsResolveTermsAndSections(t *testing.T) {
	rfcPath := filepath.Join("..", "..", "doc", "rfc1951.txt")
	contents, err := os.ReadFile(rfcPath)
	if err != nil {
		t.Fatal(err)
	}
	uri := fileURI(rfcPath)
	s := NewServer(bytes.NewReader(nil), &bytes.Buffer{})
	doc := openDocument(uri, string(contents))
	s.docs[uri] = doc
	if doc.rfcLines == nil || doc.loadErr != nil {
		t.Fatalf("RFC document = %#v", doc)
	}

	tests := []struct {
		name      string
		line, col int
		anchor    string
	}{
		{"BTYPE use", 575, 39, "term-btype"},
		{"HLIT use", 743, 16, "term-hlit"},
		{"literal length alphabet", 743, 55, "term-literal-length"},
		{"distance alphabet", 746, 60, "term-distance"},
		{"code length alphabet", 744, 40, "term-code-length"},
		{"section cross reference", 686, 30, "section-3.2.5"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			links := definitionLinksAtCharacter(t, s, uri, test.line, test.col)
			want := rfcAnchors[test.anchor].Selection
			if len(links) != 1 || links[0].TargetURI != uri || links[0].TargetSelectionRange != want {
				t.Fatalf("definition = %#v, want %s at %#v", links, test.anchor, want)
			}
		})
	}

	canonical := rfcAnchors["term-hclen"].Selection.Start
	if links := definitionLinksAtCharacter(t, s, uri, canonical.Line, canonical.Character); len(links) != 0 {
		t.Fatalf("canonical HCLEN definition loops to itself: %#v", links)
	}
	hoverParams, _ := json.Marshal(map[string]any{
		"textDocument": map[string]any{"uri": uri},
		"position":     map[string]any{"line": 575, "character": 39},
	})
	hoverResult, err := s.handle("textDocument/hover", hoverParams)
	if err != nil {
		t.Fatal(err)
	}
	hover := hoverResult.(map[string]any)["contents"].(map[string]any)["value"].(string)
	if !strings.Contains(hover, "RFC 1951") || !strings.Contains(hover, "BTYPE") {
		t.Fatalf("RFC hover = %q", hover)
	}
}

func TestRFCAnchorSelectionsRemainExact(t *testing.T) {
	contents, err := os.ReadFile(filepath.Join("..", "..", "doc", "rfc1951.txt"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(contents), "\n")
	expected := map[string]string{
		"term-bfinal":         "BFINAL",
		"term-btype":          "BTYPE",
		"term-literal-length": "literal/length",
		"term-distance":       "distance",
		"term-code-length":    "alphabet for code lengths",
		"term-hlit":           "HLIT",
		"term-hdist":          "HDIST",
		"term-hclen":          "HCLEN",
		"term-len":            "LEN",
		"term-nlen":           "NLEN",
		"repeat-extra-16":     "2 bits",
		"repeat-extra-17":     "3 bits",
		"repeat-extra-18":     "7 bits",
		"section-3.2.1":       "3.2.1",
		"section-3.2.2":       "3.2.2",
		"section-3.2.3":       "3.2.3",
		"section-3.2.4":       "3.2.4",
		"section-3.2.5":       "3.2.5",
		"section-3.2.6":       "3.2.6",
		"section-3.2.7":       "3.2.7",
	}
	for name, want := range expected {
		got := selectedRFCText(lines, rfcAnchors[name].Selection)
		if got != want {
			t.Errorf("%s selects %q, want %q", name, got, want)
		}
	}

	hclenOrder := []int{16, 17, 18, 0, 8, 7, 9, 6, 10, 5, 11, 4, 12, 3, 13, 2, 14, 1, 15}
	for field, symbol := range hclenOrder {
		name := hclenRFCAnchor(field)
		if got, want := selectedRFCText(lines, rfcAnchors[name].Selection), strconv.Itoa(symbol); got != want {
			t.Errorf("%s selects %q, want %q", name, got, want)
		}
	}
	for symbol := 257; symbol <= 285; symbol++ {
		extra := 0
		switch {
		case symbol >= 281 && symbol <= 284:
			extra = 5
		case symbol >= 277 && symbol <= 280:
			extra = 4
		case symbol >= 273 && symbol <= 276:
			extra = 3
		case symbol >= 269 && symbol <= 272:
			extra = 2
		case symbol >= 265 && symbol <= 268:
			extra = 1
		}
		name := lengthExtraRFCAnchor(symbol)
		if got, want := selectedRFCText(lines, rfcAnchors[name].Selection), strconv.Itoa(extra); got != want {
			t.Errorf("%s selects %q, want %q", name, got, want)
		}
	}
	for symbol := 0; symbol <= 29; symbol++ {
		extra := 0
		if symbol >= 4 {
			extra = symbol/2 - 1
		}
		name := distanceExtraRFCAnchor(symbol)
		if got, want := selectedRFCText(lines, rfcAnchors[name].Selection), strconv.Itoa(extra); got != want {
			t.Errorf("%s selects %q, want %q", name, got, want)
		}
	}
}

func selectedRFCText(lines []string, selection lspRange) string {
	line := lines[selection.Start.Line]
	return line[selection.Start.Character:selection.End.Character]
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
	if doc.trace != nil {
		t.Fatal("opening a tar catalog eagerly loaded the DEFLATE trace")
	}
	var directory, linkEntry tarEntry
	for _, candidate := range doc.tar {
		if candidate.Name == "docs" {
			directory = candidate
		}
		if candidate.Name == "latest" {
			linkEntry = candidate
		}
	}
	if directory.Name == "" || linkEntry.Name == "" {
		t.Fatalf("tar tree entries = %#v", doc.tar)
	}
	if !bytes.Contains(listing, []byte("├── docs/")) || !bytes.Contains(listing, []byte("└── latest → docs/readme.txt")) {
		t.Fatalf("directory-scoped tar root:\n%s", listing)
	}
	directoryDefinition := definitionLinksAtCharacter(t, s, uri, directory.NameToken.Start.Line, directory.NameToken.Start.Character)
	if len(directoryDefinition) != 1 || !strings.Contains(directoryDefinition[0].TargetURI, "/.gzpls/tree/docs.deflate") {
		t.Fatalf("tar directory definition = %#v", directoryDefinition)
	}
	directoryURI := directoryDefinition[0].TargetURI
	directoryDoc := s.docs[directoryURI]
	if directoryDoc == nil || directoryDoc.meta.Entry != "docs" {
		t.Fatalf("tar directory document = %#v", directoryDoc)
	}
	var entry tarEntry
	for _, candidate := range directoryDoc.tar {
		if candidate.Name == "docs/readme.txt" && !candidate.Synthetic {
			entry = candidate
		}
	}
	if entry.Name == "" {
		t.Fatalf("docs directory entries = %#v", directoryDoc.tar)
	}
	if entry.Name != "docs/readme.txt" || entry.Data.Start != 512 || entry.Data.End != 512+deflate.OutPos(len(contents)) {
		t.Fatalf("first tar entry = %#v", entry)
	}

	params, _ := json.Marshal(map[string]any{
		"textDocument": map[string]any{"uri": directoryURI},
		"position":     entry.NameToken.Start,
	})
	result, err := s.handle("textDocument/hover", params)
	if err != nil {
		t.Fatal(err)
	}
	hover := result.(map[string]any)["contents"].(map[string]any)["value"].(string)
	if !strings.Contains(hover, "tar file") || strings.Contains(hover, "DEFLATE provenance") {
		t.Fatalf("tar hover = %q", hover)
	}

	symbolParams, _ := json.Marshal(map[string]any{"textDocument": map[string]any{"uri": uri}})
	result, err = s.handle("textDocument/documentSymbol", symbolParams)
	if err != nil {
		t.Fatal(err)
	}
	symbols := result.([]documentSymbol)
	if len(symbols) != 2 || symbols[0].Name != "docs/" || len(symbols[0].Children) != 0 || symbols[1].Name != "latest" {
		t.Fatalf("tar document symbols = %#v", symbols)
	}

	definition := definitionLinksAtCharacter(t, s, directoryURI, entry.NameToken.Start.Line, entry.NameToken.Start.Character)
	if len(definition) != 1 || !strings.HasSuffix(definition[0].TargetURI, "/docs/readme.txt") {
		t.Fatalf("tar file definition = %#v, want extracted readme", definition)
	}
	extractedPath := strings.TrimPrefix(definition[0].TargetURI, "file://")
	extracted, err := os.ReadFile(extractedPath)
	if err != nil || !bytes.Equal(extracted, contents) {
		t.Fatalf("extracted tar definition = %q, %v", extracted, err)
	}
	info, err := os.Stat(extractedPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o222 != 0 {
		t.Fatalf("extracted tar definition mode = %v; want read-only", info.Mode())
	}
	if doc.trace != nil {
		t.Fatal("extracting one tar member eagerly loaded the DEFLATE trace")
	}
	didOpen, _ := json.Marshal(map[string]any{
		"textDocument": map[string]any{"uri": definition[0].TargetURI, "text": string(contents)},
	})
	if _, err := s.handle("textDocument/didOpen", didOpen); err != nil {
		t.Fatal(err)
	}
	plainImplementation, _ := json.Marshal(map[string]any{
		"textDocument": map[string]any{"uri": definition[0].TargetURI},
		"position":     map[string]any{"line": 0, "character": 1},
	})
	result, err = s.handle("textDocument/implementation", plainImplementation)
	if err != nil {
		t.Fatal(err)
	}
	plainLinks := result.([]locationLink)
	if len(plainLinks) != 1 || !strings.Contains(plainLinks[0].TargetURI, fmt.Sprintf("~%d-%d.deflate", entry.Data.Start+1, entry.Data.Start+2)) {
		t.Fatalf("plaintext implementation = %#v", plainLinks)
	}
	plainProvenance := s.docs[plainLinks[0].TargetURI]
	plainLine := plainLinks[0].TargetSelectionRange.Start.Line
	toTrace := definitionLinksAtCharacter(t, s, plainProvenance.uri, plainLine, 0)
	if len(toTrace) == 0 {
		t.Fatal("plaintext provenance did not drill into the trace")
	}
	backToPlain := definitionLinksAtCharacter(t, s, toTrace[0].TargetURI, toTrace[0].TargetSelectionRange.Start.Line, toTrace[0].TargetSelectionRange.Start.Character)
	if len(backToPlain) != 1 || backToPlain[0].TargetURI != definition[0].TargetURI {
		t.Fatalf("trace output definition = %#v, want plaintext", backToPlain)
	}
	linkDefinition := definitionLinksAtCharacter(t, s, uri, linkEntry.LinkToken.Start.Line, linkEntry.LinkToken.Start.Character)
	if len(linkDefinition) != 1 || linkDefinition[0].TargetURI != directoryURI || linkDefinition[0].TargetSelectionRange != entry.NameToken {
		t.Fatalf("tar link definition = %#v, want readme entry", linkDefinition)
	}
	referenceParams, _ := json.Marshal(map[string]any{
		"textDocument": map[string]any{"uri": directoryURI},
		"position":     entry.NameToken.Start,
		"context":      map[string]any{"includeDeclaration": false},
	})
	result, err = s.handle("textDocument/references", referenceParams)
	if err != nil {
		t.Fatal(err)
	}
	references := result.([]location)
	if len(references) != 1 || references[0].URI != uri || references[0].Range != linkEntry.LinkToken {
		t.Fatalf("tar references = %#v, want inbound link", references)
	}
	linkReferenceParams, _ := json.Marshal(map[string]any{
		"textDocument": map[string]any{"uri": uri},
		"position":     linkEntry.LinkToken.Start,
		"context":      map[string]any{"includeDeclaration": true},
	})
	result, err = s.handle("textDocument/references", linkReferenceParams)
	if err != nil {
		t.Fatal(err)
	}
	linkReferences := result.([]location)
	if len(linkReferences) != 2 || linkReferences[0].URI != directoryURI || linkReferences[0].Range != entry.NameToken || linkReferences[1].URI != uri || linkReferences[1].Range != linkEntry.LinkToken {
		t.Fatalf("tar link-target references = %#v, want declaration and inbound link", linkReferences)
	}
	implementationParams, _ := json.Marshal(map[string]any{
		"textDocument": map[string]any{"uri": directoryURI},
		"position":     entry.NameToken.Start,
	})
	result, err = s.handle("textDocument/implementation", implementationParams)
	if err != nil {
		t.Fatal(err)
	}
	implementations := result.([]locationLink)
	if len(implementations) != 1 || !strings.Contains(implementations[0].TargetURI, "/.gzpls/provenance/") {
		t.Fatalf("tar implementations = %#v, want one summarized provenance document", implementations)
	}
	if doc.trace == nil {
		t.Fatal("provenance navigation did not load the DEFLATE trace on demand")
	}
	provenanceDoc := s.docs[implementations[0].TargetURI]
	if provenanceDoc == nil || provenanceDoc.meta.View != "provenance" || len(provenanceDoc.provenance) == 0 {
		t.Fatalf("provenance document = %#v", provenanceDoc)
	}
	if provenanceDoc.trace != doc.trace {
		t.Fatal("generated provenance document reparsed instead of sharing the archive trace")
	}
	provenanceLine := implementations[0].TargetSelectionRange.Start.Line
	drilldown := definitionLinksAtCharacter(t, s, implementations[0].TargetURI, provenanceLine, 0)
	if len(drilldown) == 0 || !strings.HasSuffix(drilldown[0].TargetURI, "/.gzpls/trace.deflate") {
		t.Fatalf("provenance drilldown = %#v, want trace operations", drilldown)
	}
	if traceDoc := s.docs[drilldown[0].TargetURI]; traceDoc == nil || traceDoc.trace != doc.trace {
		t.Fatal("generated trace document reparsed instead of sharing the archive trace")
	}
	linkImplementationParams, _ := json.Marshal(map[string]any{
		"textDocument": map[string]any{"uri": uri},
		"position":     linkEntry.LinkToken.Start,
	})
	result, err = s.handle("textDocument/implementation", linkImplementationParams)
	if err != nil {
		t.Fatal(err)
	}
	if linkImplementations := result.([]locationLink); len(linkImplementations) != len(implementations) || linkImplementations[0].TargetURI != implementations[0].TargetURI || linkImplementations[0].OriginSelectionRange != linkEntry.LinkToken {
		t.Fatalf("tar link-target implementations = %d, want %d", len(linkImplementations), len(implementations))
	}
}

func TestAutoViewShowsNonTarContent(t *testing.T) {
	writeGzip := func(path string, payload []byte) {
		t.Helper()
		var compressed bytes.Buffer
		zw := gzip.NewWriter(&compressed)
		if _, err := zw.Write(payload); err != nil {
			t.Fatal(err)
		}
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, compressed.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	text := []byte("hello from an ordinary gzip\n# gzpls is ordinary content here\nsecond line\n")
	source := filepath.Join(t.TempDir(), "hello.txt.gz")
	writeGzip(source, text)
	listing, err := DisassembleFileView(source, false, "auto")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(listing, text) {
		t.Fatalf("auto text view = %q, want decoded payload", listing)
	}

	uri := fileURI(source)
	doc := openDocument(uri, string(listing))
	if doc.loadErr != nil {
		t.Fatal(doc.loadErr)
	}
	if doc.plain == nil || doc.meta.View != "content" || doc.plain.entry.Data.End != deflate.OutPos(len(text)) {
		t.Fatalf("content document = %#v", doc)
	}
	s := NewServer(bytes.NewReader(nil), &bytes.Buffer{})
	s.docs[uri] = doc
	params, _ := json.Marshal(map[string]any{
		"textDocument": map[string]any{"uri": uri},
		"position":     map[string]any{"line": 0, "character": 1},
	})
	result, err := s.handle("textDocument/implementation", params)
	if err != nil {
		t.Fatal(err)
	}
	links := result.([]locationLink)
	if len(links) != 1 || !strings.Contains(links[0].TargetURI, "hello.txt~1-2.deflate") {
		t.Fatalf("plaintext implementation = %#v", links)
	}
	provenance := s.docs[links[0].TargetURI]
	toTrace := definitionLinksAtCharacter(t, s, provenance.uri, links[0].TargetSelectionRange.Start.Line, 0)
	if len(toTrace) == 0 {
		t.Fatal("root plaintext provenance did not drill into the trace")
	}
	backToPlain := definitionLinksAtCharacter(t, s, toTrace[0].TargetURI, toTrace[0].TargetSelectionRange.Start.Line, toTrace[0].TargetSelectionRange.Start.Character)
	if len(backToPlain) != 1 || backToPlain[0].TargetURI != uri {
		t.Fatalf("root trace output definition = %#v, want plaintext", backToPlain)
	}

	trace, err := DisassembleFileView(source, false, "trace")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(trace, []byte("block 0 bits[")) {
		t.Fatalf("forced trace view:\n%s", trace)
	}

	binary := []byte{0, 1, 'A', 0xff}
	binarySource := filepath.Join(t.TempDir(), "data.bin.gz")
	writeGzip(binarySource, binary)
	hex, err := DisassembleFileView(binarySource, false, "auto")
	if err != nil {
		t.Fatal(err)
	}
	if want := renderHexPayload(binary); !bytes.Equal(hex, want) {
		t.Fatalf("auto binary view = %q, want %q", hex, want)
	}
	hexDoc := openDocument(fileURI(binarySource), string(hex))
	if hexDoc.loadErr != nil || hexDoc.meta.View != "hex" || hexDoc.plain != nil {
		t.Fatalf("hex document = %#v", hexDoc)
	}
}

func TestRenderHexPayload(t *testing.T) {
	got := string(renderHexPayload([]byte{0, 1, 'A', 0xff}))
	want := "00000000: 00 01 41 ff                                      |..A.|\n"
	if got != want {
		t.Fatalf("renderHexPayload() = %q, want %q", got, want)
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
	listing, err := DisassembleFileView(source, false, "trace")
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
	listing, err := DisassembleFileView(source, false, "trace")
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
	oldDefinition := definitionLinksAtCharacter(t, s, uri, literalRange.Start.Line, literalRange.Start.Character)
	if len(oldDefinition) != 1 || !strings.HasSuffix(oldDefinition[0].TargetURI, ".gzsym") {
		t.Fatalf("pre-rename literal definition = %#v", oldDefinition)
	}
	oldSymbolURI := oldDefinition[0].TargetURI
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
	refreshed := s.docs[uri]
	var renamed deflate.Op
	found = false
	for _, op := range refreshed.trace.Ops {
		if op.Kind == deflate.LiteralOp && op.Literal == 'x' {
			renamed, found = op, true
			break
		}
	}
	if !found {
		t.Fatal("refreshed trace contains no literal 'x'")
	}
	renamedRange := refreshed.tokens[renamed.ID].Literal
	newDefinition := definitionLinksAtCharacter(t, s, uri, renamedRange.Start.Line, renamedRange.Start.Character)
	if len(newDefinition) != 1 || !strings.HasSuffix(newDefinition[0].TargetURI, ".gzsym") {
		t.Fatalf("post-rename literal definition = %#v", newDefinition)
	}
	if newDefinition[0].TargetURI == oldSymbolURI {
		t.Fatalf("rename reused stale generated document %q", oldSymbolURI)
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
