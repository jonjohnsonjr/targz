package lsp

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"math"
	"os"
	"path/filepath"
	"testing"
)

type countingReaderAt struct {
	io.ReaderAt
	min int64
	n   int64
}

func (r *countingReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if off < r.min {
		r.min = off
	}
	n, err := r.ReaderAt.ReadAt(p, off)
	r.n += int64(n)
	return n, err
}

func TestTarCatalogPersistsRestartCheckpoints(t *testing.T) {
	t.Setenv("GZPLS_CACHE_DIR", t.TempDir())
	source := filepath.Join(t.TempDir(), "large.tar.gz")
	f, err := os.Create(source)
	if err != nil {
		t.Fatal(err)
	}
	zw, err := gzip.NewWriterLevel(f, gzip.BestSpeed)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(zw)
	payload := bytes.Repeat([]byte("checkpoint-friendly payload\n"), 1<<17)
	for i := 0; i < 4; i++ {
		if err := tw.WriteHeader(&tar.Header{Name: filepath.ToSlash(filepath.Join("layers", string(rune('a'+i)), "data.txt")), Mode: 0o644, Size: int64(len(payload))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(payload); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	entries, ok, err := scanTarFile(source, false)
	if err != nil || !ok || len(entries) != 4 {
		t.Fatalf("scanTarFile = %d entries, %v, %v", len(entries), ok, err)
	}
	revision, err := quickFileRevision(source)
	if err != nil {
		t.Fatal(err)
	}
	catalog, ok := loadTarCatalog(revision, false)
	if !ok || len(catalog.GSIPIndex) == 0 {
		t.Fatalf("persistent catalog = %#v, %v", catalog, ok)
	}

	archive, err := os.Open(source)
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	info, err := archive.Stat()
	if err != nil {
		t.Fatal(err)
	}
	counted := &countingReaderAt{ReaderAt: archive, min: math.MaxInt64}
	var extracted bytes.Buffer
	last := entries[len(entries)-1]
	if err := copyIndexedTarEntryAt(counted, info.Size(), catalog.GSIPIndex, last, &extracted); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(extracted.Bytes(), payload) {
		t.Fatalf("extracted %d bytes, want %d", extracted.Len(), len(payload))
	}
	if counted.min == 0 {
		t.Fatal("late member extraction restarted at compressed offset zero")
	}

	cachePath, err := tarCatalogPath(revision, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cachePath, []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if rebuilt, ok, err := scanTarFile(source, false); err != nil || !ok || len(rebuilt) != len(entries) {
		t.Fatalf("rebuild corrupt cache = %d entries, %v, %v", len(rebuilt), ok, err)
	}
	if _, ok := loadTarCatalog(revision, false); !ok {
		t.Fatal("rebuilt catalog was not persisted")
	}
}
