package lsp

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"

	"github.com/jonjohnsonjr/targz/deflate"
)

type encodingNode struct {
	Label     string
	Name      string
	Text      string
	Parent    string
	Alphabet  string
	Symbol    int
	BlockID   int
	RFCAnchor string
	Line      int
}

type encodingGraph struct {
	Nodes        []encodingNode
	LiteralDefs  map[int]string
	LengthDefs   map[int]string
	DistanceDefs map[int]string
}

func buildEncodingGraph(trace *deflate.Trace) encodingGraph {
	graph := encodingGraph{
		LiteralDefs: make(map[int]string), LengthDefs: make(map[int]string), DistanceDefs: make(map[int]string),
	}
	type symbols struct {
		litlen   map[int]struct{}
		distance map[int]struct{}
	}
	used := make(map[int]*symbols)
	for _, op := range trace.Ops {
		if used[op.BlockID] == nil {
			used[op.BlockID] = &symbols{litlen: make(map[int]struct{}), distance: make(map[int]struct{})}
		}
		switch op.Kind {
		case deflate.LiteralOp:
			used[op.BlockID].litlen[int(op.Literal)] = struct{}{}
			graph.LiteralDefs[op.ID] = litlenLabel(op.BlockID, int(op.Literal))
		case deflate.MatchOp:
			lengthSymbol := int(op.LengthSymbol)
			distanceSymbol := int(op.DistanceSymbol)
			used[op.BlockID].litlen[lengthSymbol] = struct{}{}
			used[op.BlockID].distance[distanceSymbol] = struct{}{}
			graph.LengthDefs[op.ID] = litlenLabel(op.BlockID, lengthSymbol)
			graph.DistanceDefs[op.ID] = distanceLabel(op.BlockID, distanceSymbol)
		}
	}

	for _, block := range trace.Blocks {
		graph.Nodes = append(graph.Nodes, blockHeaderNodes(trace, block)...)
		symbolSet := used[block.ID]
		if symbolSet == nil {
			continue
		}
		// A symbol-table document is an address space, not a usage report. Keep
		// every code present in the block's tables even when this stream never
		// happens to decode that symbol.
		if block.LiteralTable != nil {
			for _, code := range block.LiteralTable.Codes {
				symbolSet.litlen[code.Symbol] = struct{}{}
			}
		}
		if block.DistanceTable != nil {
			for _, code := range block.DistanceTable.Codes {
				symbolSet.distance[code.Symbol] = struct{}{}
			}
		}
		for _, symbol := range sortedKeys(symbolSet.litlen) {
			code, ok := huffmanCode(block.LiteralTable, symbol)
			if !ok {
				continue
			}
			node := encodingNode{
				Label: litlenLabel(block.ID, symbol), BlockID: block.ID,
				Name:     fmt.Sprintf("literal/length symbol %d", symbol),
				Alphabet: "litlen", Symbol: symbol,
				Text: fmt.Sprintf("%-15s # %s", codeString(code), litlenMeaning(symbol)),
			}
			if block.Kind == deflate.FixedBlock {
				node.RFCAnchor = fixedLiteralRFCAnchor(symbol)
			} else if block.Kind == deflate.DynamicBlock && block.Dynamic != nil {
				if run := lengthRunForSlot(block.Dynamic, symbol); run >= 0 {
					node.Parent = lengthSlotLabel(block.ID, "litlen", symbol)
				}
			}
			graph.Nodes = append(graph.Nodes, node)
		}
		for _, symbol := range sortedKeys(symbolSet.distance) {
			code, ok := huffmanCode(block.DistanceTable, symbol)
			if !ok {
				continue
			}
			node := encodingNode{
				Label: distanceLabel(block.ID, symbol), BlockID: block.ID,
				Name:     fmt.Sprintf("distance symbol %d", symbol),
				Alphabet: "distance", Symbol: symbol,
				Text: fmt.Sprintf("%-15s # %s", codeString(code), distanceMeaning(symbol)),
			}
			if block.Kind == deflate.FixedBlock {
				node.RFCAnchor = "fixed-distance"
			} else if block.Kind == deflate.DynamicBlock && block.Dynamic != nil {
				if run := lengthRunForSlot(block.Dynamic, block.Dynamic.NumLiteralLengths+symbol); run >= 0 {
					node.Parent = lengthSlotLabel(block.ID, "distance", symbol)
				}
			}
			graph.Nodes = append(graph.Nodes, node)
		}

		if block.Kind != deflate.DynamicBlock || block.Dynamic == nil {
			continue
		}
		header := block.Dynamic
		for slot := 0; slot < header.NumLiteralLengths+header.NumDistanceLengths; slot++ {
			runIndex := lengthRunForSlot(header, slot)
			if runIndex < 0 {
				continue
			}
			run := header.LengthRuns[runIndex]
			alphabet, symbol := "litlen-lengths", slot
			labelAlphabet := "litlen"
			if slot >= header.NumLiteralLengths {
				alphabet, symbol = "distance-lengths", slot-header.NumLiteralLengths
				labelAlphabet = "distance"
			}
			graph.Nodes = append(graph.Nodes, encodingNode{
				Label: lengthSlotLabel(block.ID, labelAlphabet, symbol), Parent: lengthRunLabel(block.ID, runIndex),
				Name:     fmt.Sprintf("%s code length %d", labelAlphabet, symbol),
				Alphabet: alphabet, Symbol: symbol, BlockID: block.ID,
				Text: lengthAssignmentText(run),
			})
		}
		for runIndex, run := range header.LengthRuns {
			graph.Nodes = append(graph.Nodes, encodingNode{
				Label: lengthRunLabel(block.ID, runIndex), Parent: codeLengthLabel(block.ID, run.Symbol),
				Name:     fmt.Sprintf("code-length instruction %d", runIndex),
				Alphabet: "length-program", Symbol: runIndex, BlockID: block.ID,
				Text: lengthRunText(trace, header, run),
			})
		}
		for _, code := range block.CodeTable.Codes {
			symbol := code.Symbol
			field := codeLengthField(header, symbol)
			if field < 0 {
				continue
			}
			graph.Nodes = append(graph.Nodes, encodingNode{
				Label: codeLengthLabel(block.ID, symbol), Parent: hclenLabel(block.ID, field), BlockID: block.ID,
				Name:     fmt.Sprintf("code-length symbol %d", symbol),
				Alphabet: "codelen", Symbol: symbol,
				Text: fmt.Sprintf("%-15s # length %d", codeString(code), symbol),
			})
		}
		for fieldIndex, field := range header.CodeLengths {
			wire, _ := trace.BitString(field.Input)
			graph.Nodes = append(graph.Nodes, encodingNode{
				Label: hclenLabel(block.ID, fieldIndex), BlockID: block.ID,
				Name:     fmt.Sprintf("HCLEN transmission field %d", fieldIndex),
				Alphabet: "hclen", Symbol: fieldIndex,
				Text:      fmt.Sprintf("%-15s # symbol %d: %d bits", wire, field.Symbol, field.Length),
				RFCAnchor: hclenRFCAnchor(fieldIndex),
			})
		}
	}
	return graph
}

func blockHeaderNodes(trace *deflate.Trace, block deflate.Block) []encodingNode {
	start := block.Input.Start
	nodes := []encodingNode{
		{
			Label: headerLabel(block.ID, "bfinal"), Name: "BFINAL", BlockID: block.ID,
			Text:      fmt.Sprintf("%-15s # BFINAL %t", mustBitString(trace, deflate.BitRange{Start: start, End: start + 1}), block.Final),
			RFCAnchor: "term-bfinal",
		},
		{
			Label: headerLabel(block.ID, "btype"), Name: "BTYPE", BlockID: block.ID,
			Text:      fmt.Sprintf("%-15s # BTYPE %s", mustBitString(trace, deflate.BitRange{Start: start + 1, End: start + 3}), block.Kind),
			RFCAnchor: "term-btype",
		},
	}
	if block.Dynamic != nil {
		header := block.Dynamic
		nodes = append(nodes,
			encodingNode{
				Label: headerLabel(block.ID, "hlit"), Name: "HLIT", BlockID: block.ID,
				Text:      fmt.Sprintf("%-15s # HLIT %d", mustBitString(trace, header.HLIT), header.NumLiteralLengths),
				RFCAnchor: "term-hlit",
			},
			encodingNode{
				Label: headerLabel(block.ID, "hdist"), Name: "HDIST", BlockID: block.ID,
				Text:      fmt.Sprintf("%-15s # HDIST %d", mustBitString(trace, header.HDIST), header.NumDistanceLengths),
				RFCAnchor: "term-hdist",
			},
			encodingNode{
				Label: headerLabel(block.ID, "hclen"), Name: "HCLEN", BlockID: block.ID,
				Text:      fmt.Sprintf("%-15s # HCLEN %d", mustBitString(trace, header.HCLEN), header.NumCodeLengths),
				RFCAnchor: "term-hclen",
			},
		)
	}
	if block.Kind == deflate.StoredBlock && block.Header.Len() >= 32 {
		lengthStart := block.Header.End - 32
		nodes = append(nodes,
			encodingNode{
				Label: headerLabel(block.ID, "len"), Name: "LEN", BlockID: block.ID,
				Text:      fmt.Sprintf("%-15s # LEN %d", mustBitString(trace, deflate.BitRange{Start: lengthStart, End: lengthStart + 16}), block.Output.Len()),
				RFCAnchor: "term-len",
			},
			encodingNode{
				Label: headerLabel(block.ID, "nlen"), Name: "NLEN", BlockID: block.ID,
				Text:      fmt.Sprintf("%-15s # NLEN complement", mustBitString(trace, deflate.BitRange{Start: lengthStart + 16, End: lengthStart + 32})),
				RFCAnchor: "term-nlen",
			},
		)
	}
	return nodes
}

func (n encodingNode) isSymbol() bool { return n.Alphabet != "" }

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

func lengthAssignmentText(run deflate.CodeLengthRun) string {
	return fmt.Sprintf("length %d", run.Length)
}

func lengthRunText(trace *deflate.Trace, header *deflate.DynamicHeader, run deflate.CodeLengthRun) string {
	wire := mustBitString(trace, run.SymbolBits)
	if run.ExtraBits.Len() != 0 {
		wire += "·" + mustBitString(trace, run.ExtraBits)
	}
	unit := "bits"
	if run.Length == 1 {
		unit = "bit"
	}
	return fmt.Sprintf("%-15s # %d %s → %s", wire, run.Length, unit,
		slotRange(header, run.SlotsStart, run.SlotsEnd))
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
		return fmt.Sprintf("litlen[%d:%d] + distance[0:%d]", start, nlit, end-nlit)
	}
}

func litlenLabel(block, symbol int) string   { return fmt.Sprintf("@b%d.litlen.%d", block, symbol) }
func distanceLabel(block, symbol int) string { return fmt.Sprintf("@b%d.distance.%d", block, symbol) }
func headerLabel(block int, field string) string {
	return fmt.Sprintf("@b%d.header.%s", block, field)
}
func lengthSlotLabel(block int, alphabet string, symbol int) string {
	return fmt.Sprintf("@b%d.%s.lengths.%d", block, alphabet, symbol)
}
func lengthRunLabel(block, run int) string     { return fmt.Sprintf("@b%d.length-program.%d", block, run) }
func codeLengthLabel(block, symbol int) string { return fmt.Sprintf("@b%d.codelen.%d", block, symbol) }
func hclenLabel(block, field int) string       { return fmt.Sprintf("@b%d.hclen.%d", block, field) }

var lengthBases = [...]int{
	3, 4, 5, 6, 7, 8, 9, 10, 11, 13, 15, 17, 19, 23, 27, 31,
	35, 43, 51, 59, 67, 83, 99, 115, 131, 163, 195, 227, 258,
}

var lengthExtras = [...]uint8{
	0, 0, 0, 0, 0, 0, 0, 0, 1, 1, 1, 1, 2, 2, 2, 2,
	3, 3, 3, 3, 4, 4, 4, 4, 5, 5, 5, 5, 0,
}

var distanceBases = [...]int{
	1, 2, 3, 4, 5, 7, 9, 13, 17, 25, 33, 49, 65, 97, 129, 193,
	257, 385, 513, 769, 1025, 1537, 2049, 3073, 4097, 6145, 8193, 12289, 16385, 24577,
}

var distanceExtras = [...]uint8{
	0, 0, 0, 0, 1, 1, 2, 2, 3, 3, 4, 4, 5, 5, 6, 6,
	7, 7, 8, 8, 9, 9, 10, 10, 11, 11, 12, 12, 13, 13,
}

func litlenMeaning(symbol int) string {
	switch {
	case symbol < 256:
		return quoteByte(byte(symbol))
	case symbol == 256:
		return "end"
	case symbol <= 285:
		return valueRange("length", lengthBases[symbol-257], lengthExtras[symbol-257])
	default:
		return fmt.Sprintf("reserved %d", symbol)
	}
}

func distanceMeaning(symbol int) string {
	if symbol < 0 || symbol >= len(distanceBases) {
		return fmt.Sprintf("reserved %d", symbol)
	}
	return valueRange("distance", distanceBases[symbol], distanceExtras[symbol])
}

func valueRange(kind string, base int, extra uint8) string {
	if extra == 0 {
		return fmt.Sprintf("%s %d", kind, base)
	}
	return fmt.Sprintf("%s %d-%d", kind, base, base+(1<<extra)-1)
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
