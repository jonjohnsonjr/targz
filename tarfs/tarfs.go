// Copyright 2023 Chainguard, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package tarfs

import (
	"archive/tar"
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"strings"
	"testing/iotest"
	"time"

	"slices"
)

var emptyReader = iotest.ErrReader(io.EOF)

type Entry struct {
	Header tar.Header
	Offset int64

	dir string
	fi  fs.FileInfo
}

func (e Entry) Name() string {
	return e.fi.Name()
}

func (e Entry) Size() int64 {
	return e.Header.Size
}

func (e Entry) Type() fs.FileMode {
	return e.fi.Mode()
}

func (e Entry) Info() (fs.FileInfo, error) {
	return e.fi, nil
}

func (e Entry) IsDir() bool {
	return e.fi.IsDir()
}

type File struct {
	Entry *Entry

	fsys *FS
	sr   *io.SectionReader
}

func (f *File) Stat() (fs.FileInfo, error) {
	return f.Entry.fi, nil
}

func (f *File) Read(p []byte) (int, error) {
	return f.sr.Read(p)
}

func (f *File) ReadAt(p []byte, off int64) (int, error) {
	return f.sr.ReadAt(p, off)
}

func (f *File) Seek(offset int64, whence int) (int64, error) {
	return f.sr.Seek(offset, whence)
}

func (f *File) Close() error {
	return nil
}

// TODO: Respect n.
func (f *File) ReadDir(n int) ([]fs.DirEntry, error) {
	return f.fsys.ReadDir(f.Entry.Header.Name)
}

type FS struct {
	ra    io.ReaderAt
	files []*Entry
	index map[string]int
}

func (fsys *FS) Readlink(name string) (string, error) {
	i, ok := fsys.index[name]
	if !ok {
		return "", fs.ErrNotExist
	}

	e := fsys.files[i]

	switch e.Header.Typeflag {
	case tar.TypeSymlink, tar.TypeLink:
		return e.Header.Linkname, nil
	}

	return "", fmt.Errorf("Readlink(%q): file is not a link", name)
}

// Open implements fs.FS.
func (fsys *FS) Open(name string) (fs.File, error) {
	if name == "." {
		return &File{
			Entry: &Entry{
				Header: tar.Header{
					Name: ".",
				},
				fi: root{},
			},
			fsys: fsys,
		}, nil
	}

	i, ok := fsys.index[name]
	if !ok {
		return nil, fs.ErrNotExist
	}

	e := fsys.files[i]

	f := &File{
		Entry: e,
		fsys:  fsys,
		// TODO: Use SectionOpener if fsys.ra implements it.
		sr: io.NewSectionReader(fsys.ra, e.Offset, e.Header.Size),
	}

	return f, nil
}

func (fsys *FS) Entries() []*Entry {
	return fsys.files
}

type root struct{}

func (r root) Name() string       { return "." }
func (r root) Size() int64        { return 0 }
func (r root) Mode() fs.FileMode  { return fs.ModeDir }
func (r root) ModTime() time.Time { return time.Unix(0, 0) }
func (r root) IsDir() bool        { return true }
func (r root) Sys() any           { return nil }

func (fsys *FS) Stat(name string) (fs.FileInfo, error) {
	if i, ok := fsys.index[name]; ok {
		return fsys.files[i].fi, nil
	}

	// fs.WalkDir expects "." to return a root entry to bootstrap the walk.
	// If we didn't find it above, synthesize one.
	if name == "." {
		return root{}, nil
	}

	return nil, fs.ErrNotExist
}

func (fsys *FS) ReadDir(name string) ([]fs.DirEntry, error) {
	children := []fs.DirEntry{}
	for _, f := range fsys.files {
		// This is load bearing for now.
		f := f

		if f.dir != name {
			continue
		}

		children = append(children, f)
	}

	slices.SortFunc(children, func(a, b fs.DirEntry) int {
		return strings.Compare(a.Name(), b.Name())
	})

	return children, nil
}

type countReader struct {
	r io.Reader
	n int64
}

func (cr *countReader) Read(p []byte) (int, error) {
	n, err := cr.r.Read(p)
	cr.n += int64(n)
	return n, err
}

func New(ra io.ReaderAt) (*FS, error) {
	fsys := &FS{
		ra:    ra,
		files: []*Entry{},
		index: map[string]int{},
	}

	var r io.Reader
	if reader, ok := ra.(io.Reader); ok {
		r = reader
	} else {
		size := int64(-1)
		if statter, ok := ra.(interface {
			Stat() (fs.FileInfo, error)
		}); ok {
			stat, err := statter.Stat()
			if err != nil {
				return nil, err
			}
			size = stat.Size()
		}
		r = io.NewSectionReader(ra, 0, size)
	}

	cr := &countReader{bufio.NewReaderSize(r, 1<<20), 0}
	tr := tar.NewReader(cr)

	// TODO: Do this lazily.
	// TODO: Allow this to be saved and restored.
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		fsys.index[hdr.Name] = len(fsys.files)
		fsys.files = append(fsys.files, &Entry{
			Header: *hdr,
			Offset: cr.n,
			dir:    path.Dir(hdr.Name),
			fi:     hdr.FileInfo(),
		})
	}

	return fsys, nil
}
