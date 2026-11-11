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
}

type document struct {
	uri     string
	meta    metadata
	trace   *deflate.Trace
	lineOp  map[int]int
	opLine  map[int]int
	lineLen map[int]int
	loadErr error
}

// DisassembleFile parses source and renders the stable, one-operation-per-line
// document consumed by the language server.
func DisassembleFile(source string, raw bool) ([]byte, error) {
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
	return Disassemble(abs, raw, trace)
}

// Disassemble renders trace as a gzpls source document.
func Disassemble(source string, raw bool, trace *deflate.Trace) ([]byte, error) {
	meta, err := json.Marshal(metadata{Source: source, Raw: raw})
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	fmt.Fprintf(&out, "%s%s\n", headerPrefix, meta)
	fmt.Fprintf(&out, "# %d output bytes, %d blocks, %d operations\n\n", len(trace.Output), len(trace.Blocks), len(trace.Ops))
	for _, op := range trace.Ops {
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
	return out.Bytes(), nil
}

func openDocument(uri, text string) *document {
	doc := &document{uri: uri, lineOp: make(map[int]int), opLine: make(map[int]int), lineLen: make(map[int]int)}
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
			}
		}
		line++
	}
	if err := scanner.Err(); err != nil {
		doc.loadErr = err
		return doc
	}
	data, err := os.ReadFile(doc.meta.Source)
	if err != nil {
		doc.loadErr = err
		return doc
	}
	doc.trace, doc.loadErr = parse(data, doc.meta.Raw)
	return doc
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
