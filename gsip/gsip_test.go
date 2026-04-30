package gsip

import (
	"bytes"
	"compress/gzip"
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
