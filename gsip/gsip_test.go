package gsip

import (
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
