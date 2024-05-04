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
	"cmp"
	"encoding/json"
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

	Filename string
	dir      string
	fi       fs.FileInfo
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
	return f.fsys.ReadDir(f.Entry.Filename)
}

type FS struct {
	ra    io.ReaderAt
	files []*Entry
	index map[string]int
	dirs  map[string][]fs.DirEntry
}

func (fsys *FS) Readlink(name string) (string, error) {
	e, err := fsys.Entry(name)
	if err != nil {
		return "", err
	}

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
				dir:      ".",
				Filename: ".",
				Header: tar.Header{
					Name: ".",
				},
				fi: root{},
			},
			fsys: fsys,
		}, nil
	}

	e, err := fsys.Entry(name)
	if err != nil {
		return nil, err
	}

	f := &File{
		Entry: e,
		fsys:  fsys,
		// TODO: Use SectionOpener if fsys.ra implements it.
		sr: io.NewSectionReader(fsys.ra, e.Offset, e.Header.Size),
	}

	return f, nil
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
		if f := fsys.files[i]; f != nil {
			return f.fi, nil
		}
	}

	// fs.WalkDir expects "." to return a root entry to bootstrap the walk.
	// If we didn't find it above, synthesize one.
	if name == "." {
		return root{}, nil
	}

	return nil, fs.ErrNotExist
}

func (fsys *FS) ReadDir(name string) ([]fs.DirEntry, error) {
	dirs, ok := fsys.dirs[name]
	if !ok {
		return []fs.DirEntry{}, nil
	}

	return dirs, nil
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

func New(ra io.ReaderAt, size int64) (*FS, error) {
	fsys := &FS{
		ra:    ra,
		files: []*Entry{},
		index: map[string]int{},
		dirs:  map[string][]fs.DirEntry{},
	}

	// Number of entries in a given directory, so we know how large of a slice to allocate.
	dirCount := map[string]int{}

	r := io.NewSectionReader(ra, 0, size)
	cr := &countReader{bufio.NewReaderSize(r, 1<<20), 0}
	tr := tar.NewReader(cr)

	// TODO: Do this lazily.
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}

		normalized := normalize(hdr.Name)
		dir := path.Dir(normalized)

		fsys.index[normalized] = len(fsys.files)

		fsys.files = append(fsys.files, &Entry{
			Header:   *hdr,
			Offset:   cr.n,
			Filename: normalized,
			dir:      dir,
			fi:       hdr.FileInfo(),
		})

		dirCount[dir]++
	}

	// Pre-generate the results of ReadDir so we don't allocate a ton if fs.WalkDir calls us.
	// TODO: Consider doing this lazily in a sync.Once the first time we see a ReadDir.
	for dir, count := range dirCount {
		fsys.dirs[dir] = make([]fs.DirEntry, 0, count)
	}

	for _, f := range fsys.files {
		fsys.dirs[f.dir] = append(fsys.dirs[f.dir], f)
	}

	for _, files := range fsys.dirs {
		slices.SortFunc(files, func(a, b fs.DirEntry) int {
			return cmp.Compare(a.Name(), b.Name())
		})
	}

	return fsys, nil
}

func (fsys *FS) Entry(name string) (*Entry, error) {
	i, ok := fsys.index[name]
	if !ok {
		return nil, fs.ErrNotExist
	}

	e := fsys.files[i]
	return e, nil
}

func (fsys *FS) Encode(w io.Writer) error {
	toc := TOC{
		Entries: fsys.files,
	}

	return json.NewEncoder(w).Encode(&toc)
}

func Decode(ra io.ReaderAt, r io.Reader) (*FS, error) {
	toc := TOC{}
	if err := json.NewDecoder(r).Decode(&toc); err != nil {
		return nil, err
	}

	fsys := &FS{
		ra:    ra,
		files: toc.Entries,
		index: make(map[string]int, len(toc.Entries)),
	}

	for i, e := range fsys.files {
		e.fi = e.Header.FileInfo()
		fsys.index[e.Filename] = i
	}

	return fsys, nil
}

type TOC struct {
	Entries []*Entry
}

func normalize(s string) string {
	return strings.TrimPrefix(strings.TrimSuffix(s, "/"), "./")
}
