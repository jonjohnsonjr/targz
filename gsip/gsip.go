package gsip

import (
	"fmt"
	"io"

	"github.com/jonjohnsonjr/targz/gsip/internal/flate"
	"github.com/jonjohnsonjr/targz/gsip/internal/gzip"
	"golang.org/x/sync/errgroup"
)

type index struct {
	// TODO: Checkpoints.
}

type Reader struct {
	ra          io.ReaderAt
	size        int64
	zr          *gzip.Reader
	index       *index
	updates     chan *flate.Checkpoint
	checkpoints []*flate.Checkpoint
	readers     []*gzip.Reader
}

func NewReader(ra io.ReaderAt, size int64) (*Reader, error) {
	updates := make(chan *flate.Checkpoint, 10)

	// This is the first pass reader.
	// We should do something more clever, but let's start with just discarding through this.
	// This will give us all the checkpoints we want without touching the gzip package.
	sr := io.NewSectionReader(ra, 0, size)

	zr, err := gzip.NewReader(sr, updates)
	if err != nil {
		return nil, err
	}

	r := &Reader{
		ra:          ra,
		size:        size,
		zr:          zr,
		updates:     updates,
		checkpoints: []*flate.Checkpoint{},
		readers:     []*gzip.Reader{},
	}

	var eg errgroup.Group
	eg.Go(func() error {
		defer close(updates)

		_, err := io.Copy(io.Discard, zr)
		return err
	})
	eg.Go(func() error {
		for checkpoint := range updates {
			r.checkpoints = append(r.checkpoints, checkpoint)
		}

		return nil
	})

	if err := eg.Wait(); err != nil {
		return nil, err
	}

	return r, nil
}

func (r *Reader) ReadAt(p []byte, off int64) (int, error) {
	// TODO: Appropriate locking around this for concurrency.
	for _, zr := range r.readers {
		if zr.Offset() == off {
			return io.ReadFull(zr, p)
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
		return 0, fmt.Errorf("what does it mean if there's no highest???")
	}

	// TODO: minimize the size based on other checkpoints.
	sr := io.NewSectionReader(r.ra, highest.In, r.size)

	// TODO: Less janky!
	zr, err := gzip.Continue(sr, 0, highest, nil)
	if err != nil {
		return 0, fmt.Errorf("continue: %w", err)
	}

	// TODO: limited reader?
	discard := off - highest.Out
	if _, err := io.CopyN(io.Discard, zr, discard); err != nil {
		return 0, err
	}

	r.readers = append(r.readers, zr)

	return io.ReadFull(zr, p)
}
