package gsip

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"sync"

	"github.com/jonjohnsonjr/targz/gsip/internal/flate"
	"github.com/jonjohnsonjr/targz/gsip/internal/gzip"
)

// Index contains the metadata used by [Reader] to skip around a gzip stream.
// The layout will absolutely change and break you if you depend on it.
type Index struct {
	Checkpoints []*flate.Checkpoint
}

type Reader struct {
	ra          io.ReaderAt
	size        int64
	updates     chan *flate.Checkpoint
	checkpoints []*flate.Checkpoint

	mu            sync.Mutex
	readers       map[*gzip.Reader]bool
	closeOnce     sync.Once
	goroutineDone chan struct{} // closed when the checkpoint goroutine exits; nil for Decode-path readers
}

func (r *Reader) Encode(w io.Writer) error {
	idx := Index{
		Checkpoints: r.checkpoints,
	}

	return json.NewEncoder(w).Encode(&idx)
}

func Decode(ra io.ReaderAt, size int64, index io.Reader) (*Reader, error) {
	idx := Index{}
	if err := json.NewDecoder(index).Decode(&idx); err != nil {
		return nil, err
	}

	return &Reader{
		ra:          ra,
		size:        size,
		checkpoints: idx.Checkpoints,
		readers:     map[*gzip.Reader]bool{},
	}, nil
}

func NewReader(ra io.ReaderAt, size int64) (*Reader, error) {
	updates := make(chan *flate.Checkpoint, 10)

	// This is our first pass frontier reader that sends us updates.
	// We probably need to do something special to make this work in the face of concurrent ReadAt.
	sr := io.NewSectionReader(ra, 0, size)

	// Add a buffered reader to the "frontier" to make sure we read at least 1MB at a time.
	// This avoids sending a ton of tiny http requests when using ranger.
	// TODO: Give callers control over this. Does io.SectionReader.Outer help here?
	// Should we implement an optional bufio.ReaderAt?
	br := bufio.NewReaderSize(sr, 1<<20)

	zr, err := gzip.NewReader(br, updates)
	if err != nil {
		return nil, fmt.Errorf("gzip.NewReader: %w", err)
	}

	r := &Reader{
		ra:          ra,
		size:        size,
		updates:     updates,
		checkpoints: []*flate.Checkpoint{},
		readers:     map[*gzip.Reader]bool{zr: true},
	}

	r.goroutineDone = make(chan struct{})
	go func() {
		defer close(r.goroutineDone)
		for checkpoint := range updates {
			r.mu.Lock()
			r.checkpoints = append(r.checkpoints, checkpoint)
			r.mu.Unlock()
		}
	}()

	return r, nil
}

// newReaderWithSpan is like [NewReader] but lets callers control the minimum
// number of decompressed bytes between checkpoints.  Used in tests to force
// multiple checkpoints over small inputs without reading megabytes of data.
func newReaderWithSpan(ra io.ReaderAt, size, span int64) (*Reader, error) {
	updates := make(chan *flate.Checkpoint, 10)
	sr := io.NewSectionReader(ra, 0, size)
	br := bufio.NewReaderSize(sr, 1<<20)
	zr, err := gzip.NewReaderWithSpans(br, span, updates)
	if err != nil {
		return nil, fmt.Errorf("gzip.NewReaderWithSpans: %w", err)
	}
	r := &Reader{
		ra:          ra,
		size:        size,
		updates:     updates,
		checkpoints: []*flate.Checkpoint{},
		readers:     map[*gzip.Reader]bool{zr: true},
	}
	r.goroutineDone = make(chan struct{})
	go func() {
		defer close(r.goroutineDone)
		for checkpoint := range updates {
			r.mu.Lock()
			r.checkpoints = append(r.checkpoints, checkpoint)
			r.mu.Unlock()
		}
	}()
	return r, nil
}

func (r *Reader) acquireReader(off int64) (*gzip.Reader, error) {
	r.mu.Lock()

	// TODO: Appropriate locking around this for concurrency.
	// TODO: Even if we don't find an exact match, one of these might be reusable.
	// TODO: Consider a fixed size pool of these that signal they're done via Close().
	for zr, ok := range r.readers {
		if ok && zr.Offset() == off {
			r.readers[zr] = false
			r.mu.Unlock()
			return zr, nil
		}
	}

	r.mu.Unlock()

	r.mu.Lock()
	var highest *flate.Checkpoint
	for _, checkpoint := range r.checkpoints {
		if checkpoint.Out > off {
			break
		}
		highest = checkpoint
	}
	r.mu.Unlock()

	if highest == nil {
		// No checkpoints probably means we are trying to ReadAt before we index.
		// Just try to find any reader that isn't already in use (probably the first one).
		r.mu.Lock()

		for zr, ok := range r.readers {
			if !ok {
				continue
			}

			if zr.Offset() > off {
				continue
			}

			r.readers[zr] = false
			r.mu.Unlock()

			discard := off - zr.Offset()
			if _, err := io.CopyN(io.Discard, zr, discard); err != nil {
				return nil, fmt.Errorf("discarding %d bytes: %w", discard, err)
			}

			return zr, nil
		}

		r.mu.Unlock()
		return nil, fmt.Errorf("could not find any checkpoints or readers for offset %d", off)
	}

	// SectionReader's third arg is length, not absolute end. Passing
	// r.size let downstream reads ask the underlying ReaderAt for bytes
	// in [highest.In, highest.In + r.size) — i.e. up to r.size past the
	// blob's end when highest.In > 0. Real Range-capable transports
	// reject those requests with 416.
	sr := io.NewSectionReader(r.ra, highest.In, r.size-highest.In)

	// use the same 1 MiB read-ahead as the frontier reader in NewReader;
	// without this the internal gzip reader falls back to bufio's default
	// 4 KiB buffer, which causes ~256× more ReadAt calls on the underlying
	// ReaderAt for large blobs
	br := bufio.NewReaderSize(sr, 1<<20)

	zr, err := gzip.Continue(br, 0, highest, nil)
	if err != nil {
		return nil, fmt.Errorf("continue: %w", err)
	}

	discard := off - highest.Out
	if _, err := io.CopyN(io.Discard, zr, discard); err != nil {
		return nil, fmt.Errorf("discarding %d bytes: %w", discard, err)
	}

	r.mu.Lock()
	r.readers[zr] = false
	r.mu.Unlock()

	return zr, nil
}

func (r *Reader) ReadAt(p []byte, off int64) (int, error) {
	zr, err := r.acquireReader(off)
	if err != nil {
		return 0, fmt.Errorf("acquireReader at %d: %w", off, err)
	}

	defer func() {
		r.mu.Lock()
		defer r.mu.Unlock()

		r.readers[zr] = true
	}()

	n, err := io.ReadFull(zr, p)
	if err != nil {
		// io.ReaderAt contract: a short read at end-of-stream must return
		// the partial bytes plus io.EOF, not (0, io.ErrUnexpectedEOF). The
		// latter loses data and breaks callers that wrap the Reader in
		// io.SectionReader / bufio.Reader (e.g. tarfs.Index).
		if err == io.ErrUnexpectedEOF || err == io.EOF {
			return n, io.EOF
		}
		return n, fmt.Errorf("ReadFull at %d: %w", off, err)
	}

	return n, nil
}

// Wait blocks until the checkpoint goroutine has fully drained all checkpoints from the scan.  The entire compressed stream must have been read to EOF before calling Wait -- if the frontier reader is still in use, the updates channel is still open and closing it will panic.  Wait is a no-op on readers created by [Decode], which have no goroutine.
func (r *Reader) Wait() {
	if r.goroutineDone == nil {
		return
	}
	r.closeOnce.Do(func() { close(r.updates) })
	<-r.goroutineDone
}

// linearTail extracts the minimum ring-buffer bytes needed to resume
// decompression from cp: all pending bytes [RdPos, WrPos) that will be
// served before new decompression, plus maxDist lookback bytes immediately
// before RdPos for satisfying back-references.  Returns a flat chronological
// slice starting at (RdPos − lookback) in ring order.
// Returns nil when nothing needs storing (pending == 0 and maxDist == 0).
func linearTail(cp *flate.Checkpoint, maxDist int) []byte {
	hist := cp.History()
	wrPos := cp.WrPos
	rdPos := cp.RdPos
	pending := wrPos - rdPos

	// available lookback before rdPos (served history in the ring)
	avail := rdPos
	if cp.Full {
		// full ring: served history = total ring − pending bytes
		avail = len(hist) - pending
	}
	lookback := maxDist
	if lookback > avail {
		lookback = avail
	}

	total := lookback + pending
	if total == 0 {
		return nil
	}

	tail := make([]byte, total)
	start := (rdPos - lookback + len(hist)) % len(hist)
	if start < wrPos {
		copy(tail, hist[start:wrPos])
	} else {
		// tail spans the ring wrap: [start, len(hist)) then [0, wrPos)
		first := copy(tail, hist[start:])
		copy(tail[first:], hist[:wrPos])
	}
	return tail
}

// Prune trims each checkpoint's stored history window to the minimum bytes
// needed to resume decompression: pending ring bytes (served before any new
// decompression) plus the MaxDist lookback for back-references.  Call after
// [Reader.Wait] and before [Reader.Encode].
//
// Prune is a no-op on readers created by [Decode] and on checkpoints where
// no savings are possible (or that have already been pruned).
func (r *Reader) Prune() {
	if r.goroutineDone == nil {
		return // Decode path: MaxDist was never computed during a scan.
	}
	r.mu.Lock()
	checkpoints := make([]*flate.Checkpoint, len(r.checkpoints))
	copy(checkpoints, r.checkpoints)
	r.mu.Unlock()

	for _, cp := range checkpoints {
		if cp.IsEmpty() || len(cp.History()) != 1<<15 {
			continue // empty sentinel or already pruned
		}
		tail := linearTail(cp, cp.MaxDist)
		if tail != nil && len(tail) < len(cp.History()) {
			cp.SetHistory(tail)
		}
	}
}

type reader struct {
	gzip.Reader
}
