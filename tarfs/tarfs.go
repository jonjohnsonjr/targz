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
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"iter"
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
	return e.fi.Mode().Type()
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

	// current position in readdir listing
	cursor int
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

func (f *File) ReadDir(n int) ([]fs.DirEntry, error) {
	if n == 0 {
		return nil, nil
	}

	dir, err := f.fsys.ReadDir(f.Entry.Filename)
	if err != nil {
		return nil, err
	}

	if f.cursor >= len(dir) {
		if n < 0 {
			return nil, nil
		}

		return nil, io.EOF
	}

	if n > 0 && len(dir)-f.cursor > n {
		ret := dir[f.cursor : f.cursor+n]
		f.cursor += n
		return ret, nil
	}

	ret := dir[f.cursor:]
	f.cursor = len(dir)

	return ret, nil
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

func dirs(name string) iter.Seq[string] {
	return func(yield func(string) bool) {
		for i, v := range name {
			if v == '/' {
				if !yield(name[0:i]) {
					return
				}
			}
		}
	}
}

// arbitrary number stolen from filepath.EvalSymlinks
// this seems to be 40 in linux (MAXSYMLINKS), which might be more reasonable
const maxHops = 255

// open follows symlinks up to [maxHops] times.
func (fsys *FS) open(name string, hops int) (fs.File, error) {
	if hops > maxHops {
		return nil, fmt.Errorf("opening %s: chased too many (%d) symlinks", name, maxHops)
	}

	e, err := fsys.Entry(name)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// Deal with symlinked dirs.
			for dir := range dirs(name) {
				e, err := fsys.Entry(dir)
				if err != nil {
					continue
				}

				if e.Header.Typeflag != tar.TypeSymlink {
					continue
				}

				// We need to rewrite what comes after the symlinked dir.
				rest := strings.TrimPrefix(name, dir)

				link := e.Header.Linkname
				if path.IsAbs(link) {
					return fsys.open(normalize(path.Join(link, rest)), hops+1)
				}

				return fsys.open(path.Join(e.dir, link, rest), hops+1)
			}
		}

		return nil, err
	}

	switch e.Header.Typeflag {
	case tar.TypeSymlink, tar.TypeLink:
		link := e.Header.Linkname
		if path.IsAbs(link) || e.Header.Typeflag == tar.TypeLink {
			return fsys.open(normalize(link), hops+1)
		}

		return fsys.open(path.Join(e.dir, link), hops+1)
	}

	f := &File{
		Entry: e,
		fsys:  fsys,
		sr:    io.NewSectionReader(fsys.ra, e.Offset, e.Header.Size),
	}

	return f, nil
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
			sr:   io.NewSectionReader(bytes.NewReader(nil), 0, 0),
		}, nil
	}

	return fsys.open(name, 0)
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

	// Assume negative size means caller doesn't know. This could be better.
	if size < 0 {
		size = 1<<63 - 1
	}

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
		// TODO: Consider lazily sorting each directory the first time it's accessed.
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
	// Trim prefix of "/" and prefix of "./"
	// Trim suffix of "/"
	return strings.TrimPrefix(strings.TrimPrefix(strings.TrimSuffix(s, "/"), "/"), "./")
}

// Index returns a list of tar entries with their offsets.
//
// This is primarily useful if you don't have an io.ReaderAt implementation handy but still
// want to know the offsets, for example if you're using something like:
// https://pkg.go.dev/cloud.google.com/go/storage#ObjectHandle.NewRangeReader
func Index(r io.Reader) ([]*Entry, error) {
	var files []*Entry

	cr := &countReader{bufio.NewReaderSize(r, 1<<20), 0}
	tr := tar.NewReader(cr)

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

		files = append(files, &Entry{
			Header:   *hdr,
			Offset:   cr.n,
			Filename: normalized,
			dir:      dir,
			fi:       hdr.FileInfo(),
		})
	}

	return files, nil
}
