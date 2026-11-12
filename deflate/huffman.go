package deflate

import "fmt"

const maxCodeBits = 15

type decoderTable struct {
	public HuffmanTable
	count  [maxCodeBits + 1]int
	first  [maxCodeBits + 1]uint16
	index  [maxCodeBits + 1]int
	symbol []int
	max    int
}

func buildTable(lengths []uint8) (*decoderTable, error) {
	var count [maxCodeBits + 1]int
	symbols := 0
	maxBits := 0
	for _, n := range lengths {
		if n > maxCodeBits {
			return nil, fmt.Errorf("invalid Huffman code length %d", n)
		}
		if n != 0 {
			count[n]++
			symbols++
			if int(n) > maxBits {
				maxBits = int(n)
			}
		}
	}
	if symbols == 0 {
		// RFC 1951 permits an empty distance tree when a dynamic block has
		// no match instructions. Attempting to decode from it still fails.
		return &decoderTable{}, nil
	}

	left := 1
	for bits := 1; bits <= maxCodeBits; bits++ {
		left <<= 1
		left -= count[bits]
		if left < 0 {
			return nil, fmt.Errorf("oversubscribed Huffman tree")
		}
	}
	if left != 0 && !(symbols == 1 && maxBits == 1) {
		return nil, fmt.Errorf("incomplete Huffman tree")
	}

	var next [maxCodeBits + 1]uint16
	var code uint16
	for bits := 1; bits <= maxCodeBits; bits++ {
		code = (code + uint16(count[bits-1])) << 1
		next[bits] = code
	}

	t := &decoderTable{count: count, symbol: make([]int, symbols)}
	offset := 0
	for bits := 1; bits <= maxCodeBits; bits++ {
		t.first[bits] = next[bits]
		t.index[bits] = offset
		offset += count[bits]
	}
	for symbol, n := range lengths {
		if n == 0 {
			continue
		}
		c := next[n]
		next[n]++
		t.symbol[t.index[n]+int(c-t.first[n])] = symbol
		if int(n) > t.max {
			t.max = int(n)
		}
		t.public.Codes = append(t.public.Codes, HuffmanCode{
			Symbol: symbol,
			Code:   c,
			Bits:   n,
		})
	}
	return t, nil
}

func (t *decoderTable) decode(r *bitReader) (int, BitRange, error) {
	start := r.position()
	var code uint16
	for bits := 1; bits <= t.max; bits++ {
		bit, err := r.codeBit()
		if err != nil {
			return 0, BitRange{Start: start, End: r.position()}, err
		}
		code = code<<1 | uint16(bit)
		offset := int(code) - int(t.first[bits])
		if offset >= 0 && offset < t.count[bits] {
			return t.symbol[t.index[bits]+offset], BitRange{Start: start, End: r.position()}, nil
		}
	}
	return 0, BitRange{Start: start, End: r.position()}, &ParseError{
		At:      start,
		Message: "invalid Huffman code",
	}
}

func cloneTable(t *decoderTable) *HuffmanTable {
	if t == nil {
		return nil
	}
	ret := &HuffmanTable{Codes: make([]HuffmanCode, len(t.public.Codes))}
	copy(ret.Codes, t.public.Codes)
	return ret
}

func fixedTables() (*decoderTable, *decoderTable) {
	litLengths := make([]uint8, 288)
	for i := 0; i <= 143; i++ {
		litLengths[i] = 8
	}
	for i := 144; i <= 255; i++ {
		litLengths[i] = 9
	}
	for i := 256; i <= 279; i++ {
		litLengths[i] = 7
	}
	for i := 280; i <= 287; i++ {
		litLengths[i] = 8
	}
	distLengths := make([]uint8, 32)
	for i := range distLengths {
		distLengths[i] = 5
	}
	lit, _ := buildTable(litLengths)
	dist, _ := buildTable(distLengths)
	return lit, dist
}
