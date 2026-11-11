package deflate

import (
	"fmt"
	"sort"
)

// OriginStep is one hop while tracing an output byte toward the literal or
// stored input byte which originated its value.
type OriginStep struct {
	Output OutPos
	Op     Op

	// DirectSource is the byte read by the DEFLATE machine. It can point into
	// the same match when the copy overlaps its destination.
	DirectSource OutPos

	// SeedSource is DirectSource normalized into the pre-match seed range.
	SeedSource OutPos

	// StoredInput is the compressed byte holding a stored-block output byte.
	StoredInput BitRange
}

// Producer returns the operation which produced an output byte.
func (t *Trace) Producer(pos OutPos) (Op, bool) {
	i := sort.Search(len(t.Ops), func(i int) bool {
		return t.Ops[i].Output.End > pos
	})
	if i == len(t.Ops) || !t.Ops[i].Output.Contains(pos) {
		return Op{}, false
	}
	return t.Ops[i], true
}

// Origin follows match-copy dependencies until pos resolves to a literal or a
// stored-block byte. Overlapping matches are normalized to their seed range,
// so the path contains at most one step per match instruction.
func (t *Trace) Origin(pos OutPos) ([]OriginStep, error) {
	if uint64(pos) >= uint64(len(t.Output)) {
		return nil, fmt.Errorf("output offset %d is outside [0,%d)", pos, len(t.Output))
	}
	steps := make([]OriginStep, 0, 4)
	for {
		op, ok := t.Producer(pos)
		if !ok {
			return nil, fmt.Errorf("no producer for output offset %d", pos)
		}
		step := OriginStep{Output: pos, Op: op}
		switch op.Kind {
		case LiteralOp:
			steps = append(steps, step)
			return steps, nil
		case StoredOp:
			delta := uint64(pos - op.Output.Start)
			step.StoredInput = BitRange{
				Start: op.Input.Start + BitPos(delta*8),
				End:   op.Input.Start + BitPos((delta+1)*8),
			}
			steps = append(steps, step)
			return steps, nil
		case MatchOp:
			delta := uint64(pos - op.Output.Start)
			step.DirectSource = OutPos(uint64(pos) - uint64(op.Distance))
			step.SeedSource = OutPos(uint64(op.Output.Start) - uint64(op.Distance) + delta%uint64(op.Distance))
			steps = append(steps, step)
			pos = step.SeedSource
		default:
			return nil, fmt.Errorf("unknown operation kind %d", op.Kind)
		}
	}
}

// Use describes a match instruction whose normalized seed intersects a query.
type Use struct {
	Op           Op
	Seed         OutRange
	Intersection OutRange
	Repeats      int
}

// Production describes the part of an output range produced by one
// instruction. It is the forward counterpart to Use: Uses answers which
// matches consume a range, while Producers answers which instructions created
// it.
type Production struct {
	Op           Op
	Intersection OutRange
}

// Producers returns the instructions which produced bytes in query. Results
// are ordered by output position and contain only the intersecting portion of
// each instruction. Since operations partition the output, the implementation
// can enter the instruction stream with one binary search and scan forward.
func (t *Trace) Producers(query OutRange) []Production {
	var productions []Production
	if query.End <= query.Start {
		return productions
	}
	i := sort.Search(len(t.Ops), func(i int) bool {
		return t.Ops[i].Output.End > query.Start
	})
	for ; i < len(t.Ops) && t.Ops[i].Output.Start < query.End; i++ {
		op := t.Ops[i]
		start := maxOut(query.Start, op.Output.Start)
		end := minOut(query.End, op.Output.End)
		if start < end {
			productions = append(productions, Production{
				Op:           op,
				Intersection: OutRange{Start: start, End: end},
			})
		}
	}
	return productions
}

type useIndexEntry struct {
	seed OutRange
	op   int
}

func (t *Trace) buildIndexes() {
	t.uses = t.uses[:0]
	for _, op := range t.Ops {
		seed, ok := op.Seed()
		if ok {
			t.uses = append(t.uses, useIndexEntry{seed: seed, op: op.ID})
		}
	}
	sort.Slice(t.uses, func(i, j int) bool {
		if t.uses[i].seed.Start == t.uses[j].seed.Start {
			return t.uses[i].seed.End < t.uses[j].seed.End
		}
		return t.uses[i].seed.Start < t.uses[j].seed.Start
	})
	t.useMax = make([]OutPos, len(t.uses))
	var high OutPos
	for i, entry := range t.uses {
		if entry.seed.End > high {
			high = entry.seed.End
		}
		t.useMax[i] = high
	}
}

// Uses finds match instructions which consume bytes from the selected output
// range. Results describe normalized seed dependencies rather than expanding
// overlapping copies into one edge per output byte.
func (t *Trace) Uses(query OutRange) []Use {
	var uses []Use
	if query.End <= query.Start {
		return uses
	}
	// Entries are ordered by seed start. Walk backward from the final start
	// before query.End, stopping once the prefix maximum proves no earlier
	// interval can overlap the query.
	i := sort.Search(len(t.uses), func(i int) bool {
		return t.uses[i].seed.Start >= query.End
	})
	for i--; i >= 0; i-- {
		if t.useMax[i] <= query.Start {
			break
		}
		entry := t.uses[i]
		seed := entry.seed
		start := maxOut(query.Start, seed.Start)
		end := minOut(query.End, seed.End)
		if start >= end {
			continue
		}
		op := t.Ops[entry.op]
		repeats := op.Length / op.Distance
		if op.Length%op.Distance != 0 {
			repeats++
		}
		uses = append(uses, Use{
			Op:           op,
			Seed:         seed,
			Intersection: OutRange{Start: start, End: end},
			Repeats:      repeats,
		})
	}
	sort.Slice(uses, func(i, j int) bool { return uses[i].Op.ID < uses[j].Op.ID })
	return uses
}

func minOut(a, b OutPos) OutPos {
	if a < b {
		return a
	}
	return b
}

func maxOut(a, b OutPos) OutPos {
	if a > b {
		return a
	}
	return b
}
