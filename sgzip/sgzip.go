package sgzip

import (
	"fmt"
	"io"
)

type index struct {
	// TODO: Checkpoints.
}

type Reader struct {
	index *index
}

func NewReader(rs io.ReadSeeker) (*Reader, error) {
	return nil, fmt.Errorf("todo: Read()")
}

func (r *Reader) Read(p []byte) (int, error) {
	return 0, fmt.Errorf("todo: Read()")
}

func (r *Reader) Seek(offset int64, whence int) (int64, error) {
	return 0, fmt.Errorf("todo: Seek()")
}
