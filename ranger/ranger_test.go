package ranger

import (
	"context"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

func TestRanger(t *testing.T) {
	s := httptest.NewServer(http.FileServerFS(os.DirFS("./testdata")))
	defer s.Close()

	uri := s.URL + "/gsip.tar"

	t.Logf("uri: %q", uri)

	ra := New(context.Background(), uri, s.Client().Transport)

	f, err := os.Open("./testdata/gsip.tar")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}

	size := info.Size()

	// TODO: Pull this out into a test package.
	for range 100 {
		start := rand.Int64N(size)
		length := rand.Int64N(size - start)

		if length == 0 {
			continue
		}

		b := make([]byte, length)
		zb := make([]byte, length)

		n, err := f.ReadAt(b, start)
		zn, zerr := ra.ReadAt(zb, start)

		if err != zerr {
			t.Fatalf("ReadAt(%d, %d): %v != %v", start, len(b), err, zerr)
		}

		if n != zn {
			t.Fatalf("ReadAt(%d, %d): %d != %d", start, len(b), n, zn)
		}
	}
}
