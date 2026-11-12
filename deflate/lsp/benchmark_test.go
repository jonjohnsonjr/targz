package lsp

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func benchmarkTarGzip(b *testing.B) string {
	b.Helper()
	path := filepath.Join(b.TempDir(), "fixture.tar.gz")
	var compressed bytes.Buffer
	zw, err := gzip.NewWriterLevel(&compressed, gzip.BestSpeed)
	if err != nil {
		b.Fatal(err)
	}
	tw := tar.NewWriter(zw)
	payload := make([]byte, 4096)
	for i := range payload {
		// Mostly incompressible payloads keep the parser honest while repeated
		// chunks still exercise match provenance.
		payload[i] = byte((i*131 + i/7) & 0xff)
	}
	for i := 0; i < 2048; i++ {
		name := fmt.Sprintf("layer-%02d/pkg-%04d/data.bin", i%16, i)
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(payload))}); err != nil {
			b.Fatal(err)
		}
		if _, err := tw.Write(payload); err != nil {
			b.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		b.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		b.Fatal(err)
	}
	if err := os.WriteFile(path, compressed.Bytes(), 0o600); err != nil {
		b.Fatal(err)
	}
	b.Logf("fixture: %d compressed bytes, %d output bytes", compressed.Len(), 2048*(4096+512)+1024)
	return path
}

func BenchmarkDisassembleTar(b *testing.B) {
	source := benchmarkTarGzip(b)
	data, err := os.ReadFile(source)
	if err != nil {
		b.Fatal(err)
	}
	trace, err := parse(data, false)
	if err != nil {
		b.Fatal(err)
	}
	b.Logf("trace: %d operations, %d blocks", len(trace.Ops), len(trace.Blocks))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := DisassembleFileView(source, false, "tar"); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkParseTarGzip(b *testing.B) {
	source := benchmarkTarGzip(b)
	data, err := os.ReadFile(source)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := parse(data, false); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkOpenTarDocument(b *testing.B) {
	source := benchmarkTarGzip(b)
	listing, err := DisassembleFileView(source, false, "tar")
	if err != nil {
		b.Fatal(err)
	}
	uri := fileURI(source + ".deflate")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		doc := openDocument(uri, string(listing))
		if doc.loadErr != nil {
			b.Fatal(doc.loadErr)
		}
	}
}
