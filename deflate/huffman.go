package deflate

import "fmt"

const maxCodeBits = 15

type decoderTable struct {
	public HuffmanTable
	byLen  [maxCodeBits + 1]map[uint16]int
	max    int
}

func buildTable(lengths []uint8) (*decoderTable, error) {
	var count [maxCodeBits + 1]int
	for _, n := range lengths {
		if n > maxCodeBits {
			return nil, fmt.Errorf("invalid Huffman code length %d", n)
		}
		if n != 0 {
			count[n]++
		}
	}

	left := 1
	for bits := 1; bits <= maxCodeBits; bits++ {
		left <<= 1
		left -= count[bits]
		if left < 0 {
			return nil, fmt.Errorf("oversubscribed Huffman tree")
		}
	}

	var next [maxCodeBits + 1]uint16
	var code uint16
	for bits := 1; bits <= maxCodeBits; bits++ {
		code = (code + uint16(count[bits-1])) << 1
		next[bits] = code
	}

	t := &decoderTable{}
	for symbol, n := range lengths {
		if n == 0 {
			continue
		}
		c := next[n]
		next[n]++
		if t.byLen[n] == nil {
			t.byLen[n] = make(map[uint16]int)
		}
		t.byLen[n][c] = symbol
		if int(n) > t.max {
			t.max = int(n)
		}
		t.public.Codes = append(t.public.Codes, HuffmanCode{
			Symbol: symbol,
			Code:   c,
			Bits:   n,
		})
	}
	if len(t.public.Codes) == 0 {
		return nil, fmt.Errorf("empty Huffman tree")
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
		if symbol, ok := t.byLen[bits][code]; ok {
			return symbol, BitRange{Start: start, End: r.position()}, nil
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
