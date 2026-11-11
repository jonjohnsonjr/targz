// Package lsp exposes a deliberately straight-faced language server for
// DEFLATE instruction listings.
package lsp

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf16"

	"github.com/jonjohnsonjr/targz/deflate"
)

const headerPrefix = "# gzpls "

type metadata struct {
	Source   string `json:"source"`
	Raw      bool   `json:"raw"`
	View     string `json:"view,omitempty"`
	RFC      string `json:"rfc,omitempty"`
	TraceURI string `json:"traceUri,omitempty"`
	Block    int    `json:"block,omitempty"`
	Alphabet string `json:"alphabet,omitempty"`
}

type document struct {
	uri          string
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
	loadErr      error
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

// DisassembleFileView renders auto, trace, or tar. Auto selects tar only when
// the complete decoded output passes archive/tar checksum validation.
func DisassembleFileView(source string, raw bool, view string) ([]byte, error) {
	abs, err := filepath.Abs(source)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return nil, err
	}
	trace, err := parse(data, raw)
	if err != nil {
		return nil, err
	}
	return disassembleView(abs, raw, trace, view)
}

// Disassemble renders trace as a gzpls source document.
func Disassemble(source string, raw bool, trace *deflate.Trace) ([]byte, error) {
	return disassembleView(source, raw, trace, "auto")
}

func disassembleView(source string, raw bool, trace *deflate.Trace, requested string) ([]byte, error) {
	entries, isTar := parseTarView(trace.Output)
	view := requested
	if view == "auto" {
		if isTar {
			view = "tar"
		} else {
			view = "trace"
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
		return renderTarView(meta, trace, entries), nil
	}
	var out bytes.Buffer
	out.WriteString("# DEFLATE trace · gd: value → code → header → RFC 1951\n")
	fmt.Fprintf(&out, "# %d B out · %d blocks · %d ops\n\n", len(trace.Output), len(trace.Blocks), len(trace.Ops))
	graph := buildEncodingGraph(trace)
	for _, block := range trace.Blocks {
		fmt.Fprintf(&out, "block %d bits[%s:%s] # %s\n", block.ID, block.Input.Start, block.Input.End, block.Kind)
		for _, op := range trace.Ops[block.FirstOp : block.FirstOp+block.NumOps] {
			switch op.Kind {
			case deflate.LiteralOp:
				fmt.Fprintf(&out, "%s  # %s\n", mustBitString(trace, op.SymbolBits), quoteByte(op.Literal))
			case deflate.StoredOp:
				fmt.Fprintf(&out, "stored %d bytes\n", op.Length)
			case deflate.MatchOp:
				fmt.Fprintf(&out, "%s  %s  # ←%d ×%d\n",
					codedOperand(trace, op.SymbolBits, op.LengthExtraBits),
					codedOperand(trace, op.DistanceBits, op.DistanceExtraBits), op.Distance, op.Length)
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

func openDocument(uri, text string) *document {
	if strings.HasSuffix(uri, "/rfc1951.txt") && !strings.Contains(text, headerPrefix) {
		return openRFCDocument(uri, text)
	}
	doc := &document{
		uri: uri, lineOp: make(map[int]int), opLine: make(map[int]int),
		lineLen: make(map[int]int), tokens: make(map[int]opTokens), tar: make(map[int]tarEntry),
		encoding: make(map[string]encodingNode), encodingLine: make(map[int]string), encodingTok: make(map[string]encodingTokens), literalDefs: make(map[int]string),
		lengthDefs: make(map[int]string), distanceDefs: make(map[int]string),
	}
	opText := make(map[int]string)
	var definitionLines []int
	scanner := bufio.NewScanner(strings.NewReader(text))
	line := 0
	nextOp := 0
	inOps, inDefinitions := false, false
	foundMeta := false
	for scanner.Scan() {
		value := scanner.Text()
		doc.lineLen[line] = len(utf16.Encode([]rune(value)))
		if strings.HasPrefix(value, headerPrefix) {
			if err := json.Unmarshal([]byte(strings.TrimPrefix(value, headerPrefix)), &doc.meta); err != nil {
				doc.loadErr = fmt.Errorf("invalid gzpls header: %w", err)
				return doc
			}
			foundMeta = true
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
	data, err := os.ReadFile(doc.meta.Source)
	if err != nil {
		doc.loadErr = err
		return doc
	}
	doc.revision = sha256.Sum256(data)
	doc.trace, doc.loadErr = parse(data, doc.meta.Raw)
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
		entries, ok := parseTarView(doc.trace.Output)
		if !ok {
			doc.loadErr = fmt.Errorf("document requests tar view but decoded output is not tar")
			return doc
		}
		for i := range entries {
			entries[i].Line = tarEntryFirstLine + i
			doc.tar[entries[i].Line] = entries[i]
		}
	}
	return doc
}

func encodingTokenRanges(text string, line int) encodingTokens {
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
		return opTokens{
			Bits:    bits,
			Literal: byteRange(line, lineNumber, min(comment+4, len(line)), len(line)),
			Output:  bits,
		}
	case deflate.MatchOp:
		separator := strings.Index(line[:comment], "  ")
		if separator < 0 {
			return opTokens{}
		}
		distanceStart := separator + 2
		backref := byteRange(line, lineNumber, min(comment+4, len(line)), len(line))
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
			Output:         backref,
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

func quoteByte(b byte) string {
	if unicode.IsPrint(rune(b)) {
		return strconv.QuoteRune(rune(b))
	}
	return strconv.QuoteRuneToASCII(rune(b))
}
