// Package deflate parses DEFLATE streams into a bit-exact, queryable trace.
//
// Unlike an ordinary decompressor, the primary result is the instruction
// stream. Decompressed bytes are one projection of that trace. Every
// instruction records both the compressed bits which control it and the output
// range it produces.
package deflate

import (
	"fmt"
	"strings"
)

// BitPos is an absolute bit offset in the compressed source. DEFLATE consumes
// bits least-significant bit first within each byte, so bit 0 is the low bit of
// source byte 0.
type BitPos uint64

func (p BitPos) String() string {
	return fmt.Sprintf("%x.%d", uint64(p)/8, uint64(p)%8)
}

// OutPos is an absolute byte offset in the decompressed output.
type OutPos uint64

// BitRange is a half-open compressed-input range.
type BitRange struct {
	Start BitPos
	End   BitPos
}

func (r BitRange) Len() uint64 { return uint64(r.End - r.Start) }

// OutRange is a half-open decompressed-output range.
type OutRange struct {
	Start OutPos
	End   OutPos
}

func (r OutRange) Len() uint64 { return uint64(r.End - r.Start) }

func (r OutRange) Contains(p OutPos) bool { return r.Start <= p && p < r.End }

// BlockKind is the encoding of a DEFLATE block.
type BlockKind uint8

const (
	StoredBlock BlockKind = iota
	FixedBlock
	DynamicBlock
)

func (k BlockKind) String() string {
	switch k {
	case StoredBlock:
		return "stored"
	case FixedBlock:
		return "fixed"
	case DynamicBlock:
		return "dynamic"
	default:
		return fmt.Sprintf("block(%d)", k)
	}
}

// OpKind describes an output-producing DEFLATE instruction.
type OpKind uint8

const (
	LiteralOp OpKind = iota
	MatchOp
	StoredOp
)

func (k OpKind) String() string {
	switch k {
	case LiteralOp:
		return "literal"
	case MatchOp:
		return "match"
	case StoredOp:
		return "stored"
	default:
		return fmt.Sprintf("op(%d)", k)
	}
}

// Op is one output-producing instruction. Match fields are meaningful only
// for MatchOp, and Literal is meaningful only for LiteralOp.
type Op struct {
	ID      int
	BlockID int
	Kind    OpKind

	Input  BitRange
	Output OutRange

	Literal  byte
	Length   int
	Distance int

	SymbolBits        BitRange
	LengthExtraBits   BitRange
	DistanceBits      BitRange
	DistanceExtraBits BitRange
}

// Seed returns the range preceding a match which is repeated to produce its
// output. This normalization is especially useful for overlapping matches.
func (op Op) Seed() (OutRange, bool) {
	if op.Kind != MatchOp || op.Distance <= 0 {
		return OutRange{}, false
	}
	seedLen := uint64(op.Distance)
	if n := op.Output.Len(); seedLen > n {
		seedLen = n
	}
	start := OutPos(uint64(op.Output.Start) - uint64(op.Distance))
	return OutRange{Start: start, End: start + OutPos(seedLen)}, true
}

// Block describes a DEFLATE block and the operations it contains.
type Block struct {
	ID int

	Final bool
	Kind  BlockKind

	Input  BitRange
	Header BitRange
	Output OutRange

	FirstOp int
	NumOps  int

	LiteralTable  *HuffmanTable
	DistanceTable *HuffmanTable
	CodeTable     *HuffmanTable
}

// HuffmanCode is one canonical Huffman code. Code is written in the natural
// MSB-first representation used by RFC 1951 diagrams.
type HuffmanCode struct {
	Symbol int
	Code   uint16
	Bits   uint8
}

// HuffmanTable retains the canonical codes used to decode a block.
type HuffmanTable struct {
	Codes []HuffmanCode
}

// Member describes one gzip member. Raw DEFLATE traces have no members.
type Member struct {
	ID int

	Name    string
	Comment string
	Extra   []byte

	Input   BitRange
	Header  BitRange
	Body    BitRange
	Trailer BitRange
	Output  OutRange

	CRC32 uint32
	ISize uint32
}

// Trace is an immutable decoded stream.
type Trace struct {
	Blocks  []Block
	Ops     []Op
	Members []Member
	Output  []byte

	input    []byte
	consumed BitPos
	uses     []useIndexEntry
	useMax   []OutPos
}

// ConsumedBits reports the logical end of the parsed raw DEFLATE stream. For
// gzip input, it reports the end of the final member including its trailer.
func (t *Trace) ConsumedBits() BitPos { return t.consumed }

// BitString returns source bits in logical consumption order. The leftmost
// character is the first bit consumed, independent of how bits are packed into
// source bytes.
func (t *Trace) BitString(r BitRange) (string, error) {
	if r.End < r.Start || uint64(r.End) > uint64(len(t.input))*8 {
		return "", fmt.Errorf("compressed bit range [%s,%s) is outside input", r.Start, r.End)
	}
	var b strings.Builder
	b.Grow(int(r.Len()))
	for pos := r.Start; pos < r.End; pos++ {
		bytePos := uint64(pos) >> 3
		bitPos := uint64(pos) & 7
		if t.input[bytePos]&(1<<bitPos) != 0 {
			b.WriteByte('1')
		} else {
			b.WriteByte('0')
		}
	}
	return b.String(), nil
}

// ParseError identifies the logical compressed bit where parsing failed.
type ParseError struct {
	At      BitPos
	Message string
}

func (e *ParseError) Error() string {
	return fmt.Sprintf("deflate bit %s: %s", e.At, e.Message)
}
