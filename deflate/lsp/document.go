// Package lsp exposes a deliberately straight-faced language server for
// DEFLATE instruction listings.
package lsp

import (
	"bufio"
	"bytes"
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
	Source string `json:"source"`
	Raw    bool   `json:"raw"`
	View   string `json:"view,omitempty"`
	RFC    string `json:"rfc,omitempty"`
}

type document struct {
	uri          string
	meta         metadata
	trace        *deflate.Trace
	end          position
	lineOp       map[int]int
	opLine       map[int]int
	lineLen      map[int]int
	tokens       map[int]opTokens
	tar          map[int]tarEntry
	encoding     map[string]encodingNode
	encodingLine map[int]string
	literalDefs  map[int]string
	loadErr      error
}

type opTokens struct {
	Output  lspRange
	Seed    lspRange
	Literal lspRange
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
	fmt.Fprintf(&out, "%s%s\n", headerPrefix, meta)
	fmt.Fprintf(&out, "# %d output bytes, %d blocks, %d operations\n\n", len(trace.Output), len(trace.Blocks), len(trace.Ops))
	graph := buildEncodingGraph(trace)
	for _, block := range trace.Blocks {
		fmt.Fprintf(&out, "# block %d · %s Huffman · bits[%s:%s]\n", block.ID, block.Kind, block.Input.Start, block.Input.End)
		for _, node := range graph.Nodes {
			if node.BlockID == block.ID {
				fmt.Fprintf(&out, "%-18s %s\n", node.Label, node.Text)
			}
		}
		if len(graph.Nodes) != 0 {
			out.WriteByte('\n')
		}
		for _, op := range trace.Ops[block.FirstOp : block.FirstOp+block.NumOps] {
			switch op.Kind {
			case deflate.LiteralOp:
				fmt.Fprintf(&out, "%%%d  literal %-6s out[%d:%d] bits[%s:%s]\n",
					op.ID, quoteByte(op.Literal), op.Output.Start, op.Output.End, op.Input.Start, op.Input.End)
			case deflate.StoredOp:
				fmt.Fprintf(&out, "%%%d  stored  len=%d out[%d:%d] bits[%s:%s]\n",
					op.ID, op.Length, op.Output.Start, op.Output.End, op.Input.Start, op.Input.End)
			case deflate.MatchOp:
				seed, _ := op.Seed()
				fmt.Fprintf(&out, "%%%d  match   len=%d dist=%d out[%d:%d] seed[%d:%d] bits[%s:%s]\n",
					op.ID, op.Length, op.Distance, op.Output.Start, op.Output.End,
					seed.Start, seed.End, op.Input.Start, op.Input.End)
			}
		}
		out.WriteByte('\n')
	}
	return out.Bytes(), nil
}

func openDocument(uri, text string) *document {
	doc := &document{
		uri: uri, lineOp: make(map[int]int), opLine: make(map[int]int),
		lineLen: make(map[int]int), tokens: make(map[int]opTokens), tar: make(map[int]tarEntry),
		encoding: make(map[string]encodingNode), encodingLine: make(map[int]string), literalDefs: make(map[int]string),
	}
	encodedLines := make(map[string]int)
	scanner := bufio.NewScanner(strings.NewReader(text))
	line := 0
	for scanner.Scan() {
		value := scanner.Text()
		doc.lineLen[line] = len(utf16.Encode([]rune(value)))
		if line == 0 {
			if !strings.HasPrefix(value, headerPrefix) {
				doc.loadErr = fmt.Errorf("not a gzpls disassembly: first line must start with %q", headerPrefix)
				return doc
			}
			if err := json.Unmarshal([]byte(strings.TrimPrefix(value, headerPrefix)), &doc.meta); err != nil {
				doc.loadErr = fmt.Errorf("invalid gzpls header: %w", err)
				return doc
			}
		}
		if strings.HasPrefix(value, "%") {
			end := strings.IndexAny(value, " \t")
			if end < 0 {
				end = len(value)
			}
			id, err := strconv.Atoi(value[1:end])
			if err == nil {
				doc.lineOp[line] = id
				doc.opLine[id] = line
				doc.tokens[id] = opTokenRanges(value, line)
			}
		}
		if label := encodingLabel(value); label != "" {
			encodedLines[label] = line
		}
		line++
	}
	if err := scanner.Err(); err != nil {
		doc.loadErr = err
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
	doc.trace, doc.loadErr = parse(data, doc.meta.Raw)
	if doc.loadErr == nil && doc.meta.View == "trace" {
		graph := buildEncodingGraph(doc.trace)
		doc.literalDefs = graph.LiteralDefs
		for _, node := range graph.Nodes {
			if line, ok := encodedLines[node.Label]; ok {
				node.Line = line
				doc.encoding[node.Label] = node
				doc.encodingLine[line] = node.Label
			}
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

func opTokenRanges(line string, lineNumber int) opTokens {
	return opTokens{
		Output:  tokenRange(line, lineNumber, "out["),
		Seed:    tokenRange(line, lineNumber, "seed["),
		Literal: literalTokenRange(line, lineNumber),
	}
}

func literalTokenRange(line string, lineNumber int) lspRange {
	const prefix = "literal "
	start := strings.Index(line, prefix)
	if start < 0 {
		return lspRange{}
	}
	start += len(prefix)
	end := strings.Index(line[start:], " out[")
	if end < 0 {
		return lspRange{}
	}
	end += start
	for end > start && line[end-1] == ' ' {
		end--
	}
	return byteRange(line, lineNumber, start, end)
}

func tokenRange(line string, lineNumber int, prefix string) lspRange {
	start := strings.Index(line, prefix)
	if start < 0 {
		return lspRange{}
	}
	end := strings.IndexByte(line[start:], ']')
	if end < 0 {
		return lspRange{}
	}
	end += start + 1
	// LSP character offsets count UTF-16 code units rather than bytes.
	return byteRange(line, lineNumber, start, end)
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
