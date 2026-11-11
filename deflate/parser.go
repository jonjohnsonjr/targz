package deflate

import (
	"fmt"
	"io"
)

var lengthBase = [...]int{
	3, 4, 5, 6, 7, 8, 9, 10,
	11, 13, 15, 17,
	19, 23, 27, 31,
	35, 43, 51, 59,
	67, 83, 99, 115,
	131, 163, 195, 227,
	258,
}

var lengthExtra = [...]uint8{
	0, 0, 0, 0, 0, 0, 0, 0,
	1, 1, 1, 1,
	2, 2, 2, 2,
	3, 3, 3, 3,
	4, 4, 4, 4,
	5, 5, 5, 5,
	0,
}

var distanceBase = [...]int{
	1, 2, 3, 4,
	5, 7,
	9, 13,
	17, 25,
	33, 49,
	65, 97,
	129, 193,
	257, 385,
	513, 769,
	1025, 1537,
	2049, 3073,
	4097, 6145,
	8193, 12289,
	16385, 24577,
}

var distanceExtra = [...]uint8{
	0, 0, 0, 0,
	1, 1,
	2, 2,
	3, 3,
	4, 4,
	5, 5,
	6, 6,
	7, 7,
	8, 8,
	9, 9,
	10, 10,
	11, 11,
	12, 12,
	13, 13,
}

var codeOrder = [...]int{16, 17, 18, 0, 8, 7, 9, 6, 10, 5, 11, 4, 12, 3, 13, 2, 14, 1, 15}

// ParseRaw parses one raw RFC 1951 stream. Bytes following the final block are
// not an error; ConsumedBits identifies the exact end of the stream.
func ParseRaw(data []byte) (*Trace, error) {
	r := newBitReader(data)
	t := &Trace{input: append([]byte(nil), data...)}
	p := parser{r: r, trace: t}
	if err := p.blocks(0); err != nil {
		return nil, err
	}
	t.consumed = r.position()
	t.buildIndexes()
	return t, nil
}

type parser struct {
	r     *bitReader
	trace *Trace
}

func (p *parser) fail(at BitPos, format string, args ...any) error {
	return &ParseError{At: at, Message: fmt.Sprintf(format, args...)}
}

func (p *parser) blocks(historyBase OutPos) error {
	for {
		blockStart := p.r.position()
		final, _, err := p.r.bits(1)
		if err != nil {
			return p.fail(blockStart, "reading final-block bit: %v", err)
		}
		kindBits, _, err := p.r.bits(2)
		if err != nil {
			return p.fail(blockStart, "reading block kind: %v", err)
		}

		block := Block{
			ID:      len(p.trace.Blocks),
			Final:   final != 0,
			Input:   BitRange{Start: blockStart},
			Output:  OutRange{Start: OutPos(len(p.trace.Output))},
			FirstOp: len(p.trace.Ops),
		}

		switch kindBits {
		case 0:
			block.Kind = StoredBlock
			if err := p.storedBlock(&block); err != nil {
				return err
			}
		case 1:
			block.Kind = FixedBlock
			lit, dist := fixedTables()
			block.Header = BitRange{Start: blockStart, End: p.r.position()}
			block.LiteralTable = cloneTable(lit)
			block.DistanceTable = cloneTable(dist)
			if err := p.compressedBlock(&block, lit, dist, historyBase); err != nil {
				return err
			}
		case 2:
			block.Kind = DynamicBlock
			lit, dist, code, dynamic, err := p.dynamicTables()
			if err != nil {
				return err
			}
			block.Header = BitRange{Start: blockStart, End: p.r.position()}
			block.LiteralTable = cloneTable(lit)
			block.DistanceTable = cloneTable(dist)
			block.CodeTable = cloneTable(code)
			block.Dynamic = dynamic
			if err := p.compressedBlock(&block, lit, dist, historyBase); err != nil {
				return err
			}
		default:
			return p.fail(blockStart, "reserved block kind 3")
		}

		block.Input.End = p.r.position()
		block.Output.End = OutPos(len(p.trace.Output))
		block.NumOps = len(p.trace.Ops) - block.FirstOp
		p.trace.Blocks = append(p.trace.Blocks, block)
		if block.Final {
			return nil
		}
	}
}

func (p *parser) storedBlock(block *Block) error {
	p.r.alignByte()
	lengthBytes, _, err := p.r.bytes(4)
	if err != nil {
		return p.fail(p.r.position(), "reading stored-block length: %v", err)
	}
	n := int(lengthBytes[0]) | int(lengthBytes[1])<<8
	nn := int(lengthBytes[2]) | int(lengthBytes[3])<<8
	if uint16(nn) != ^uint16(n) {
		return p.fail(p.r.position()-32, "stored-block length complement does not match")
	}
	block.Header = BitRange{Start: block.Input.Start, End: p.r.position()}
	data, span, err := p.r.bytes(n)
	if err != nil {
		return p.fail(p.r.position(), "reading %d stored bytes: %v", n, err)
	}
	if n == 0 {
		return nil
	}
	start := OutPos(len(p.trace.Output))
	p.trace.Output = append(p.trace.Output, data...)
	p.trace.Ops = append(p.trace.Ops, Op{
		ID:      len(p.trace.Ops),
		BlockID: block.ID,
		Kind:    StoredOp,
		Input:   span,
		Output:  OutRange{Start: start, End: OutPos(len(p.trace.Output))},
		Length:  n,
	})
	return nil
}

func (p *parser) compressedBlock(block *Block, lit, dist *decoderTable, historyBase OutPos) error {
	for {
		symbol, symbolBits, err := lit.decode(p.r)
		if err != nil {
			return p.fail(symbolBits.Start, "decoding literal/length symbol: %v", err)
		}
		switch {
		case symbol < 256:
			start := OutPos(len(p.trace.Output))
			p.trace.Output = append(p.trace.Output, byte(symbol))
			p.trace.Ops = append(p.trace.Ops, Op{
				ID:         len(p.trace.Ops),
				BlockID:    block.ID,
				Kind:       LiteralOp,
				Input:      symbolBits,
				Output:     OutRange{Start: start, End: start + 1},
				Literal:    byte(symbol),
				Length:     1,
				SymbolBits: symbolBits,
			})
		case symbol == 256:
			return nil
		case symbol < 257 || symbol > 285:
			return p.fail(symbolBits.Start, "reserved literal/length symbol %d", symbol)
		default:
			lengthIndex := symbol - 257
			length := lengthBase[lengthIndex]
			extra, lengthExtraBits, err := p.r.bits(lengthExtra[lengthIndex])
			if err != nil {
				return p.fail(p.r.position(), "reading length extra bits: %v", err)
			}
			length += int(extra)

			distanceSymbol, distanceBits, err := dist.decode(p.r)
			if err != nil {
				return p.fail(p.r.position(), "decoding distance symbol: %v", err)
			}
			if distanceSymbol < 0 || distanceSymbol >= len(distanceBase) {
				return p.fail(distanceBits.Start, "reserved distance symbol %d", distanceSymbol)
			}
			distanceValue := distanceBase[distanceSymbol]
			distanceExtraValue, distanceExtraBits, err := p.r.bits(distanceExtra[distanceSymbol])
			if err != nil {
				return p.fail(p.r.position(), "reading distance extra bits: %v", err)
			}
			distanceValue += int(distanceExtraValue)

			start := OutPos(len(p.trace.Output))
			available := uint64(start - historyBase)
			if distanceValue > 32768 || uint64(distanceValue) > available {
				return p.fail(distanceBits.Start, "distance %d exceeds %d bytes of history", distanceValue, available)
			}
			for i := 0; i < length; i++ {
				source := len(p.trace.Output) - distanceValue
				p.trace.Output = append(p.trace.Output, p.trace.Output[source])
			}
			p.trace.Ops = append(p.trace.Ops, Op{
				ID:                len(p.trace.Ops),
				BlockID:           block.ID,
				Kind:              MatchOp,
				Input:             BitRange{Start: symbolBits.Start, End: p.r.position()},
				Output:            OutRange{Start: start, End: OutPos(len(p.trace.Output))},
				Length:            length,
				Distance:          distanceValue,
				SymbolBits:        symbolBits,
				LengthExtraBits:   lengthExtraBits,
				DistanceBits:      distanceBits,
				DistanceExtraBits: distanceExtraBits,
			})
		}
	}
}

func (p *parser) dynamicTables() (lit, dist, code *decoderTable, header *DynamicHeader, err error) {
	start := p.r.position()
	nlitBits, nlitSpan, err := p.r.bits(5)
	if err != nil {
		return nil, nil, nil, nil, p.fail(start, "reading HLIT: %v", err)
	}
	ndistBits, ndistSpan, err := p.r.bits(5)
	if err != nil {
		return nil, nil, nil, nil, p.fail(start, "reading HDIST: %v", err)
	}
	nclenBits, nclenSpan, err := p.r.bits(4)
	if err != nil {
		return nil, nil, nil, nil, p.fail(start, "reading HCLEN: %v", err)
	}
	nlit := int(nlitBits) + 257
	ndist := int(ndistBits) + 1
	nclen := int(nclenBits) + 4
	if nlit > 286 || ndist > 32 {
		return nil, nil, nil, nil, p.fail(start, "invalid dynamic table sizes HLIT=%d HDIST=%d", nlit, ndist)
	}
	header = &DynamicHeader{
		NumLiteralLengths: nlit, NumDistanceLengths: ndist, NumCodeLengths: nclen,
		HLIT: nlitSpan, HDIST: ndistSpan, HCLEN: nclenSpan,
	}

	codeLengths := make([]uint8, 19)
	for i := 0; i < nclen; i++ {
		v, span, readErr := p.r.bits(3)
		if readErr != nil {
			return nil, nil, nil, nil, p.fail(p.r.position(), "reading code-length alphabet: %v", readErr)
		}
		symbol := codeOrder[i]
		codeLengths[symbol] = uint8(v)
		header.CodeLengths = append(header.CodeLengths, CodeLengthField{Symbol: symbol, Length: uint8(v), Input: span})
	}
	code, err = buildTable(codeLengths)
	if err != nil {
		return nil, nil, nil, nil, p.fail(start, "building code-length table: %v", err)
	}
	if code.max == 0 {
		return nil, nil, nil, nil, p.fail(start, "empty code-length table")
	}

	lengths := make([]uint8, 0, nlit+ndist)
	for len(lengths) < cap(lengths) {
		symbol, span, decodeErr := code.decode(p.r)
		if decodeErr != nil {
			return nil, nil, nil, nil, p.fail(span.Start, "decoding code length: %v", decodeErr)
		}
		run := CodeLengthRun{Symbol: symbol, SlotsStart: len(lengths), SymbolBits: span, Input: span}
		switch {
		case symbol < 16:
			run.Length, run.Repeat = uint8(symbol), 1
			lengths = append(lengths, uint8(symbol))
		case symbol == 16:
			if len(lengths) == 0 {
				return nil, nil, nil, nil, p.fail(span.Start, "code 16 has no previous length")
			}
			extra, extraSpan, readErr := p.r.bits(2)
			if readErr != nil {
				return nil, nil, nil, nil, p.fail(p.r.position(), "reading code-16 repeat: %v", readErr)
			}
			run.Length, run.Repeat, run.ExtraBits = lengths[len(lengths)-1], int(extra)+3, extraSpan
			for range run.Repeat {
				lengths = append(lengths, lengths[len(lengths)-1])
			}
		case symbol == 17:
			extra, extraSpan, readErr := p.r.bits(3)
			if readErr != nil {
				return nil, nil, nil, nil, p.fail(p.r.position(), "reading code-17 repeat: %v", readErr)
			}
			run.Repeat, run.ExtraBits = int(extra)+3, extraSpan
			lengths = append(lengths, make([]uint8, run.Repeat)...)
		case symbol == 18:
			extra, extraSpan, readErr := p.r.bits(7)
			if readErr != nil {
				return nil, nil, nil, nil, p.fail(p.r.position(), "reading code-18 repeat: %v", readErr)
			}
			run.Repeat, run.ExtraBits = int(extra)+11, extraSpan
			lengths = append(lengths, make([]uint8, run.Repeat)...)
		default:
			return nil, nil, nil, nil, p.fail(span.Start, "invalid code-length symbol %d", symbol)
		}
		run.SlotsEnd = len(lengths)
		run.Input.End = p.r.position()
		header.LengthRuns = append(header.LengthRuns, run)
		if len(lengths) > cap(lengths) {
			return nil, nil, nil, nil, p.fail(span.Start, "code lengths exceed HLIT+HDIST")
		}
	}

	lit, err = buildTable(lengths[:nlit])
	if err != nil {
		return nil, nil, nil, nil, p.fail(start, "building literal/length table: %v", err)
	}
	if lengths[256] == 0 {
		return nil, nil, nil, nil, p.fail(start, "literal/length table has no end-of-block symbol")
	}
	dist, err = buildTable(lengths[nlit:])
	if err != nil {
		return nil, nil, nil, nil, p.fail(start, "building distance table: %v", err)
	}
	return lit, dist, code, header, nil
}

func isUnexpectedEOF(err error) bool {
	return err == io.EOF || err == io.ErrUnexpectedEOF
}
