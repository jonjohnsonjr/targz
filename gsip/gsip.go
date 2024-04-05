package gsip

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"

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
	readers     []*gzip.Reader
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
		readers:     []*gzip.Reader{},
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
		return nil, err
	}

	r := &Reader{
		ra:          ra,
		size:        size,
		updates:     updates,
		checkpoints: []*flate.Checkpoint{},
		readers:     []*gzip.Reader{zr},
	}

	// TODO: Locking around this to make sure it's safe.
	// TODO: Make sure we don't leak this goroutine.
	go func() {
		for checkpoint := range updates {
			r.checkpoints = append(r.checkpoints, checkpoint)
		}
	}()

	return r, nil
}

func (r *Reader) findReader(off int64) (io.Reader, error) {
	// TODO: Appropriate locking around this for concurrency.
	// TODO: Even if we don't find an exact match, one of these might be reusable.
	// TODO: Consider a fixed size pool of these that signal they're done via Close().
	for _, zr := range r.readers {
		if zr.Offset() == off {
			return zr, nil
		}
	}

	var highest *flate.Checkpoint
	for _, checkpoint := range r.checkpoints {
		if checkpoint.Out > off {
			break
		}

		highest = checkpoint
	}

	if highest == nil {
		return nil, fmt.Errorf("no checkpoints available, is this a real gzip archive?")
	}

	// TODO: Do we need to bound the size?
	sr := io.NewSectionReader(r.ra, highest.In, r.size-highest.In)

	zr, err := gzip.Continue(sr, 0, highest, nil)
	if err != nil {
		return nil, fmt.Errorf("continue: %w", err)
	}

	// TODO: Make sure this doesn't send a bunch of tiny ReadAts.
	discard := off - highest.Out
	if _, err := io.CopyN(io.Discard, zr, discard); err != nil {
		return nil, err
	}

	r.readers = append(r.readers, zr)

	return zr, nil
}

func (r *Reader) ReadAt(p []byte, off int64) (int, error) {
	zr, err := r.findReader(off)
	if err != nil {
		return 0, err
	}

	return io.ReadFull(zr, p)
}
