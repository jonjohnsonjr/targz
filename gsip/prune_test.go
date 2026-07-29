package gsip

import (
	"bytes"
	"compress/gzip"
	"io"
	"testing"
)

// gzipData compresses data with the stdlib gzip writer.
func gzipData(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	if _, err := gw.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// drainReader sequentially reads all plaintext bytes through r (building the
// checkpoint index as a side-effect) and then calls Wait.
func drainReader(t *testing.T, r *Reader, size int64) {
	t.Helper()
	chunk := make([]byte, 1<<14)
	for off := int64(0); off < size; {
		n, err := r.ReadAt(chunk, off)
		off += int64(n)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("drainReader at %d: %v", off, err)
		}
	}
	r.Wait()
}

// checkReadAt verifies that r.ReadAt returns the expected bytes at several
// representative offsets across the plaintext.
func checkReadAt(t *testing.T, r *Reader, plaintext []byte) {
	t.Helper()
	size := int64(len(plaintext))
	for _, off := range []int64{0, size / 4, size / 2, size - 64} {
		if off < 0 {
			off = 0
		}
		p := make([]byte, 64)
		n, err := r.ReadAt(p, off)
		if err != nil && err != io.EOF {
			t.Fatalf("ReadAt(%d): %v", off, err)
		}
		if !bytes.Equal(p[:n], plaintext[off:off+int64(n)]) {
			t.Errorf("ReadAt(%d): content mismatch", off)
		}
	}
}

// TestPruneReducesHist verifies that:
//   - Before Prune, every non-empty checkpoint stores the full 32 KiB window.
//   - After Prune, non-empty checkpoints have a shorter window (proving that
//     pending + lookback bytes were computed and applied).
//   - ReadAt remains correct after pruning (exercising the linear-tail restore
//     path in Continue).
//
// The 12-byte repeating pattern keeps all back-reference distances at ≤ 12.
// The compressed blob is ~518 bytes → 1 deflate block → 1 non-empty checkpoint
// (fired when the span is exceeded).  Prune trims that checkpoint's Hist from
// 32 KiB to (pending + lookback) bytes, well under the original 32 KiB.
func TestPruneReducesHist(t *testing.T) {
	plaintext := bytes.Repeat([]byte("hello world "), 20000) // ~240 KiB
	compressed := gzipData(t, plaintext)

	// span=64 KiB causes a checkpoint to fire at the deflate block boundary.
	r, err := newReaderWithSpan(bytes.NewReader(compressed), int64(len(compressed)), 1<<16)
	if err != nil {
		t.Fatal(err)
	}
	drainReader(t, r, int64(len(plaintext)))

	nonEmpty := 0
	for _, cp := range r.checkpoints {
		if cp.IsEmpty() {
			continue
		}
		nonEmpty++
		if got := len(cp.History()); got != 1<<15 {
			t.Errorf("before Prune: checkpoint Hist len = %d, want 32768", got)
		}
	}
	if nonEmpty < 1 {
		t.Fatalf("need at least 1 non-empty checkpoint, got 0")
	}

	r.Prune()

	anyPruned := false
	for _, cp := range r.checkpoints {
		if !cp.IsEmpty() && len(cp.History()) < 1<<15 {
			anyPruned = true
			break
		}
	}
	if !anyPruned {
		t.Error("Prune did not reduce any checkpoint's Hist")
	}

	checkReadAt(t, r, plaintext)
}

// TestPruneEncodeDecodeRoundtrip verifies that Prune + Encode + Decode
// preserves ReadAt correctness through the full serialization cycle.
// This exercises the linear-tail restore path in Continue on a decoded index.
func TestPruneEncodeDecodeRoundtrip(t *testing.T) {
	plaintext := bytes.Repeat([]byte("hello world "), 20000)
	compressed := gzipData(t, plaintext)

	r, err := newReaderWithSpan(bytes.NewReader(compressed), int64(len(compressed)), 1<<16)
	if err != nil {
		t.Fatal(err)
	}
	drainReader(t, r, int64(len(plaintext)))
	r.Prune()

	var idx bytes.Buffer
	if err := r.Encode(&idx); err != nil {
		t.Fatal(err)
	}

	r2, err := Decode(bytes.NewReader(compressed), int64(len(compressed)), &idx)
	if err != nil {
		t.Fatal(err)
	}
	checkReadAt(t, r2, plaintext)
}

// TestPruneNoopOnDecode verifies that calling Prune on a Decode-path reader
// is safe (the goroutineDone guard fires) and does not corrupt ReadAt results.
func TestPruneNoopOnDecode(t *testing.T) {
	plaintext := bytes.Repeat([]byte("hello world "), 20000)
	compressed := gzipData(t, plaintext)

	r, err := newReaderWithSpan(bytes.NewReader(compressed), int64(len(compressed)), 1<<16)
	if err != nil {
		t.Fatal(err)
	}
	drainReader(t, r, int64(len(plaintext)))

	var idx bytes.Buffer
	if err := r.Encode(&idx); err != nil {
		t.Fatal(err)
	}

	r2, err := Decode(bytes.NewReader(compressed), int64(len(compressed)), &idx)
	if err != nil {
		t.Fatal(err)
	}
	r2.Prune() // must be a no-op, not a panic or corruption
	checkReadAt(t, r2, plaintext)
}
