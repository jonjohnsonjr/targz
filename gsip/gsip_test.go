package gsip

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"testing"
)

func TestGsip(t *testing.T) {
	f, err := os.Open("./testdata/Mark.Twain-Tom.Sawyer.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	finfo, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}

	size := finfo.Size()

	zf, err := os.Open("./testdata/Mark.Twain-Tom.Sawyer.txt.gz")
	if err != nil {
		t.Fatal(err)
	}
	defer zf.Close()

	gsip, err := NewReader(zf, size)
	if err != nil {
		t.Fatal(err)
	}

	for range 100 {
		start := rand.Int64N(size)
		end := rand.Int64N(size-start) + start

		b := make([]byte, end-start)
		zb := make([]byte, end-start)

		n, err := f.ReadAt(b, start)
		zn, zerr := gsip.ReadAt(zb, start)

		if err != zerr {
			t.Fatalf("ReadAt(%d, %d): %v != %v", start, len(b), err, zerr)
		}

		if n != zn {
			t.Fatalf("ReadAt(%d, %d): %d != %d", start, len(b), n, zn)
		}
	}
}

// TestReadAtShortReadAtEOF asserts that a ReadAt whose buffer extends past
// the end of the decompressed stream returns the partial bytes plus
// io.EOF, not (0, io.ErrUnexpectedEOF). This honors the io.ReaderAt
// contract; without it, downstream wrappers (io.SectionReader, bufio
// inside tar.Reader, etc.) silently lose the tail of the stream.
func TestReadAtShortReadAtEOF(t *testing.T) {
	const want = "hello, world"
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	if _, err := gw.Write([]byte(want)); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}

	r, err := NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatal(err)
	}

	// Ask for more decompressed bytes than exist.
	p := make([]byte, 1024)
	n, err := r.ReadAt(p, 0)
	if n != len(want) {
		t.Errorf("n = %d, want %d", n, len(want))
	}
	if err != io.EOF {
		t.Errorf("err = %v, want io.EOF", err)
	}
	if string(p[:n]) != want {
		t.Errorf("got %q, want %q", p[:n], want)
	}
}

// strictReaderAt is an io.ReaderAt that errors loudly if asked to read
// past the size of its underlying buffer. Real-world Range-capable
// transports (e.g. registry blob endpoints) return 416 in that case.
type strictReaderAt struct {
	data []byte
	t    *testing.T
}

func (s *strictReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 || off+int64(len(p)) > int64(len(s.data)) {
		err := fmt.Errorf("ReadAt past end of stream: off=%d len=%d size=%d", off, len(p), len(s.data))
		s.t.Error(err)
		return 0, err
	}
	return copy(p, s.data[off:off+int64(len(p))]), nil
}

// TestAcquireReaderRespectsBlobBounds asserts that ReadAt via the
// checkpoint path (acquireReader's "no exact-match reader, find highest
// checkpoint <= off" branch) doesn't ask the underlying reader for bytes
// past the blob's end. The internal SectionReader's third arg is a
// length, not an absolute end — passing total size as the length lets
// downstream reads spill past the blob.
func TestAcquireReaderRespectsBlobBounds(t *testing.T) {
	// Enough plaintext to ensure multiple flate-block checkpoints.
	plaintext := bytes.Repeat([]byte("Lorem ipsum dolor sit amet, consectetur adipiscing elit. "), 50000)
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	if _, err := gw.Write(plaintext); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}

	sra := &strictReaderAt{data: buf.Bytes(), t: t}
	r, err := NewReader(sra, int64(buf.Len()))
	if err != nil {
		t.Fatal(err)
	}

	// Sequentially drain the stream so the frontier reader emits all
	// flate-block checkpoints.
	chunk := make([]byte, 1<<16)
	for off := int64(0); off < int64(len(plaintext)); {
		n, err := r.ReadAt(chunk, off)
		if err != nil && err != io.EOF {
			t.Fatalf("sequential drain at %d: %v", off, err)
		}
		off += int64(n)
		if n == 0 {
			break
		}
	}

	// Now ReadAt at an offset the frontier reader has already passed.
	// acquireReader must take the checkpoint path and must not request
	// bytes past the blob's end.
	target := make([]byte, 64)
	targetOff := int64(len(plaintext) - 200)
	n, err := r.ReadAt(target, targetOff)
	if err != nil && err != io.EOF {
		t.Fatalf("checkpoint ReadAt: %v", err)
	}
	if n != len(target) {
		t.Errorf("n = %d, want %d", n, len(target))
	}
	if !bytes.Equal(target[:n], plaintext[targetOff:targetOff+int64(n)]) {
		t.Errorf("content mismatch at offset %d", targetOff)
	}
}
