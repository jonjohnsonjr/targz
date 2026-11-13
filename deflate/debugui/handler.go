// Package debugui provides a read-only HTTP interface for exploring a DEFLATE
// trace. It deliberately depends only on the trace model, not on parsing or
// filesystem concerns.
package debugui

import (
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/jonjohnsonjr/targz/deflate"
)

//go:embed workbench.html
var indexHTML []byte

// Handler returns a read-only debugger application for trace.
func Handler(trace *deflate.Trace) http.Handler {
	s := &server{trace: trace}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", s.index)
	mux.HandleFunc("GET /api/summary", s.summary)
	mux.HandleFunc("GET /api/ops", s.ops)
	mux.HandleFunc("GET /api/op", s.op)
	mux.HandleFunc("GET /api/producer", s.producer)
	mux.HandleFunc("GET /api/input-anchor", s.inputAnchor)
	mux.HandleFunc("GET /api/decode", s.decode)
	mux.HandleFunc("GET /api/block", s.block)
	mux.HandleFunc("GET /api/origin", s.origin)
	mux.HandleFunc("GET /api/uses", s.uses)
	mux.HandleFunc("GET /api/input", s.input)
	mux.HandleFunc("GET /api/output", s.output)
	return mux
}

type server struct {
	trace *deflate.Trace
}

type span struct {
	Start    string `json:"start"`
	End      string `json:"end"`
	StartBit uint64 `json:"startBit"`
	EndBit   uint64 `json:"endBit"`
	Bits     uint64 `json:"bits"`
}

type outSpan struct {
	Start uint64 `json:"start"`
	End   uint64 `json:"end"`
}

type blockView struct {
	ID     int     `json:"id"`
	Kind   string  `json:"kind"`
	Final  bool    `json:"final"`
	Input  span    `json:"input"`
	Header span    `json:"header"`
	Output outSpan `json:"output"`
	First  int     `json:"firstOp"`
	Count  int     `json:"numOps"`
}

type opView struct {
	ID      int     `json:"id"`
	BlockID int     `json:"blockId"`
	Kind    string  `json:"kind"`
	Input   span    `json:"input"`
	Output  outSpan `json:"output"`

	Literal   *byte    `json:"literal,omitempty"`
	Length    int      `json:"length,omitempty"`
	Distance  int      `json:"distance,omitempty"`
	Seed      *outSpan `json:"seed,omitempty"`
	InputBits string   `json:"inputBits,omitempty"`

	SymbolBits        span   `json:"symbolBits"`
	SymbolCode        string `json:"symbolCode,omitempty"`
	LengthExtraBits   span   `json:"lengthExtraBits"`
	LengthExtraCode   string `json:"lengthExtraCode,omitempty"`
	DistanceBits      span   `json:"distanceBits"`
	DistanceCode      string `json:"distanceCode,omitempty"`
	DistanceExtraBits span   `json:"distanceExtraBits"`
	DistanceExtraCode string `json:"distanceExtraCode,omitempty"`
}

type decodeStage struct {
	Key      string `json:"key"`
	Level    string `json:"level"`
	Bits     string `json:"bits,omitempty"`
	Input    *span  `json:"input,omitempty"`
	Value    string `json:"value"`
	Detail   string `json:"detail,omitempty"`
	Relation string `json:"relation,omitempty"`
}

type decodeBranch struct {
	Name   string        `json:"name"`
	Stages []decodeStage `json:"stages"`
}

type decodeView struct {
	OpID     int            `json:"opId"`
	BlockID  int            `json:"blockId"`
	Block    string         `json:"block"`
	Branches []decodeBranch `json:"branches"`
	Header   []decodeStage  `json:"header,omitempty"`
}

type blockDetailView struct {
	Block  blockView     `json:"block"`
	Fields []decodeStage `json:"fields"`
	Bits   string        `json:"bits"`
	Runs   int           `json:"runs,omitempty"`
	Layers []blockLayer  `json:"layers,omitempty"`
}

type blockLayer struct {
	ID      string        `json:"id"`
	Title   string        `json:"title"`
	Summary string        `json:"summary"`
	Items   []decodeStage `json:"items"`
}

func (s *server) index(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(indexHTML)
}

func (s *server) summary(w http.ResponseWriter, _ *http.Request) {
	blocks := make([]blockView, len(s.trace.Blocks))
	for i, block := range s.trace.Blocks {
		blocks[i] = viewBlock(block)
	}
	writeJSON(w, map[string]any{
		"outputSize":     len(s.trace.Output),
		"operationCount": len(s.trace.Ops),
		"consumedBits":   s.trace.ConsumedBits(),
		"consumedAt":     s.trace.ConsumedBits().String(),
		"memberCount":    len(s.trace.Members),
		"blocks":         blocks,
	})
}

func (s *server) ops(w http.ResponseWriter, r *http.Request) {
	start, err := queryInt(r, "start", 0)
	if err != nil {
		writeError(w, err)
		return
	}
	limit, err := queryInt(r, "limit", 200)
	if err != nil {
		writeError(w, err)
		return
	}
	if start < 0 {
		start = 0
	}
	if limit < 1 {
		limit = 1
	}
	if limit > 1000 {
		limit = 1000
	}
	if start > len(s.trace.Ops) {
		start = len(s.trace.Ops)
	}
	end := start + limit
	if end > len(s.trace.Ops) {
		end = len(s.trace.Ops)
	}
	views := make([]opView, 0, end-start)
	for _, op := range s.trace.Ops[start:end] {
		views = append(views, s.viewOp(op))
	}
	writeJSON(w, map[string]any{
		"start": start,
		"end":   end,
		"total": len(s.trace.Ops),
		"ops":   views,
	})
}

func (s *server) op(w http.ResponseWriter, r *http.Request) {
	id, err := queryInt(r, "id", -1)
	if err != nil || id < 0 || id >= len(s.trace.Ops) {
		if err == nil {
			err = fmt.Errorf("operation id %d is outside [0,%d)", id, len(s.trace.Ops))
		}
		writeError(w, err)
		return
	}
	writeJSON(w, s.viewOp(s.trace.Ops[id]))
}

func (s *server) producer(w http.ResponseWriter, r *http.Request) {
	offset, err := queryUint64(r, "offset", 0)
	if err != nil || offset >= uint64(len(s.trace.Output)) {
		if err == nil {
			err = fmt.Errorf("output offset %d is outside [0,%d)", offset, len(s.trace.Output))
		}
		writeError(w, err)
		return
	}
	op, ok := s.trace.Producer(deflate.OutPos(offset))
	if !ok {
		writeError(w, fmt.Errorf("no producer for output offset %d", offset))
		return
	}
	writeJSON(w, s.viewOp(op))
}

// inputAnchor returns the operation at, or nearest to, a compressed-input bit.
// Header and alignment gaps are associated with the closest payload operation,
// which is more useful for synchronized browsing than returning no mapping.
func (s *server) inputAnchor(w http.ResponseWriter, r *http.Request) {
	bit, err := queryUint64(r, "bit", 0)
	if err != nil || bit >= uint64(s.trace.ConsumedBits()) {
		if err == nil {
			err = fmt.Errorf("input bit %d is outside [0,%d)", bit, s.trace.ConsumedBits())
		}
		writeError(w, err)
		return
	}
	if len(s.trace.Ops) == 0 {
		writeError(w, fmt.Errorf("trace has no operations"))
		return
	}
	i := sort.Search(len(s.trace.Ops), func(i int) bool {
		return uint64(s.trace.Ops[i].Input.End) > bit
	})
	if i == len(s.trace.Ops) {
		i--
	} else if i > 0 && bit < uint64(s.trace.Ops[i].Input.Start) {
		previousEnd := uint64(s.trace.Ops[i-1].Input.End)
		nextStart := uint64(s.trace.Ops[i].Input.Start)
		if bit-previousEnd <= nextStart-bit {
			i--
		}
	}
	writeJSON(w, s.viewOp(s.trace.Ops[i]))
}

func (s *server) decode(w http.ResponseWriter, r *http.Request) {
	id, err := queryInt(r, "id", -1)
	if err != nil || id < 0 || id >= len(s.trace.Ops) {
		if err == nil {
			err = fmt.Errorf("operation id %d is outside [0,%d)", id, len(s.trace.Ops))
		}
		writeError(w, err)
		return
	}
	op := s.trace.Ops[id]
	block := s.trace.Blocks[op.BlockID]
	view := decodeView{OpID: op.ID, BlockID: block.ID, Block: block.Kind.String()}
	switch op.Kind {
	case deflate.LiteralOp:
		view.Branches = append(view.Branches, s.decodeBranch(block, "literal/length", int(op.Literal), op.SymbolBits, deflate.BitRange{},
			fmt.Sprintf("literal %s", printableByte(op.Literal))))
	case deflate.MatchOp:
		view.Branches = append(view.Branches,
			s.decodeBranch(block, "literal/length", int(op.LengthSymbol), op.SymbolBits, op.LengthExtraBits, fmt.Sprintf("length %d", op.Length)),
			s.decodeBranch(block, "distance", int(op.DistanceSymbol), op.DistanceBits, op.DistanceExtraBits, fmt.Sprintf("distance %d", op.Distance)),
		)
	case deflate.StoredOp:
		bits, _ := s.trace.BitString(op.Input)
		input := viewSpan(op.Input)
		view.Branches = []decodeBranch{{Name: "stored bytes", Stages: []decodeStage{{
			Key: "stored", Level: "stored payload", Bits: bits, Input: &input,
			Value: fmt.Sprintf("%d uncompressed bytes", op.Length), Relation: "copied directly",
		}}}}
	}
	if block.Dynamic != nil {
		view.Header = []decodeStage{
			s.headerStage("HLIT", block.Dynamic.HLIT, block.Dynamic.NumLiteralLengths, "literal/length code lengths"),
			s.headerStage("HDIST", block.Dynamic.HDIST, block.Dynamic.NumDistanceLengths, "distance code lengths"),
			s.headerStage("HCLEN", block.Dynamic.HCLEN, block.Dynamic.NumCodeLengths, "code-length code lengths"),
		}
	}
	writeJSON(w, view)
}

func (s *server) block(w http.ResponseWriter, r *http.Request) {
	id, err := queryInt(r, "id", -1)
	if err != nil || id < 0 || id >= len(s.trace.Blocks) {
		if err == nil {
			err = fmt.Errorf("block id %d is outside [0,%d)", id, len(s.trace.Blocks))
		}
		writeError(w, err)
		return
	}
	block := s.trace.Blocks[id]
	view := blockDetailView{Block: viewBlock(block)}
	view.Bits, _ = s.trace.BitString(block.Header)
	field := func(key, level string, input deflate.BitRange, value, detail string) decodeStage {
		bits, _ := s.trace.BitString(input)
		inputView := viewSpan(input)
		return decodeStage{Key: key, Level: level, Bits: bits, Input: &inputView, Value: value, Detail: detail}
	}
	finalInput := deflate.BitRange{Start: block.Input.Start, End: block.Input.Start + 1}
	typeInput := deflate.BitRange{Start: block.Input.Start + 1, End: block.Input.Start + 3}
	finalValue := "more blocks follow"
	if block.Final {
		finalValue = "last block"
	}
	view.Fields = append(view.Fields,
		field("block:bfinal", "BFINAL", finalInput, finalValue, "one bit"),
		field("block:btype", "BTYPE", typeInput, block.Kind.String(), "two bits"),
	)
	if block.Dynamic != nil {
		view.Fields = append(view.Fields,
			s.headerStage("HLIT", block.Dynamic.HLIT, block.Dynamic.NumLiteralLengths, "literal/length code lengths"),
			s.headerStage("HDIST", block.Dynamic.HDIST, block.Dynamic.NumDistanceLengths, "distance code lengths"),
			s.headerStage("HCLEN", block.Dynamic.HCLEN, block.Dynamic.NumCodeLengths, "code-length code lengths"),
		)
		view.Runs = len(block.Dynamic.LengthRuns)
		view.Layers = s.dynamicBlockLayers(block)
	}
	if block.Kind == deflate.StoredBlock && block.Header.Len() >= 32 {
		lengthStart := block.Header.End - 32
		if alignment := (deflate.BitRange{Start: typeInput.End, End: lengthStart}); alignment.Len() != 0 {
			view.Fields = append(view.Fields, field("block:alignment", "alignment", alignment, "discarded", "zero to seven bits"))
		}
		length := int(block.Output.End - block.Output.Start)
		view.Fields = append(view.Fields,
			field("block:len", "LEN", deflate.BitRange{Start: lengthStart, End: lengthStart + 16}, strconv.Itoa(length), "stored byte count"),
			field("block:nlen", "NLEN", deflate.BitRange{Start: lengthStart + 16, End: block.Header.End}, strconv.Itoa(int(^uint16(length))), "one's complement of LEN"),
		)
	}
	writeJSON(w, view)
}

func (s *server) dynamicBlockLayers(block deflate.Block) []blockLayer {
	header := block.Dynamic
	if header == nil {
		return nil
	}
	stage := func(key, level, bits, value, detail string, input deflate.BitRange) decodeStage {
		var inputView *span
		if input.Len() != 0 {
			view := viewSpan(input)
			inputView = &view
		}
		return decodeStage{Key: key, Level: level, Bits: bits, Input: inputView, Value: value, Detail: detail}
	}
	layers := []blockLayer{{
		ID: "hclen", Title: "HCLEN vector",
		Summary: fmt.Sprintf("%d three-bit fields assign lengths to the code-length alphabet", len(header.CodeLengths)),
	}}
	for i, field := range header.CodeLengths {
		bits, _ := s.trace.BitString(field.Input)
		layers[0].Items = append(layers[0].Items, stage(
			fmt.Sprintf("hclen:%d", i), fmt.Sprintf("slot %d", i), bits,
			fmt.Sprintf("symbol %d → %d bits", field.Symbol, field.Length), "", field.Input,
		))
	}

	codeTree := blockLayer{ID: "codelen", Title: "code-length tree", Summary: "the HCLEN vector becomes a tiny Huffman tree for reading the length program"}
	if block.CodeTable != nil {
		for _, code := range block.CodeTable.Codes {
			input := deflate.BitRange{}
			for _, field := range header.CodeLengths {
				if field.Symbol == code.Symbol {
					input = field.Input
					break
				}
			}
			codeTree.Items = append(codeTree.Items, stage(
				fmt.Sprintf("codelen:%d", code.Symbol), fmt.Sprintf("symbol %d", code.Symbol), codeBits(code),
				fmt.Sprintf("%d-bit code", code.Bits), "from HCLEN", input,
			))
		}
	}
	layers = append(layers, codeTree)

	program := blockLayer{ID: "program", Title: "length program", Summary: fmt.Sprintf("%d Huffman instructions expand into %d code lengths", len(header.LengthRuns), header.NumLiteralLengths+header.NumDistanceLengths)}
	for i, run := range header.LengthRuns {
		bits, _ := s.trace.BitString(run.Input)
		program.Items = append(program.Items, stage(
			fmt.Sprintf("run:%d", i), fmt.Sprintf("run %d", i), bits, lengthRunValue(run),
			webSlotRange(header, run.SlotsStart, run.SlotsEnd), run.Input,
		))
	}
	layers = append(layers, program)

	lengthLayer := func(id, title, alphabet string, start, count int) blockLayer {
		layer := blockLayer{ID: id, Title: title, Summary: fmt.Sprintf("%d symbol slots receive the lengths produced by the program", count)}
		for symbol := 0; symbol < count; symbol++ {
			runIndex, run, ok := findLengthRun(header, start+symbol)
			if !ok {
				continue
			}
			layer.Items = append(layer.Items, stage(
				fmt.Sprintf("%s-length:%d", alphabet, symbol), fmt.Sprintf("symbol %d", symbol), "",
				fmt.Sprintf("%d bits", run.Length), fmt.Sprintf("from run %d", runIndex), run.Input,
			))
		}
		return layer
	}
	layers = append(layers,
		lengthLayer("litlen-lengths", "literal/length code lengths", "litlen", 0, header.NumLiteralLengths),
		lengthLayer("distance-lengths", "distance code lengths", "distance", header.NumLiteralLengths, header.NumDistanceLengths),
	)

	codeLayer := func(id, title, alphabet string, table *deflate.HuffmanTable, offset int) blockLayer {
		layer := blockLayer{ID: id, Title: title, Summary: "non-zero lengths become the canonical codes used by the payload"}
		if table == nil {
			return layer
		}
		for _, code := range table.Codes {
			_, run, _ := findLengthRun(header, offset+code.Symbol)
			meaning := "distance code"
			if alphabet == "litlen" {
				switch {
				case code.Symbol < 256:
					meaning = "literal " + printableByte(byte(code.Symbol))
				case code.Symbol == 256:
					meaning = "end of block"
				default:
					meaning = "length code"
				}
			}
			layer.Items = append(layer.Items, stage(
				fmt.Sprintf("%s-code:%d", alphabet, code.Symbol), fmt.Sprintf("symbol %d", code.Symbol), codeBits(code),
				meaning, fmt.Sprintf("%d bits", code.Bits), run.Input,
			))
		}
		return layer
	}
	layers = append(layers,
		codeLayer("litlen-tree", "literal/length tree", "litlen", block.LiteralTable, 0),
		codeLayer("distance-tree", "distance tree", "distance", block.DistanceTable, header.NumLiteralLengths),
	)
	return layers
}

func webSlotRange(header *deflate.DynamicHeader, start, end int) string {
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

func (s *server) decodeBranch(block deflate.Block, alphabet string, symbol int, codeInput, extraInput deflate.BitRange, result string) decodeBranch {
	wire, _ := s.trace.BitString(codeInput)
	payloadInput := codeInput
	detail := result
	if extraInput.Len() != 0 {
		extra, _ := s.trace.BitString(extraInput)
		wire += " · " + extra
		payloadInput.End = extraInput.End
		detail += fmt.Sprintf(" · %d extra bit(s)", extraInput.Len())
	}
	inputView := viewSpan(payloadInput)
	branch := decodeBranch{Name: alphabet, Stages: []decodeStage{{
		Key: fmt.Sprintf("payload:%s:%d", alphabet, symbol), Level: "payload bits", Bits: wire, Input: &inputView,
		Value: fmt.Sprintf("symbol %d", symbol), Detail: detail, Relation: "decodes as",
	}}}
	table := block.LiteralTable
	if alphabet == "distance" {
		table = block.DistanceTable
	}
	if code, ok := findCode(table, symbol); ok {
		branch.Stages = append(branch.Stages, decodeStage{
			Key: fmt.Sprintf("table:%d:%s:%d", block.ID, alphabet, symbol), Level: alphabet + " tree",
			Bits: codeBits(code), Value: fmt.Sprintf("leaf %d", symbol),
			Detail: fmt.Sprintf("canonical code · %d bits", code.Bits), Relation: "defined by",
		})
	}
	if block.Dynamic == nil {
		branch.Stages = append(branch.Stages, decodeStage{
			Key: "fixed:" + alphabet, Level: "fixed table", Value: "RFC 1951 §3.2.6",
			Detail: "static code lengths", Relation: "specified by",
		})
		return branch
	}
	header := block.Dynamic
	slot := symbol
	if alphabet == "distance" {
		slot += header.NumLiteralLengths
	}
	runIndex, run, ok := findLengthRun(header, slot)
	if !ok {
		return branch
	}
	branch.Stages = append(branch.Stages, decodeStage{
		Key: fmt.Sprintf("slot:%d:%d", block.ID, slot), Level: "expanded lengths",
		Value:  fmt.Sprintf("%s[%d] = %d", shortAlphabet(alphabet), symbol, run.Length),
		Detail: fmt.Sprintf("slot %d of %d", slot, header.NumLiteralLengths+header.NumDistanceLengths), Relation: "gets its code length from",
	})
	runBits, _ := s.trace.BitString(run.Input)
	runInput := viewSpan(run.Input)
	branch.Stages = append(branch.Stages, decodeStage{
		Key: fmt.Sprintf("run:%d:%d", block.ID, runIndex), Level: "length program", Bits: runBits, Input: &runInput,
		Value: lengthRunValue(run), Detail: fmt.Sprintf("fills slots [%d:%d)", run.SlotsStart, run.SlotsEnd), Relation: "decoded instruction",
	})
	if code, found := findCode(block.CodeTable, run.Symbol); found {
		branch.Stages = append(branch.Stages, decodeStage{
			Key: fmt.Sprintf("codelen:%d:%d", block.ID, run.Symbol), Level: "code-length tree",
			Bits: codeBits(code), Value: fmt.Sprintf("symbol %d", run.Symbol),
			Detail: fmt.Sprintf("canonical code · %d bits", code.Bits), Relation: "defined by",
		})
	}
	for fieldIndex, field := range header.CodeLengths {
		if field.Symbol != run.Symbol {
			continue
		}
		fieldBits, _ := s.trace.BitString(field.Input)
		fieldInput := viewSpan(field.Input)
		branch.Stages = append(branch.Stages, decodeStage{
			Key: fmt.Sprintf("hclen:%d:%d", block.ID, fieldIndex), Level: "HCLEN vector", Bits: fieldBits, Input: &fieldInput,
			Value:  fmt.Sprintf("symbol %d has %d-bit code", field.Symbol, field.Length),
			Detail: fmt.Sprintf("transmission slot %d", fieldIndex), Relation: "declared by",
		})
		break
	}
	return branch
}

func (s *server) headerStage(name string, input deflate.BitRange, count int, detail string) decodeStage {
	bits, _ := s.trace.BitString(input)
	inputView := viewSpan(input)
	return decodeStage{Key: "header:" + strings.ToLower(name), Level: name, Bits: bits, Input: &inputView,
		Value: strconv.Itoa(count), Detail: detail}
}

func findCode(table *deflate.HuffmanTable, symbol int) (deflate.HuffmanCode, bool) {
	if table != nil {
		for _, code := range table.Codes {
			if code.Symbol == symbol {
				return code, true
			}
		}
	}
	return deflate.HuffmanCode{}, false
}

func findLengthRun(header *deflate.DynamicHeader, slot int) (int, deflate.CodeLengthRun, bool) {
	for i, run := range header.LengthRuns {
		if run.SlotsStart <= slot && slot < run.SlotsEnd {
			return i, run, true
		}
	}
	return 0, deflate.CodeLengthRun{}, false
}

func codeBits(code deflate.HuffmanCode) string {
	return fmt.Sprintf("%0*b", code.Bits, code.Code)
}

func shortAlphabet(alphabet string) string {
	if alphabet == "literal/length" {
		return "litlen"
	}
	return alphabet
}

func lengthRunValue(run deflate.CodeLengthRun) string {
	switch run.Symbol {
	case 16:
		return fmt.Sprintf("repeat previous length ×%d", run.Repeat)
	case 17, 18:
		return fmt.Sprintf("zero length ×%d", run.Repeat)
	default:
		return fmt.Sprintf("length %d", run.Length)
	}
}

func printableByte(value byte) string {
	if value >= 0x20 && value <= 0x7e {
		return strconv.QuoteRune(rune(value))
	}
	return fmt.Sprintf("0x%02x", value)
}

func (s *server) origin(w http.ResponseWriter, r *http.Request) {
	offset, err := queryUint64(r, "offset", 0)
	if err != nil {
		writeError(w, err)
		return
	}
	steps, err := s.trace.Origin(deflate.OutPos(offset))
	if err != nil {
		writeError(w, err)
		return
	}
	type stepView struct {
		Output       uint64  `json:"output"`
		Op           opView  `json:"op"`
		DirectSource *uint64 `json:"directSource,omitempty"`
		SeedSource   *uint64 `json:"seedSource,omitempty"`
		StoredInput  *span   `json:"storedInput,omitempty"`
	}
	views := make([]stepView, 0, len(steps))
	for _, step := range steps {
		v := stepView{Output: uint64(step.Output), Op: s.viewOp(step.Op)}
		if step.Op.Kind == deflate.MatchOp {
			direct, seed := uint64(step.DirectSource), uint64(step.SeedSource)
			v.DirectSource, v.SeedSource = &direct, &seed
		}
		if step.Op.Kind == deflate.StoredOp {
			stored := viewSpan(step.StoredInput)
			v.StoredInput = &stored
		}
		views = append(views, v)
	}
	writeJSON(w, map[string]any{
		"offset": offset,
		"byte":   s.trace.Output[offset],
		"steps":  views,
	})
}

func (s *server) uses(w http.ResponseWriter, r *http.Request) {
	start, err := queryUint64(r, "start", 0)
	if err != nil {
		writeError(w, err)
		return
	}
	end, err := queryUint64(r, "end", start+1)
	if err != nil {
		writeError(w, err)
		return
	}
	uses := s.trace.Uses(deflate.OutRange{Start: deflate.OutPos(start), End: deflate.OutPos(end)})
	type useView struct {
		Op           opView  `json:"op"`
		Seed         outSpan `json:"seed"`
		Intersection outSpan `json:"intersection"`
		Repeats      int     `json:"repeats"`
	}
	views := make([]useView, 0, len(uses))
	for _, use := range uses {
		views = append(views, useView{
			Op:           s.viewOp(use.Op),
			Seed:         viewOut(use.Seed),
			Intersection: viewOut(use.Intersection),
			Repeats:      use.Repeats,
		})
	}
	writeJSON(w, map[string]any{"start": start, "end": end, "uses": views})
}

func (s *server) output(w http.ResponseWriter, r *http.Request) {
	start, err := queryUint64(r, "start", 0)
	if err != nil {
		writeError(w, err)
		return
	}
	length, err := queryUint64(r, "length", 256)
	if err != nil {
		writeError(w, err)
		return
	}
	if length > 4096 {
		length = 4096
	}
	if start > uint64(len(s.trace.Output)) {
		writeError(w, fmt.Errorf("output start %d is outside [0,%d]", start, len(s.trace.Output)))
		return
	}
	end := start + length
	if end > uint64(len(s.trace.Output)) {
		end = uint64(len(s.trace.Output))
	}
	b := s.trace.Output[start:end]
	text := strings.Map(func(r rune) rune {
		if r < 0x20 || r > 0x7e {
			return '.'
		}
		return r
	}, string(b))
	writeJSON(w, map[string]any{
		"start":   start,
		"end":     end,
		"hex":     hex.EncodeToString(b),
		"text":    text,
		"anchors": s.outputAnchors(start, end),
	})
}

func (s *server) input(w http.ResponseWriter, r *http.Request) {
	start, err := queryUint64(r, "start", 0)
	if err != nil {
		writeError(w, err)
		return
	}
	length, err := queryUint64(r, "length", 256)
	if err != nil {
		writeError(w, err)
		return
	}
	if length > 4096 {
		length = 4096
	}
	total := uint64(s.trace.ConsumedBits())
	if start > total {
		writeError(w, fmt.Errorf("input bit %d is outside [0,%d]", start, total))
		return
	}
	end := start + length
	if end > total {
		end = total
	}
	bitRange := deflate.BitRange{Start: deflate.BitPos(start), End: deflate.BitPos(end)}
	bits, err := s.trace.BitString(bitRange)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, map[string]any{
		"startBit": start,
		"endBit":   end,
		"bits":     bits,
		"anchors":  s.inputAnchors(start, end),
	})
}

// Anchors use [operation id, input start, input end, output start, output end].
// The positional encoding keeps the per-window scroll index compact enough to
// ship with the bytes/bits it describes.
func (s *server) inputAnchors(start, end uint64) [][5]uint64 {
	first := sort.Search(len(s.trace.Ops), func(i int) bool {
		return uint64(s.trace.Ops[i].Input.End) > start
	})
	anchors := make([][5]uint64, 0, 256)
	for _, op := range s.trace.Ops[first:] {
		if uint64(op.Input.Start) >= end {
			break
		}
		anchors = append(anchors, scrollAnchor(op))
	}
	return anchors
}

func (s *server) outputAnchors(start, end uint64) [][5]uint64 {
	first := sort.Search(len(s.trace.Ops), func(i int) bool {
		return uint64(s.trace.Ops[i].Output.End) > start
	})
	anchors := make([][5]uint64, 0, 256)
	for _, op := range s.trace.Ops[first:] {
		if uint64(op.Output.Start) >= end {
			break
		}
		anchors = append(anchors, scrollAnchor(op))
	}
	return anchors
}

func scrollAnchor(op deflate.Op) [5]uint64 {
	return [5]uint64{
		uint64(op.ID),
		uint64(op.Input.Start), uint64(op.Input.End),
		uint64(op.Output.Start), uint64(op.Output.End),
	}
}

func viewBlock(block deflate.Block) blockView {
	return blockView{
		ID:     block.ID,
		Kind:   block.Kind.String(),
		Final:  block.Final,
		Input:  viewSpan(block.Input),
		Header: viewSpan(block.Header),
		Output: viewOut(block.Output),
		First:  block.FirstOp,
		Count:  block.NumOps,
	}
}

func (s *server) viewOp(op deflate.Op) opView {
	v := opView{
		ID:                op.ID,
		BlockID:           op.BlockID,
		Kind:              op.Kind.String(),
		Input:             viewSpan(op.Input),
		Output:            viewOut(op.Output),
		Length:            int(op.Length),
		Distance:          int(op.Distance),
		SymbolBits:        viewSpan(op.SymbolBits),
		LengthExtraBits:   viewSpan(op.LengthExtraBits),
		DistanceBits:      viewSpan(op.DistanceBits),
		DistanceExtraBits: viewSpan(op.DistanceExtraBits),
	}
	if op.Kind != deflate.StoredOp {
		v.InputBits, _ = s.trace.BitString(op.Input)
	}
	v.SymbolCode, _ = s.trace.BitString(op.SymbolBits)
	v.LengthExtraCode, _ = s.trace.BitString(op.LengthExtraBits)
	v.DistanceCode, _ = s.trace.BitString(op.DistanceBits)
	v.DistanceExtraCode, _ = s.trace.BitString(op.DistanceExtraBits)
	if op.Kind == deflate.LiteralOp {
		literal := op.Literal
		v.Literal = &literal
	}
	if seed, ok := op.Seed(); ok {
		value := viewOut(seed)
		v.Seed = &value
	}
	return v
}

func viewSpan(value deflate.BitRange) span {
	return span{
		Start: value.Start.String(), End: value.End.String(),
		StartBit: uint64(value.Start), EndBit: uint64(value.End), Bits: value.Len(),
	}
}

func viewOut(value deflate.OutRange) outSpan {
	return outSpan{Start: uint64(value.Start), End: uint64(value.End)}
}

func queryInt(r *http.Request, name string, fallback int) (int, error) {
	value := r.URL.Query().Get(name)
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("invalid %s %q", name, value)
	}
	return parsed, nil
}

func queryUint64(r *http.Request, name string, fallback uint64) (uint64, error) {
	value := r.URL.Query().Get(name)
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseUint(value, 0, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid %s %q", name, value)
	}
	return parsed, nil
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(value)
}

func writeError(w http.ResponseWriter, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	writeJSON(w, map[string]string{"error": err.Error()})
}
