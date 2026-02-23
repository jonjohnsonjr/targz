package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"testing/fstest"

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

// https://github.com/jonjohnsonjr/targz/issues/2
func TestNoTarTrailer(t *testing.T) {
	buf := &bytes.Buffer{}
	gz := gzip.NewWriter(buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "hi jon"}); err != nil {
		t.Fatal(err)
	}
	// Note: Flush() but not Close().
	if err := tw.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	targz := bytes.NewReader(buf.Bytes())

	sippy, err := gsip.NewReader(targz, targz.Size())
	if err != nil {
		t.Fatal(err)
	}

	fs, err := tarfs.New(sippy, -1)
	if err != nil {
		t.Fatal(err)
	}

	if err := fstest.TestFS(fs, "hi jon"); err != nil {
		t.Fatal(err)
	}
}
