package deflate

import (
	"encoding/binary"
	"hash/crc32"
)

const (
	gzipID1       = 0x1f
	gzipID2       = 0x8b
	gzipDeflate   = 8
	gzipFlagHCRC  = 1 << 1
	gzipFlagExtra = 1 << 2
	gzipFlagName  = 1 << 3
	gzipFlagCmnt  = 1 << 4
)

// ParseGzip parses every member in an RFC 1952 gzip stream and validates each
// member's CRC32 and uncompressed size.
func ParseGzip(data []byte) (*Trace, error) {
	r := newBitReader(data)
	// Most gzip streams contain one member, so the trailing ISIZE is a useful
	// allocation hint. Keep the hint bounded: malformed input must not be able
	// to force an arbitrary allocation before its trailer has been validated.
	outputCap := 0
	if len(data) >= 8 {
		outputCap = int(binary.LittleEndian.Uint32(data[len(data)-4:]))
		// Also relate the speculative allocation to bytes actually supplied.
		// Highly compressible valid streams can grow beyond this and will use
		// ordinary slice growth; a tiny bogus trailer cannot allocate 256 MiB.
		hintCap := max(1<<20, len(data)*64)
		outputCap = min(outputCap, min(hintCap, 256<<20))
	}
	// An output-producing instruction generally consumes at least one byte of
	// compressed input. Cap the hint so a very large input does not require its
	// entire operation stream to be resident before parsing begins.
	opCap := min(len(data)-len(data)/4, 1<<20)
	t := &Trace{
		input:  append([]byte(nil), data...),
		Output: make([]byte, 0, outputCap),
		Ops:    make([]Op, 0, opCap),
	}
	p := parser{r: r, trace: t}

	for r.remaining() != 0 {
		memberStart := r.position()
		headerStartByte := uint64(memberStart) / 8
		fixed, _, err := r.bytes(10)
		if err != nil {
			return nil, p.fail(memberStart, "reading gzip header: %v", err)
		}
		if fixed[0] != gzipID1 || fixed[1] != gzipID2 || fixed[2] != gzipDeflate {
			return nil, p.fail(memberStart, "invalid gzip header")
		}
		flags := fixed[3]
		if flags&0xe0 != 0 {
			return nil, p.fail(memberStart+24, "gzip reserved flags are set: %#x", flags)
		}

		member := Member{ID: len(t.Members)}
		if flags&gzipFlagExtra != 0 {
			xlenBytes, _, readErr := r.bytes(2)
			if readErr != nil {
				return nil, p.fail(r.position(), "reading gzip extra length: %v", readErr)
			}
			xlen := int(binary.LittleEndian.Uint16(xlenBytes))
			extra, _, readErr := r.bytes(xlen)
			if readErr != nil {
				return nil, p.fail(r.position(), "reading gzip extra field: %v", readErr)
			}
			member.Extra = append([]byte(nil), extra...)
		}
		if flags&gzipFlagName != 0 {
			name, readErr := readGzipString(r)
			if readErr != nil {
				return nil, p.fail(r.position(), "reading gzip name: %v", readErr)
			}
			member.Name = name
		}
		if flags&gzipFlagCmnt != 0 {
			comment, readErr := readGzipString(r)
			if readErr != nil {
				return nil, p.fail(r.position(), "reading gzip comment: %v", readErr)
			}
			member.Comment = comment
		}
		if flags&gzipFlagHCRC != 0 {
			crcEndByte := uint64(r.position()) / 8
			wantBytes, _, readErr := r.bytes(2)
			if readErr != nil {
				return nil, p.fail(r.position(), "reading gzip header CRC: %v", readErr)
			}
			want := binary.LittleEndian.Uint16(wantBytes)
			got := uint16(crc32.ChecksumIEEE(data[headerStartByte:crcEndByte]))
			if got != want {
				return nil, p.fail(r.position()-16, "gzip header CRC mismatch: got %#04x want %#04x", got, want)
			}
		}

		member.Header = BitRange{Start: memberStart, End: r.position()}
		member.Body.Start = r.position()
		member.Output.Start = OutPos(len(t.Output))
		if err := p.blocks(member.Output.Start); err != nil {
			return nil, err
		}
		member.Body.End = r.position()
		r.alignByte()
		member.Trailer.Start = r.position()
		trailer, _, err := r.bytes(8)
		if err != nil {
			return nil, p.fail(r.position(), "reading gzip trailer: %v", err)
		}
		member.Trailer.End = r.position()
		member.Input = BitRange{Start: memberStart, End: r.position()}
		member.Output.End = OutPos(len(t.Output))
		member.CRC32 = binary.LittleEndian.Uint32(trailer[:4])
		member.ISize = binary.LittleEndian.Uint32(trailer[4:])

		output := t.Output[member.Output.Start:member.Output.End]
		if got := crc32.ChecksumIEEE(output); got != member.CRC32 {
			return nil, p.fail(member.Trailer.Start, "gzip data CRC mismatch: got %#08x want %#08x", got, member.CRC32)
		}
		if got := uint32(len(output)); got != member.ISize {
			return nil, p.fail(member.Trailer.Start+32, "gzip size mismatch: got %d want %d", got, member.ISize)
		}
		t.Members = append(t.Members, member)
	}

	t.consumed = r.position()
	return t, nil
}

func readGzipString(r *bitReader) (string, error) {
	b := make([]byte, 0, 32)
	for {
		c, _, err := r.byte()
		if err != nil {
			return "", err
		}
		if c == 0 {
			return string(b), nil
		}
		b = append(b, c)
	}
}
