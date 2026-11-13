// Package lsp exposes a deliberately straight-faced language server for
// DEFLATE instruction listings.
package lsp

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/jonjohnsonjr/targz/deflate"
)

const headerPrefix = "# gzpls "

type metadata struct {
	Source      string `json:"source"`
	Raw         bool   `json:"raw"`
	View        string `json:"view,omitempty"`
	RFC         string `json:"rfc,omitempty"`
	TraceURI    string `json:"traceUri,omitempty"`
	Block       int    `json:"block,omitempty"`
	Alphabet    string `json:"alphabet,omitempty"`
	Entry       string `json:"entry,omitempty"`
	OutputStart uint64 `json:"outputStart,omitempty"`
	OutputEnd   uint64 `json:"outputEnd,omitempty"`
}

type document struct {
	uri          string
	archiveURI   string
	meta         metadata
	trace        *deflate.Trace
	revision     [sha256.Size]byte
	end          position
	lineOp       map[int]int
	opLine       map[int]int
	lineLen      map[int]int
	tokens       map[int]opTokens
	tar          map[int]tarEntry
	encoding     map[string]encodingNode
	encodingLine map[int]string
	encodingTok  map[string]encodingTokens
	literalDefs  map[int]string
	lengthDefs   map[int]string
	distanceDefs map[int]string
	rfcLines     []string
	provenance   map[int][]int
	loadErr      error
	plain        *plaintextDocument
}

type plaintextDocument struct {
	entry      tarEntry
	text       string
	lineStarts []int
}

// ViewProgress is a stable, line-oriented progress event for editor clients
// which need to occupy a buffer while auto-view detection and indexing run.
type ViewProgress struct {
	Phase      string `json:"phase"`
	Detail     string `json:"detail,omitempty"`
	View       string `json:"view,omitempty"`
	Compressed int64  `json:"compressed,omitempty"`
	Decoded    uint64 `json:"decoded,omitempty"`
	Entries    uint64 `json:"entries,omitempty"`
}

type opTokens struct {
	Bits           lspRange
	Literal        lspRange
	Length         lspRange
	LengthSymbol   lspRange
	LengthExtra    lspRange
	Distance       lspRange
	DistanceSymbol lspRange
	DistanceExtra  lspRange
	Backref        lspRange
	Output         lspRange
	Seed           lspRange
}

type encodingTokens struct {
	Primary lspRange
	Extra   lspRange
}

// DisassembleFile parses source and renders the stable, one-operation-per-line
// document consumed by the language server.
func DisassembleFile(source string, raw bool) ([]byte, error) {
	return DisassembleFileView(source, raw, "auto")
}

// DisassembleFileView renders auto, trace, or tar. Auto presents decoded
// content: tar archives as directories, text verbatim, and other payloads as a
// compact hex dump. Trace remains an explicit debugger view.
func DisassembleFileView(source string, raw bool, view string) ([]byte, error) {
	return DisassembleFileViewProgress(source, raw, view, nil)
}

// DisassembleFileViewProgress is DisassembleFileView with coarse progress
// events. Events intentionally describe stable phases rather than pretend that
// compressed and decoded byte offsets form a meaningful percentage.
func DisassembleFileViewProgress(source string, raw bool, view string, progress func(ViewProgress)) ([]byte, error) {
	abs, err := filepath.Abs(source)
	if err != nil {
		return nil, err
	}
	report := func(event ViewProgress) {
		if progress != nil {
			progress(event)
		}
	}
	var compressed int64
	if info, statErr := os.Stat(abs); statErr == nil {
		compressed = info.Size()
	}
	report(ViewProgress{Phase: "inspect", Detail: "inspecting stream", Compressed: compressed})
	if view == "auto" || view == "tar" {
		report(ViewProgress{Phase: "index", Detail: "checking for a tar filesystem", Compressed: compressed})
		entries, header, isTar, err := scanTarDirectory(abs, raw, "")
		if err != nil {
			return nil, err
		}
		if isTar {
			report(ViewProgress{Phase: "render", Detail: "rendering root directory", View: "tar", Compressed: compressed, Decoded: header.Payload, Entries: header.Count})
			meta, err := json.Marshal(metadata{Source: abs, Raw: raw, View: "tar", RFC: locateRFC1951(abs)})
			if err != nil {
				return nil, err
			}
			result := renderTarView(meta, entries, "", header.Count, header.Payload)
			report(ViewProgress{Phase: "ready", View: "tar", Compressed: compressed, Decoded: header.Payload, Entries: header.Count})
			return result, nil
		}
		if view == "tar" {
			return nil, fmt.Errorf("decoded output is not a tar archive")
		}
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return nil, err
	}
	report(ViewProgress{Phase: "inflate", Detail: "decoding content", Compressed: compressed})
	trace, err := parse(data, raw)
	if err != nil {
		return nil, err
	}
	selected := view
	if selected == "auto" {
		if isTextPayload(trace.Output) {
			selected = "text"
		} else {
			selected = "hex"
		}
	}
	report(ViewProgress{Phase: "render", Detail: "rendering " + selected, View: selected, Compressed: compressed, Decoded: uint64(len(trace.Output))})
	result, err := disassembleView(abs, raw, trace, view)
	if err == nil {
		report(ViewProgress{Phase: "ready", View: selected, Compressed: compressed, Decoded: uint64(len(trace.Output))})
	}
	return result, err
}

// Disassemble renders a trace as a gzpls source document. Content-first
// selection belongs to the file-level entry point, where the editor is opening
// the compressed payload rather than an already parsed debugger trace.
func Disassemble(source string, raw bool, trace *deflate.Trace) ([]byte, error) {
	return disassembleView(source, raw, trace, "trace")
}

func disassembleView(source string, raw bool, trace *deflate.Trace, requested string) ([]byte, error) {
	entries, isTar := parseTarView(trace.Output)
	view := requested
	if view == "auto" {
		if isTar {
			view = "tar"
		} else if isTextPayload(trace.Output) {
			return append([]byte(nil), trace.Output...), nil
		} else {
			return renderHexPayload(trace.Output), nil
		}
	}
	if view != "trace" && view != "tar" {
		return nil, fmt.Errorf("unknown view %q (want auto, trace, or tar)", requested)
	}
	if view == "tar" && !isTar {
		return nil, fmt.Errorf("decoded output is not a tar archive")
	}
	metaValue := metadata{Source: source, Raw: raw, View: view, RFC: locateRFC1951(source)}
	meta, err := json.Marshal(metaValue)
	if err != nil {
		return nil, err
	}
	if view == "tar" {
		rows := directoryTarEntries(entries, "")
		var payload uint64
		for _, entry := range entries {
			payload += uint64(max(entry.Size, 0))
		}
		return renderTarView(meta, rows, "", uint64(len(entries)), payload), nil
	}
	var out bytes.Buffer
	out.WriteString("# DEFLATE trace · gd: value → code → header → RFC 1951\n")
	fmt.Fprintf(&out, "# %d B out · %d blocks · %d ops\n\n", len(trace.Output), len(trace.Blocks), len(trace.Ops))
	graph := buildEncodingGraph(trace)
	commentColumn := traceCommentColumn(trace)
	for _, block := range trace.Blocks {
		writeTraceComment(&out,
			fmt.Sprintf("block %d bits[%s:%s]", block.ID, block.Input.Start, block.Input.End),
			fmt.Sprint(block.Kind), commentColumn)
		for _, op := range trace.Ops[block.FirstOp : block.FirstOp+block.NumOps] {
			switch op.Kind {
			case deflate.LiteralOp:
				writeTraceComment(&out, mustBitString(trace, op.SymbolBits), quoteByte(op.Literal), commentColumn)
			case deflate.StoredOp:
				fmt.Fprintf(&out, "stored %d bytes\n", op.Length)
			case deflate.MatchOp:
				writeTraceComment(&out, fmt.Sprintf("%s  %s",
					codedOperand(trace, op.SymbolBits, op.LengthExtraBits),
					codedOperand(trace, op.DistanceBits, op.DistanceExtraBits)),
					fmt.Sprintf("←%d ×%d", op.Distance, op.Length), commentColumn)
			}
		}
		out.WriteString("\n# definitions\n")
		for _, node := range graph.Nodes {
			if node.BlockID == block.ID && !node.isSymbol() {
				fmt.Fprintf(&out, "%s\n", node.Text)
			}
		}
		out.WriteByte('\n')
	}
	fmt.Fprintf(&out, "%s%s\n", headerPrefix, meta)
	return out.Bytes(), nil
}

func traceCommentColumn(trace *deflate.Trace) int {
	column := 0
	for _, block := range trace.Blocks {
		column = max(column, utf8.RuneCountInString(fmt.Sprintf("block %d bits[%s:%s]", block.ID, block.Input.Start, block.Input.End)))
		for _, op := range trace.Ops[block.FirstOp : block.FirstOp+block.NumOps] {
			var prefix string
			switch op.Kind {
			case deflate.LiteralOp:
				prefix = mustBitString(trace, op.SymbolBits)
			case deflate.MatchOp:
				prefix = fmt.Sprintf("%s  %s",
					codedOperand(trace, op.SymbolBits, op.LengthExtraBits),
					codedOperand(trace, op.DistanceBits, op.DistanceExtraBits))
			}
			column = max(column, utf8.RuneCountInString(prefix))
		}
	}
	return column + 2
}

func writeTraceComment(out *bytes.Buffer, prefix, comment string, column int) {
	out.WriteString(prefix)
	out.WriteString(strings.Repeat(" ", max(column-utf8.RuneCountInString(prefix), 2)))
	out.WriteString("# ")
	out.WriteString(comment)
	out.WriteByte('\n')
}

func openDocument(uri, text string) *document {
	doc := openDerivedDocument(uri, text, nil)
	if doc.loadErr == nil {
		return doc
	}
	if content := openCompressedContentDocument(uri, text); content != nil {
		return content
	}
	return doc
}

// openCompressedContentDocument reconstructs the invisible metadata for an
// auto-view buffer. The editor sees only the natural decoded text or hex dump;
// the compressed file URI is sufficient for the server to recover provenance.
func openCompressedContentDocument(uri, text string) *document {
	parsed, err := url.Parse(uri)
	if err != nil || parsed.Scheme != "file" {
		return nil
	}
	source := filepath.FromSlash(parsed.Path)
	data, err := os.ReadFile(source)
	if err != nil {
		return nil
	}
	trace, err := parse(data, false)
	if err != nil {
		return nil
	}
	view := "content"
	if string(trace.Output) != text {
		if string(renderHexPayload(trace.Output)) != text {
			return nil
		}
		view = "hex"
	}
	doc := &document{
		uri: uri, archiveURI: uri,
		meta:  metadata{Source: source, View: view, RFC: locateRFC1951(source)},
		trace: trace, revision: sha256.Sum256(data),
		lineOp: make(map[int]int), opLine: make(map[int]int), lineLen: make(map[int]int),
		tokens: make(map[int]opTokens), tar: make(map[int]tarEntry), encoding: make(map[string]encodingNode),
		encodingLine: make(map[int]string), encodingTok: make(map[string]encodingTokens),
		literalDefs: make(map[int]string), lengthDefs: make(map[int]string), distanceDefs: make(map[int]string),
		provenance: make(map[int][]int),
	}
	if view == "content" {
		name := strings.TrimSuffix(filepath.Base(source), filepath.Ext(source))
		doc.plain = &plaintextDocument{entry: tarEntry{
			Name: name, Kind: "file", Size: int64(len(trace.Output)),
			Data: deflate.OutRange{End: deflate.OutPos(len(trace.Output))},
		}}
		doc.setPlaintext(text)
	}
	return doc
}

func openDerivedDocument(uri, text string, source *document) *document {
	if strings.HasSuffix(uri, "/rfc1951.txt") && !strings.Contains(text, headerPrefix) {
		return openRFCDocument(uri, text)
	}
	doc := &document{
		uri: uri, lineOp: make(map[int]int), opLine: make(map[int]int),
		lineLen: make(map[int]int), tokens: make(map[int]opTokens), tar: make(map[int]tarEntry),
		encoding: make(map[string]encodingNode), encodingLine: make(map[int]string), encodingTok: make(map[string]encodingTokens), literalDefs: make(map[int]string),
		lengthDefs: make(map[int]string), distanceDefs: make(map[int]string),
		provenance: make(map[int][]int),
	}
	opText := make(map[int]string)
	var definitionLines []int
	var tarLines []int
	var provenanceLines []int
	isProvenance := strings.HasPrefix(text, "# provenance ")
	lineText := make(map[int]string)
	scanner := bufio.NewScanner(strings.NewReader(text))
	line := 0
	nextOp := 0
	inOps, inDefinitions, inTar := false, false, false
	foundMeta := false
	for scanner.Scan() {
		value := scanner.Text()
		lineText[line] = value
		doc.lineLen[line] = len(utf16.Encode([]rune(value)))
		if strings.HasPrefix(value, headerPrefix) {
			inTar = false
			if err := json.Unmarshal([]byte(strings.TrimPrefix(value, headerPrefix)), &doc.meta); err != nil {
				doc.loadErr = fmt.Errorf("invalid gzpls header: %w", err)
				return doc
			}
			foundMeta = true
		}
		if value == "/" {
			inTar = true
		} else if inTar && value != "" && !strings.HasPrefix(value, headerPrefix) {
			tarLines = append(tarLines, line)
		}
		if strings.HasPrefix(value, "block ") {
			inOps, inDefinitions = true, false
		} else if value == "# definitions" {
			inOps, inDefinitions = false, true
		} else if value != "" && inOps {
			doc.lineOp[line] = nextOp
			doc.opLine[nextOp] = line
			opText[nextOp] = value
			nextOp++
		} else if value != "" && inDefinitions && !strings.HasPrefix(value, "#") {
			definitionLines = append(definitionLines, line)
		}
		if isProvenance && value != "" && !strings.HasPrefix(value, "#") && !strings.HasPrefix(value, "block ") {
			provenanceLines = append(provenanceLines, line)
		}
		line++
	}
	if err := scanner.Err(); err != nil {
		doc.loadErr = err
		return doc
	}
	if !foundMeta {
		doc.loadErr = fmt.Errorf("not a gzpls disassembly: missing %q metadata", headerPrefix)
		return doc
	}
	if strings.HasSuffix(text, "\n") {
		doc.end = position{Line: line}
	} else if line > 0 {
		doc.end = position{Line: line - 1, Character: doc.lineLen[line-1]}
	}
	if source != nil {
		doc.revision = source.revision
		doc.trace = source.trace
		doc.archiveURI = source.archiveURI
	} else if doc.meta.View == "tar" {
		doc.archiveURI = uri
		var err error
		doc.revision, err = quickFileRevision(doc.meta.Source)
		if err != nil {
			doc.loadErr = err
			return doc
		}
	} else {
		data, err := os.ReadFile(doc.meta.Source)
		if err != nil {
			doc.loadErr = err
			return doc
		}
		doc.revision = sha256.Sum256(data)
		doc.trace, doc.loadErr = parse(data, doc.meta.Raw)
	}
	if doc.loadErr == nil && doc.meta.View == "trace" {
		if nextOp != len(doc.trace.Ops) {
			doc.loadErr = fmt.Errorf("trace listing has %d operation lines, want %d", nextOp, len(doc.trace.Ops))
			return doc
		}
		for id, value := range opText {
			doc.tokens[id] = opTokenRanges(value, doc.opLine[id], doc.trace.Ops[id])
		}
		graph := buildEncodingGraph(doc.trace)
		doc.literalDefs = graph.LiteralDefs
		doc.lengthDefs = graph.LengthDefs
		doc.distanceDefs = graph.DistanceDefs
		for _, node := range graph.Nodes {
			if node.isSymbol() {
				doc.encoding[node.Label] = node
			}
		}
		visibleNodes := make([]encodingNode, 0, len(graph.Nodes))
		for _, node := range graph.Nodes {
			if !node.isSymbol() {
				visibleNodes = append(visibleNodes, node)
			}
		}
		if len(definitionLines) != len(visibleNodes) {
			doc.loadErr = fmt.Errorf("trace listing has %d definition lines, want %d", len(definitionLines), len(visibleNodes))
			return doc
		}
		for i, node := range visibleNodes {
			node.Line = definitionLines[i]
			doc.encoding[node.Label] = node
			doc.encodingLine[node.Line] = node.Label
			doc.encodingTok[node.Label] = encodingTokenRanges(node.Text, node.Line)
		}
	}
	if doc.loadErr == nil && doc.meta.View == "symbols" {
		graph := buildEncodingGraph(doc.trace)
		for _, node := range graph.Nodes {
			if node.BlockID != doc.meta.Block || node.Alphabet != doc.meta.Alphabet {
				continue
			}
			node.Line = node.Symbol
			doc.encoding[node.Label] = node
			doc.encodingLine[node.Line] = node.Label
			doc.encodingTok[node.Label] = encodingTokenRanges(node.Text, node.Line)
		}
	}
	if doc.loadErr == nil && doc.meta.View == "tar" {
		entries, _, ok, err := scanTarDirectory(doc.meta.Source, doc.meta.Raw, doc.meta.Entry)
		if err != nil {
			doc.loadErr = err
			return doc
		}
		if !ok {
			doc.loadErr = fmt.Errorf("document requests tar view but decoded output is not tar")
			return doc
		}
		if len(entries) != len(tarLines) {
			doc.loadErr = fmt.Errorf("tar listing has %d entry lines, want %d", len(tarLines), len(entries))
			return doc
		}
		for i := range entries {
			entries[i].Line = tarLines[i]
			entries[i].NameToken, entries[i].LinkToken = tarTokenRanges(lineText[tarLines[i]], tarLines[i], entries[i])
			doc.tar[entries[i].Line] = entries[i]
		}
	}
	if doc.loadErr == nil && doc.meta.View == "provenance" {
		groups := provenanceGroups(doc.trace, deflate.OutRange{Start: deflate.OutPos(doc.meta.OutputStart), End: deflate.OutPos(doc.meta.OutputEnd)})
		if len(groups) != len(provenanceLines) {
			doc.loadErr = fmt.Errorf("provenance listing has %d group lines, want %d", len(provenanceLines), len(groups))
			return doc
		}
		for i, group := range groups {
			doc.provenance[provenanceLines[i]] = group.Ops
		}
	}
	return doc
}

func quickFileRevision(path string) ([sha256.Size]byte, error) {
	var revision [sha256.Size]byte
	f, err := os.Open(path)
	if err != nil {
		return revision, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return revision, err
	}
	h := sha256.New()
	var identity [16]byte
	binary.LittleEndian.PutUint64(identity[:8], uint64(info.Size()))
	binary.LittleEndian.PutUint64(identity[8:], uint64(info.ModTime().UnixNano()))
	_, _ = h.Write(identity[:])
	const sample = 64 << 10
	buf := make([]byte, sample)
	n, err := f.ReadAt(buf, 0)
	if err != nil && err != io.EOF {
		return revision, err
	}
	_, _ = h.Write(buf[:n])
	if info.Size() > sample {
		n, err = f.ReadAt(buf, max(0, info.Size()-sample))
		if err != nil && err != io.EOF {
			return revision, err
		}
		_, _ = h.Write(buf[:n])
	}
	copy(revision[:], h.Sum(nil))
	return revision, nil
}

func encodingTokenRanges(text string, line int) encodingTokens {
	if strings.HasPrefix(text, "# ") {
		start := len("# ")
		end := strings.IndexAny(text[start:], " \t")
		if end < 0 {
			end = len(text)
		} else {
			end += start
		}
		return encodingTokens{Primary: byteRange(text, line, start, end)}
	}
	end := strings.IndexAny(text, " \t")
	if end < 0 {
		end = len(text)
	}
	primary, extra := operandTokenRanges(text, line, 0, end)
	return encodingTokens{Primary: primary, Extra: extra}
}

func openRFCDocument(uri, text string) *document {
	doc := &document{uri: uri, lineLen: make(map[int]int)}
	doc.rfcLines = strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	for line, value := range doc.rfcLines {
		doc.lineLen[line] = len(utf16.Encode([]rune(value)))
	}
	if strings.HasSuffix(text, "\n") {
		doc.end = position{Line: len(doc.rfcLines)}
	} else if len(doc.rfcLines) != 0 {
		last := len(doc.rfcLines) - 1
		doc.end = position{Line: last, Character: doc.lineLen[last]}
	}
	return doc
}

func renderSymbolDocument(meta metadata, trace *deflate.Trace, block int, alphabet string) ([]byte, error) {
	graph := buildEncodingGraph(trace)
	maxSymbol := -1
	switch alphabet {
	case "litlen", "litlen-lengths":
		maxSymbol = 285
	case "distance":
		maxSymbol = 29
	case "distance-lengths":
		maxSymbol = 31
	case "codelen":
		maxSymbol = 18
	case "length-program":
		for _, node := range graph.Nodes {
			if node.BlockID == block && node.Alphabet == alphabet && node.Symbol > maxSymbol {
				maxSymbol = node.Symbol
			}
		}
	case "hclen":
		maxSymbol = 18
	default:
		return nil, fmt.Errorf("unknown Huffman alphabet %q", alphabet)
	}
	if maxSymbol < 0 {
		return nil, fmt.Errorf("empty %s table for block %d", alphabet, block)
	}
	lines := make([]string, maxSymbol+1)
	for _, node := range graph.Nodes {
		if node.BlockID == block && node.Alphabet == alphabet && node.Symbol <= maxSymbol {
			lines[node.Symbol] = node.Text
		}
	}
	var out bytes.Buffer
	for _, line := range lines {
		out.WriteString(line)
		out.WriteByte('\n')
	}
	encoded, err := json.Marshal(meta)
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(&out, "%s%s\n", headerPrefix, encoded)
	return out.Bytes(), nil
}

func codedOperand(trace *deflate.Trace, symbol, extra deflate.BitRange) string {
	value := mustBitString(trace, symbol)
	if extra.Len() != 0 {
		value += "·" + mustBitString(trace, extra)
	}
	return value
}

func opTokenRanges(line string, lineNumber int, op deflate.Op) opTokens {
	comment := strings.Index(line, "  # ")
	if comment < 0 {
		comment = len(line)
	}
	switch op.Kind {
	case deflate.LiteralOp:
		bits := byteRange(line, lineNumber, 0, comment)
		literal := byteRange(line, lineNumber, min(comment+4, len(line)), len(line))
		return opTokens{
			Bits:    bits,
			Literal: literal,
			Output:  literal,
		}
	case deflate.MatchOp:
		separator := strings.Index(line[:comment], "  ")
		if separator < 0 {
			return opTokens{}
		}
		distanceStart := separator + 2
		backrefStart := min(comment+4, len(line))
		outputStart := strings.LastIndex(line, "×")
		if outputStart < backrefStart {
			outputStart = len(line)
		}
		backref := byteRange(line, lineNumber, backrefStart, outputStart)
		output := byteRange(line, lineNumber, outputStart, len(line))
		lengthSymbol, lengthExtra := operandTokenRanges(line, lineNumber, 0, separator)
		distanceSymbol, distanceExtra := operandTokenRanges(line, lineNumber, distanceStart, comment)
		return opTokens{
			Length:         byteRange(line, lineNumber, 0, separator),
			LengthSymbol:   lengthSymbol,
			LengthExtra:    lengthExtra,
			Distance:       byteRange(line, lineNumber, distanceStart, comment),
			DistanceSymbol: distanceSymbol,
			DistanceExtra:  distanceExtra,
			Backref:        backref,
			Output:         output,
			Seed:           backref,
		}
	case deflate.StoredOp:
		whole := byteRange(line, lineNumber, 0, len(line))
		return opTokens{Output: whole}
	default:
		return opTokens{}
	}
}

func operandTokenRanges(line string, lineNumber, start, end int) (lspRange, lspRange) {
	dot := strings.Index(line[start:end], "·")
	if dot < 0 {
		return byteRange(line, lineNumber, start, end), lspRange{}
	}
	dot += start
	dotEnd := dot + len("·")
	return byteRange(line, lineNumber, start, dot), byteRange(line, lineNumber, dotEnd, end)
}

func byteRange(line string, lineNumber, start, end int) lspRange {
	startCharacter := len(utf16.Encode([]rune(line[:start])))
	endCharacter := len(utf16.Encode([]rune(line[:end])))
	return lspRange{
		Start: position{Line: lineNumber, Character: startCharacter},
		End:   position{Line: lineNumber, Character: endCharacter},
	}
}

func parse(data []byte, raw bool) (*deflate.Trace, error) {
	if !raw && len(data) >= 2 && data[0] == 0x1f && data[1] == 0x8b {
		return deflate.ParseGzip(data)
	}
	return deflate.ParseRaw(data)
}

func (s *Server) ensureTrace(doc *document) error {
	if doc.trace != nil {
		return nil
	}
	data, err := os.ReadFile(doc.meta.Source)
	if err != nil {
		return err
	}
	trace, err := parse(data, doc.meta.Raw)
	if err != nil {
		return err
	}
	for _, candidate := range s.docs {
		if candidate.meta.Source == doc.meta.Source && candidate.revision == doc.revision {
			candidate.trace = trace
		}
	}
	doc.trace = trace
	return err
}

func quoteByte(b byte) string {
	if unicode.IsPrint(rune(b)) {
		return strconv.QuoteRune(rune(b))
	}
	return strconv.QuoteRuneToASCII(rune(b))
}
