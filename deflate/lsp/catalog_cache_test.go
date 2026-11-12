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

	entries, ok, index, err := buildTarCatalog(source, false)
	if err != nil || !ok || len(entries) != 4 {
		t.Fatalf("buildTarCatalog = %d entries, %v, %v", len(entries), ok, err)
	}
	revision, err := quickFileRevision(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := saveTarCatalog(revision, false, entries, index); err != nil {
		t.Fatal(err)
	}
	loadedIndex, ok := loadGSIPIndex(revision, false)
	if !ok || len(loadedIndex) == 0 {
		t.Fatalf("persistent checkpoint index = %d bytes, %v", len(loadedIndex), ok)
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
	if err := copyIndexedTarEntryAt(counted, info.Size(), loadedIndex, last, &extracted); err != nil {
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
	if rebuilt, _, ok, err := scanTarDirectory(source, false, ""); err != nil || !ok || len(rebuilt) == 0 {
		t.Fatalf("rebuild corrupt cache = %d root entries, %v, %v", len(rebuilt), ok, err)
	}
	if _, _, ok := loadTarDirectory(revision, false, ""); !ok {
		t.Fatal("rebuilt catalog was not persisted")
	}
}
