package main

import (
	"bytes"
	"context"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/jonjohnsonjr/targz/gsip"
	"github.com/jonjohnsonjr/targz/ranger"
	"github.com/jonjohnsonjr/targz/tarfs"
)

func TestTargz(t *testing.T) {
	f, err := os.Open("./tarfs/testdata/gsip.tar")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}

	size := info.Size()

	s := httptest.NewServer(http.FileServerFS(os.DirFS("./tarfs/testdata")))
	defer s.Close()

	uri := s.URL + "/gsip.tar.gz"

	t.Logf("uri: %q", uri)

	ra := ranger.New(context.Background(), uri, s.Client().Transport)

	zr, err := gsip.NewReader(ra, size)
	if err != nil {
		t.Fatal(err)
	}

	hfs, err := tarfs.New(zr, size)
	if err != nil {
		t.Fatal(err)
	}

	ffs, err := tarfs.New(f, size)
	if err != nil {
		t.Fatal(err)
	}

	if err := fs.WalkDir(ffs, ".", func(p string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		want, err := ffs.Open(p)
		if err != nil {
			t.Fatal(err)
		}

		got, err := hfs.Open(p)
		if err != nil {
			t.Fatal(err)
		}

		b1, err := io.ReadAll(want)
		if err != nil {
			t.Fatal(err)
		}

		b2, err := io.ReadAll(got)
		if err != nil {
			t.Fatal(err)
		}

		if !bytes.Equal(b1, b2) {
			t.Errorf("mismatched contents: %q", p)
		}

		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
