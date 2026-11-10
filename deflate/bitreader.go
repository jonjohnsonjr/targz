package deflate

import "io"

type bitReader struct {
	data []byte
	pos  BitPos
}

func newBitReader(data []byte) *bitReader { return &bitReader{data: data} }

func (r *bitReader) position() BitPos { return r.pos }

func (r *bitReader) remaining() uint64 {
	total := uint64(len(r.data)) * 8
	if uint64(r.pos) >= total {
		return 0
	}
	return total - uint64(r.pos)
}

// bits reads an RFC 1951 integer: the first bit in the stream is the low bit
// of the result.
func (r *bitReader) bits(n uint8) (uint32, BitRange, error) {
	start := r.pos
	if n > 32 || r.remaining() < uint64(n) {
		return 0, BitRange{Start: start, End: r.pos}, io.ErrUnexpectedEOF
	}
	var v uint32
	for i := uint8(0); i < n; i++ {
		bytePos := uint64(r.pos) >> 3
		bitPos := uint64(r.pos) & 7
		v |= uint32((r.data[bytePos]>>bitPos)&1) << i
		r.pos++
	}
	return v, BitRange{Start: start, End: r.pos}, nil
}

// codeBit reads the next physical bit. Huffman codes use these bits in
// MSB-first code order even though they are packed into bytes LSB-first.
func (r *bitReader) codeBit() (uint8, error) {
	v, _, err := r.bits(1)
	return uint8(v), err
}

func (r *bitReader) alignByte() BitRange {
	start := r.pos
	if rem := uint64(r.pos) & 7; rem != 0 {
		r.pos += BitPos(8 - rem)
	}
	return BitRange{Start: start, End: r.pos}
}

func (r *bitReader) bytes(n int) ([]byte, BitRange, error) {
	start := r.pos
	if uint64(r.pos)&7 != 0 {
		return nil, BitRange{Start: start, End: start}, &ParseError{At: start, Message: "unaligned byte read"}
	}
	bytePos := uint64(r.pos) >> 3
	if n < 0 || bytePos+uint64(n) > uint64(len(r.data)) {
		return nil, BitRange{Start: start, End: start}, io.ErrUnexpectedEOF
	}
	r.pos += BitPos(uint64(n) * 8)
	return r.data[bytePos : bytePos+uint64(n)], BitRange{Start: start, End: r.pos}, nil
}

func (r *bitReader) byte() (byte, BitRange, error) {
	b, span, err := r.bytes(1)
	if err != nil {
		return 0, span, err
	}
	return b[0], span, nil
}
