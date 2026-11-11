package deflate

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"fmt"
	"hash/crc32"
)

// RewriteLiteral changes one literal and every output byte transitively copied
// from it, then emits a new stream. Gzip headers and unaffected members are
// preserved byte-for-byte; affected members receive a freshly encoded raw
// DEFLATE body and a new CRC32/ISIZE trailer.
//
// The encoder intentionally preserves output semantics rather than the
// original block and Huffman layout. A subsequent parse therefore describes
// the newly encoded instruction stream, not a patched version of the old one.
func (t *Trace) RewriteLiteral(opID int, replacement byte) ([]byte, RenameImpact, error) {
	impact, affected, err := t.renameImpact(opID, replacement)
	if err != nil {
		return nil, RenameImpact{}, err
	}
	output := append([]byte(nil), t.Output...)
	for i, yes := range affected {
		if yes {
			output[i] = replacement
		}
	}

	if len(t.Members) == 0 {
		encoded, err := encodeRaw(output)
		return encoded, impact, err
	}

	var encoded bytes.Buffer
	for _, member := range t.Members {
		changed := false
		for pos := member.Output.Start; pos < member.Output.End; pos++ {
			if affected[pos] {
				changed = true
				break
			}
		}
		if !changed {
			start, end := byteBounds(member.Input)
			encoded.Write(t.input[start:end])
			continue
		}

		headerStart, headerEnd := byteBounds(member.Header)
		encoded.Write(t.input[headerStart:headerEnd])
		payload := output[member.Output.Start:member.Output.End]
		body, err := encodeRaw(payload)
		if err != nil {
			return nil, RenameImpact{}, fmt.Errorf("encode gzip member %d: %w", member.ID, err)
		}
		encoded.Write(body)
		var trailer [8]byte
		binary.LittleEndian.PutUint32(trailer[:4], crc32.ChecksumIEEE(payload))
		binary.LittleEndian.PutUint32(trailer[4:], uint32(len(payload)))
		encoded.Write(trailer[:])
	}
	return encoded.Bytes(), impact, nil
}

func encodeRaw(output []byte) ([]byte, error) {
	var encoded bytes.Buffer
	w, err := flate.NewWriter(&encoded, flate.DefaultCompression)
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(output); err != nil {
		_ = w.Close()
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return encoded.Bytes(), nil
}

func byteBounds(r BitRange) (int, int) {
	return int(uint64(r.Start) / 8), int(uint64(r.End) / 8)
}
