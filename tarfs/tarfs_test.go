package tarfs

import (
	"archive/tar"
	"bytes"
	"io/fs"
	"os"
	"testing"
	"testing/fstest"
)

func TestFS(t *testing.T) {
	f, err := os.Open("./testdata/gsip.tar")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	stat, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}

	fsys, err := New(f, stat.Size())
	if err != nil {
		t.Fatal(err)
	}

	if err := fstest.TestFS(fsys,
		"gsip",
		"gsip/internal",
		"gsip/internal/flate",
		"gsip/internal/flate/token.go",
		"gsip/internal/flate/inflate.go",
		"gsip/internal/flate/dict_decoder.go",
		"gsip/internal/flate/huffman_code.go",
		"gsip/internal/gzip",
		"gsip/internal/gzip/gunzip.go",
		"gsip/gsip.go"); err != nil {
		t.Fatal(err)
	}
}

func TestSymlinkedDirs(t *testing.T) {
	buf := &bytes.Buffer{}

	tw := tar.NewWriter(buf)

	want := "pretend this is a binary"

	tw.WriteHeader(&tar.Header{
		Name:     "usr",
		Typeflag: tar.TypeDir,
	})
	tw.WriteHeader(&tar.Header{
		Name:     "usr/bin",
		Typeflag: tar.TypeDir,
	})
	tw.WriteHeader(&tar.Header{
		Name:     "usr/bin/binary",
		Typeflag: tar.TypeReg,
		Size:     int64(len(want)),
	})
	tw.Write([]byte(want))
	tw.WriteHeader(&tar.Header{
		Name:     "weird",
		Typeflag: tar.TypeDir,
	})
	tw.WriteHeader(&tar.Header{
		Name:     "weird/linked",
		Typeflag: tar.TypeSymlink,
		Linkname: "/usr/bin",
	})
	tw.WriteHeader(&tar.Header{
		Name:     "weird/absolute",
		Typeflag: tar.TypeDir,
	})
	tw.WriteHeader(&tar.Header{
		Name:     "weird/absolute/binary",
		Typeflag: tar.TypeSymlink,
		Linkname: "/weird/linked/binary",
	})
	tw.WriteHeader(&tar.Header{
		Name:     "weird/relative",
		Typeflag: tar.TypeDir,
	})
	tw.WriteHeader(&tar.Header{
		Name:     "weird/relative/binary",
		Typeflag: tar.TypeSymlink,
		Linkname: "../linked/binary",
	})

	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	fsys, err := New(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{
		"weird/linked/binary",
		"weird/absolute/binary",
	} {
		if b, err := fs.ReadFile(fsys, name); err != nil {
			t.Fatalf("ReadFile(%q): %v", name, err)
		} else if string(b) != want {
			t.Fatalf("want %q, got %q", want, b)
		}
	}
}
