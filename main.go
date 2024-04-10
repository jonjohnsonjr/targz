package main

import (
	"bytes"
	ogzip "compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"

	"github.com/jonjohnsonjr/targz/gsip"
	"github.com/jonjohnsonjr/targz/tarfs"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		log.Fatal(err)
	}
}

func run(args []string) error {
	f, err := os.Open(args[0])
	if err != nil {
		return err
	}

	info, err := f.Stat()
	if err != nil {
		return err
	}

	zr, err := gsip.NewReader(f, info.Size())
	if err != nil {
		return err
	}

	f2, err := os.Open(args[0])
	if err != nil {
		return err
	}

	tmp, err := os.CreateTemp("", "")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())

	gzr, err := ogzip.NewReader(f2)
	if err != nil {
		return err
	}

	size, err := io.Copy(tmp, gzr)
	if err != nil {
		return err
	}

	if _, err := tmp.Seek(0, 0); err != nil {
		return err
	}

	// To populate stuff.
	fsys, err := tarfs.New(zr)
	if err != nil {
		return err
	}

	if len(args) == 1 {
		fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
			return nil
		})
	}

	// return http.ListenAndServe(args[1], http.FileServer(http.FS(fsys)))

	return compare(zr, tmp, size)
}

func compare(lhs, rhs io.ReaderAt, size int64) error {
	stride := int64(1024 * 32)
	// bad := int64(49577984)
	// bad := int64(50090255)
	bad := int64(37769517)
	for offset := bad; offset >= 0; offset = max(0, offset-stride) {
		b1 := make([]byte, stride)
		b2 := make([]byte, stride)

		log.Printf("reading at %d", offset)
		n1, err1 := lhs.ReadAt(b1, offset)
		n2, err2 := rhs.ReadAt(b2, offset)

		if err1 != err2 {
			return fmt.Errorf("diff error: %w", errors.Join(err1, err2))
		}

		if n1 != n2 {
			return fmt.Errorf("diff n: %d != %d", n1, n2)
		}

		if !bytes.Equal(b1, b2) {
			return fmt.Errorf("diff bytes")
		}

		if offset == 0 {
			break
		}
	}

	return nil
}
