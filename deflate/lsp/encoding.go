package lsp

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jonjohnsonjr/targz/deflate"
)

type encodingNode struct {
	Label     string
	Text      string
	Parent    string
	BlockID   int
	RFCStart  int
	RFCEnd    int
	RFCSelect int
	Line      int
}

type encodingGraph struct {
	Nodes       []encodingNode
	LiteralDefs map[int]string
}

func buildEncodingGraph(trace *deflate.Trace) encodingGraph {
	graph := encodingGraph{LiteralDefs: make(map[int]string)}
	used := make(map[int]map[int]struct{})
	for _, op := range trace.Ops {
		if op.Kind != deflate.LiteralOp {
			continue
		}
		if used[op.BlockID] == nil {
			used[op.BlockID] = make(map[int]struct{})
		}
		used[op.BlockID][int(op.Literal)] = struct{}{}
		graph.LiteralDefs[op.ID] = litlenLabel(op.BlockID, int(op.Literal))
	}

	for _, block := range trace.Blocks {
		symbolSet := used[block.ID]
		if len(symbolSet) == 0 || block.LiteralTable == nil {
			continue
		}
		symbols := sortedKeys(symbolSet)
		neededRuns := make(map[int]struct{})
		for _, symbol := range symbols {
			code, ok := huffmanCode(block.LiteralTable, symbol)
			if !ok {
				continue
			}
			node := encodingNode{
				Label: litlenLabel(block.ID, symbol), BlockID: block.ID,
				Text: fmt.Sprintf("%-15s # %s", codeString(code), quoteByte(byte(symbol))),
			}
			if block.Kind == deflate.FixedBlock {
				node.RFCStart, node.RFCEnd, node.RFCSelect = 654, 670, fixedRFCLine(symbol)
			} else if block.Kind == deflate.DynamicBlock && block.Dynamic != nil {
				if run := lengthRunForSlot(block.Dynamic, symbol); run >= 0 {
					node.Parent = lengthRunLabel(block.ID, run)
					neededRuns[run] = struct{}{}
				}
			}
			graph.Nodes = append(graph.Nodes, node)
		}

		if block.Kind != deflate.DynamicBlock || block.Dynamic == nil {
			continue
		}
		header := block.Dynamic
		neededCodeSymbols := make(map[int]struct{})
		for _, runIndex := range sortedKeys(neededRuns) {
			run := header.LengthRuns[runIndex]
			neededCodeSymbols[run.Symbol] = struct{}{}
			graph.Nodes = append(graph.Nodes, encodingNode{
				Label: lengthRunLabel(block.ID, runIndex), Parent: codeLengthLabel(block.ID, run.Symbol), BlockID: block.ID,
				Text: fmt.Sprintf("%-15s # %s = %d",
					mustBitString(trace, run.Input), slotRange(header, run.SlotsStart, run.SlotsEnd), run.Length),
			})
		}
		neededFields := make(map[int]struct{})
		for _, symbol := range sortedKeys(neededCodeSymbols) {
			code, ok := huffmanCode(block.CodeTable, symbol)
			if !ok {
				continue
			}
			field := codeLengthField(header, symbol)
			if field < 0 {
				continue
			}
			neededFields[field] = struct{}{}
			graph.Nodes = append(graph.Nodes, encodingNode{
				Label: codeLengthLabel(block.ID, symbol), Parent: hclenLabel(block.ID, field), BlockID: block.ID,
				Text: fmt.Sprintf("%-15s # length %d", codeString(code), symbol),
			})
		}
		for _, fieldIndex := range sortedKeys(neededFields) {
			field := header.CodeLengths[fieldIndex]
			wire, _ := trace.BitString(field.Input)
			graph.Nodes = append(graph.Nodes, encodingNode{
				Label: hclenLabel(block.ID, fieldIndex), BlockID: block.ID,
				Text: fmt.Sprintf("%-15s # symbol %d: %d bits (HCLEN[%d])",
					wire, field.Symbol, field.Length, fieldIndex),
				RFCStart: 689, RFCEnd: 750, RFCSelect: 734,
			})
		}
	}
	return graph
}

func mustBitString(trace *deflate.Trace, span deflate.BitRange) string {
	wire, _ := trace.BitString(span)
	return wire
}

func sortedKeys[V any](values map[int]V) []int {
	keys := make([]int, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Ints(keys)
	return keys
}

func huffmanCode(table *deflate.HuffmanTable, symbol int) (deflate.HuffmanCode, bool) {
	if table != nil {
		for _, code := range table.Codes {
			if code.Symbol == symbol {
				return code, true
			}
		}
	}
	return deflate.HuffmanCode{}, false
}

func codeString(code deflate.HuffmanCode) string {
	return fmt.Sprintf("%0*b", int(code.Bits), code.Code)
}

func lengthRunForSlot(header *deflate.DynamicHeader, slot int) int {
	for i, run := range header.LengthRuns {
		if run.SlotsStart <= slot && slot < run.SlotsEnd {
			return i
		}
	}
	return -1
}

func codeLengthField(header *deflate.DynamicHeader, symbol int) int {
	for i, field := range header.CodeLengths {
		if field.Symbol == symbol {
			return i
		}
	}
	return -1
}

func slotRange(header *deflate.DynamicHeader, start, end int) string {
	nlit := header.NumLiteralLengths
	switch {
	case end == start+1 && end <= nlit:
		return fmt.Sprintf("litlen[%d]", start)
	case end == start+1 && start >= nlit:
		return fmt.Sprintf("distance[%d]", start-nlit)
	case end <= nlit:
		return fmt.Sprintf("litlen[%d:%d]", start, end)
	case start >= nlit:
		return fmt.Sprintf("distance[%d:%d]", start-nlit, end-nlit)
	default:
		return fmt.Sprintf("litlen[%d:%d]+distance[0:%d]", start, nlit, end-nlit)
	}
}

func litlenLabel(block, symbol int) string     { return fmt.Sprintf("@b%d.litlen.%d", block, symbol) }
func lengthRunLabel(block, run int) string     { return fmt.Sprintf("@b%d.lengths.%d", block, run) }
func codeLengthLabel(block, symbol int) string { return fmt.Sprintf("@b%d.codelen.%d", block, symbol) }
func hclenLabel(block, field int) string       { return fmt.Sprintf("@b%d.hclen.%d", block, field) }

func fixedRFCLine(symbol int) int {
	if symbol <= 143 {
		return 662
	}
	return 664
}

func locateRFC1951(source string) string {
	var starts []string
	if cwd, err := os.Getwd(); err == nil {
		starts = append(starts, cwd)
	}
	if source != "" {
		starts = append(starts, filepath.Dir(source))
	}
	if executable, err := os.Executable(); err == nil {
		starts = append(starts, filepath.Dir(executable))
	}
	seen := make(map[string]struct{})
	for _, start := range starts {
		dir, _ := filepath.Abs(start)
		for {
			if _, ok := seen[dir]; ok {
				break
			}
			seen[dir] = struct{}{}
			candidate := filepath.Join(dir, "doc", "rfc1951.txt")
			if info, statErr := os.Stat(candidate); statErr == nil && info.Mode().IsRegular() {
				absolute, _ := filepath.Abs(candidate)
				return absolute
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}
	return ""
}

func fileURI(path string) string {
	return (&url.URL{Scheme: "file", Path: filepath.ToSlash(path)}).String()
}

func encodingLabel(line string) string {
	if !strings.HasPrefix(line, "@b") {
		return ""
	}
	if end := strings.IndexAny(line, " \t"); end >= 0 {
		return line[:end]
	}
	return line
}
