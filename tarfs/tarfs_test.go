package tarfs

import (
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
