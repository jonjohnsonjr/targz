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
	"strconv"
	"strings"

	"github.com/jonjohnsonjr/targz/deflate"
)

//go:embed index.html
var indexHTML []byte

// Handler returns a read-only debugger application for trace.
func Handler(trace *deflate.Trace) http.Handler {
	s := &server{trace: trace}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", s.index)
	mux.HandleFunc("GET /api/summary", s.summary)
	mux.HandleFunc("GET /api/ops", s.ops)
	mux.HandleFunc("GET /api/op", s.op)
	mux.HandleFunc("GET /api/origin", s.origin)
	mux.HandleFunc("GET /api/uses", s.uses)
	mux.HandleFunc("GET /api/output", s.output)
	return mux
}

type server struct {
	trace *deflate.Trace
}

type span struct {
	Start string `json:"start"`
	End   string `json:"end"`
	Bits  uint64 `json:"bits"`
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
		"start": start,
		"end":   end,
		"hex":   hex.EncodeToString(b),
		"text":  text,
	})
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
		Length:            op.Length,
		Distance:          op.Distance,
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
	return span{Start: value.Start.String(), End: value.End.String(), Bits: value.Len()}
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
