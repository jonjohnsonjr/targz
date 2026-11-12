package lsp

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/flate"
	"compress/gzip"
	"fmt"
	"io"
	"math"
	"os"
	"path"
	"sort"
	"strings"
	"unicode"

	"github.com/jonjohnsonjr/targz/deflate"
	"github.com/jonjohnsonjr/targz/gsip"
)

type tarEntry struct {
	Index       int
	Name        string
	DisplayName string
	LinkName    string
	Kind        string
	Mode        string
	Size        int64
	Modified    string
	Header      deflate.OutRange
	Data        deflate.OutRange
	Line        int
	Depth       int
	TreePrefix  string
	Synthetic   bool
	SymbolKind  int
	NameToken   lspRange
	LinkToken   lspRange
}

type tarCountReader struct {
	r      io.Reader
	n      int64
	prefix []byte
}

func (r *tarCountReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	if len(r.prefix) < 1024 {
		keep := min(n, 1024-len(r.prefix))
		r.prefix = append(r.prefix, p[:keep]...)
	}
	r.n += int64(n)
	return n, err
}

// parseTarView deliberately relies on archive/tar's checksum validation rather
// than extensions or MIME guesses. Calling Next skips the previous payload and
// padding, leaving the count at the exact start of the next entry's data.
func parseTarView(output []byte) ([]tarEntry, bool) {
	cr := &tarCountReader{r: bytes.NewReader(output)}
	entries, err := scanTarReader(cr)
	if err != nil {
		return nil, false
	}
	return entries, tarScanValid(cr, entries)
}

func scanTarFile(source string, raw bool) ([]tarEntry, bool, error) {
	revision, err := quickFileRevision(source)
	if err != nil {
		return nil, false, err
	}
	if catalog, ok := loadTarCatalog(revision, raw); ok {
		return catalog.Entries, true, nil
	}
	entries, isTar, index, err := buildTarCatalog(source, raw)
	if err == nil && isTar {
		_ = saveTarCatalog(revision, raw, entries, index)
	}
	return entries, isTar, err
}

func buildTarCatalog(source string, raw bool) ([]tarEntry, bool, []byte, error) {
	f, err := os.Open(source)
	if err != nil {
		return nil, false, nil, err
	}
	defer f.Close()

	input := bufio.NewReader(f)
	magic, _ := input.Peek(2)
	if !raw && len(magic) == 2 && magic[0] == 0x1f && magic[1] == 0x8b {
		return buildIndexedGzipCatalog(f)
	}
	decoded := flate.NewReader(input)
	defer decoded.Close()

	cr := &tarCountReader{r: decoded}
	entries, scanErr := scanTarReader(cr)
	if scanErr != nil {
		return nil, false, nil, nil
	}
	if _, err := io.Copy(io.Discard, cr); err != nil {
		return nil, false, nil, err
	}
	return entries, tarScanValid(cr, entries), nil, nil
}

func buildIndexedGzipCatalog(f *os.File) ([]tarEntry, bool, []byte, error) {
	info, err := f.Stat()
	if err != nil {
		return nil, false, nil, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, false, nil, err
	}
	zr, err := gsip.NewReader(f, info.Size())
	if err != nil {
		return nil, false, nil, err
	}
	cr := &tarCountReader{r: io.NewSectionReader(zr, 0, math.MaxInt64)}
	entries, scanErr := scanTarReader(cr)
	_, drainErr := io.Copy(io.Discard, cr)
	zr.Wait()
	if drainErr != nil {
		return nil, false, nil, drainErr
	}
	if scanErr != nil {
		return nil, false, nil, nil
	}
	valid := tarScanValid(cr, entries)
	if !valid {
		return entries, false, nil, nil
	}
	var index bytes.Buffer
	if err := zr.Encode(&index); err != nil {
		return nil, false, nil, err
	}
	return entries, true, index.Bytes(), nil
}

func copyTarEntry(source string, raw bool, entry tarEntry, dst io.Writer) error {
	if !raw {
		revision, err := quickFileRevision(source)
		if err == nil {
			if catalog, ok := loadTarCatalog(revision, raw); ok && len(catalog.GSIPIndex) != 0 {
				return copyIndexedTarEntry(source, catalog.GSIPIndex, entry, dst)
			}
		}
	}
	return copyTarEntrySequential(source, raw, entry.Index, dst)
}

func copyIndexedTarEntry(source string, index []byte, entry tarEntry, dst io.Writer) error {
	f, err := os.Open(source)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	return copyIndexedTarEntryAt(f, info.Size(), index, entry, dst)
}

func copyIndexedTarEntryAt(ra io.ReaderAt, size int64, index []byte, entry tarEntry, dst io.Writer) error {
	zr, err := gsip.Decode(ra, size, bytes.NewReader(index))
	if err != nil {
		return err
	}
	n, err := io.Copy(dst, io.NewSectionReader(zr, int64(entry.Data.Start), entry.Size))
	if err != nil {
		return err
	}
	if n != entry.Size {
		return io.ErrUnexpectedEOF
	}
	return nil
}

func copyTarEntrySequential(source string, raw bool, index int, dst io.Writer) error {
	f, err := os.Open(source)
	if err != nil {
		return err
	}
	defer f.Close()
	input := bufio.NewReader(f)
	magic, _ := input.Peek(2)
	var decoded io.ReadCloser
	if !raw && len(magic) == 2 && magic[0] == 0x1f && magic[1] == 0x8b {
		decoded, err = gzip.NewReader(input)
	} else {
		decoded = flate.NewReader(input)
	}
	if err != nil {
		return err
	}
	defer decoded.Close()
	tr := tar.NewReader(decoded)
	for current := 0; ; current++ {
		hdr, err := tr.Next()
		if err != nil {
			if err == io.EOF {
				return fmt.Errorf("tar entry %d does not exist", index)
			}
			return err
		}
		if current == index {
			n, err := io.Copy(dst, io.LimitReader(tr, hdr.Size))
			if err != nil {
				return err
			}
			if n != hdr.Size {
				return io.ErrUnexpectedEOF
			}
			return nil
		}
	}
}

func scanTarReader(cr *tarCountReader) ([]tarEntry, error) {
	tr := tar.NewReader(cr)
	var entries []tarEntry
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		dataStart := cr.n
		headerStart := dataStart - 512
		if headerStart < 0 {
			headerStart = 0
		}
		kind, symbolKind := tarKind(hdr.Typeflag)
		modified := "-"
		if !hdr.ModTime.IsZero() {
			modified = hdr.ModTime.UTC().Format("2006-01-02 15:04")
		}
		entries = append(entries, tarEntry{
			Index:      len(entries),
			Name:       displayTarText(hdr.Name),
			LinkName:   displayTarText(hdr.Linkname),
			Kind:       kind,
			Mode:       hdr.FileInfo().Mode().String(),
			Size:       hdr.Size,
			Modified:   modified,
			Header:     deflate.OutRange{Start: deflate.OutPos(headerStart), End: deflate.OutPos(dataStart)},
			Data:       deflate.OutRange{Start: deflate.OutPos(dataStart), End: deflate.OutPos(dataStart + hdr.Size)},
			SymbolKind: symbolKind,
		})
	}
	return entries, nil
}

func tarScanValid(cr *tarCountReader, entries []tarEntry) bool {
	if len(entries) != 0 {
		return true
	}
	// archive/tar reports EOF for both an empty input and the two zero blocks
	// which constitute an empty archive. Require the actual end marker here.
	return len(cr.prefix) >= 1024 && bytes.Equal(cr.prefix[:1024], make([]byte, 1024))
}

func tarKind(flag byte) (string, int) {
	switch flag {
	case tar.TypeDir:
		return "dir", 3 // SymbolKind.Namespace
	case tar.TypeSymlink:
		return "symlink", 1
	case tar.TypeLink:
		return "hardlink", 1
	case tar.TypeChar:
		return "char", 1
	case tar.TypeBlock:
		return "block", 1
	case tar.TypeFifo:
		return "fifo", 1
	default:
		return "file", 1 // SymbolKind.File
	}
}

func displayTarText(value string) string {
	value = strings.ToValidUTF8(value, "�")
	return strings.Map(func(r rune) rune {
		if r == '\t' || r == '\n' || r == '\r' || !unicode.IsPrint(r) {
			return '�'
		}
		return r
	}, value)
}

func directoryTarEntries(entries []tarEntry, directory string) []tarEntry {
	directory = normalizedTarPath(directory)
	prefix := directory
	if prefix != "" && prefix != "." {
		prefix += "/"
	} else {
		prefix = ""
	}
	direct := make(map[string][]tarEntry)
	nested := make(map[string]bool)
	for _, entry := range entries {
		name := normalizedTarPath(entry.Name)
		if !strings.HasPrefix(name, prefix) || name == strings.TrimSuffix(prefix, "/") {
			continue
		}
		relative := strings.TrimPrefix(name, prefix)
		segment, rest, hasRest := strings.Cut(relative, "/")
		if segment == "" {
			continue
		}
		child := normalizedTarPath(path.Join(directory, segment))
		if hasRest && rest != "" {
			nested[child] = true
			continue
		}
		direct[child] = append(direct[child], entry)
	}
	for child := range nested {
		hasDirectory := false
		for _, entry := range direct[child] {
			if entry.Kind == "dir" {
				hasDirectory = true
				break
			}
		}
		if !hasDirectory {
			direct[child] = append(direct[child], tarEntry{
				Name: child, Kind: "dir", Mode: "drwxr-xr-x", Synthetic: true, SymbolKind: 3,
			})
		}
	}
	names := make([]string, 0, len(direct))
	for name := range direct {
		names = append(names, name)
	}
	sort.Strings(names)
	rowCount := 0
	for _, children := range direct {
		rowCount += len(children)
	}
	rows := make([]tarEntry, 0, rowCount)
	for i, name := range names {
		last := i == len(names)-1
		for _, entry := range direct[name] {
			entry.DisplayName = path.Base(strings.TrimSuffix(entry.Name, "/"))
			if entry.Kind == "dir" {
				entry.DisplayName += "/"
			}
			entry.Depth = 0
			entry.TreePrefix = tarTreePrefix(nil, last)
			rows = append(rows, entry)
		}
	}
	return rows
}

func tarTreePrefix(ancestorLast []bool, last bool) string {
	var b strings.Builder
	for _, ancestorWasLast := range ancestorLast {
		if ancestorWasLast {
			b.WriteString("    ")
		} else {
			b.WriteString("│   ")
		}
	}
	if last {
		b.WriteString("└── ")
	} else {
		b.WriteString("├── ")
	}
	return b.String()
}

func renderTarView(meta []byte, entries []tarEntry, directory string) []byte {
	var out bytes.Buffer
	var payload int64
	for _, entry := range entries {
		payload += entry.Size
	}
	location := "/"
	if directory = normalizedTarPath(directory); directory != "" && directory != "." {
		location += directory + "/"
	}
	fmt.Fprintf(&out, "# tar · %s · %d entries · %d payload bytes\n", location, len(entries), payload)
	fmt.Fprintln(&out, "/")
	for _, entry := range directoryTarEntries(entries, directory) {
		fmt.Fprintf(&out, "%s%s", entry.TreePrefix, entry.DisplayName)
		if entry.LinkName != "" {
			fmt.Fprintf(&out, " → %s", entry.LinkName)
		}
		out.WriteByte('\n')
	}
	fmt.Fprintf(&out, "%s%s\n", headerPrefix, meta)
	return out.Bytes()
}

func tarTokenRanges(text string, line int, entry tarEntry) (lspRange, lspRange) {
	nameStart := len(entry.TreePrefix)
	nameEnd := nameStart + len(entry.DisplayName)
	name := byteRange(text, line, nameStart, nameEnd)
	if entry.LinkName == "" {
		return name, lspRange{}
	}
	linkStart := strings.LastIndex(text, entry.LinkName)
	if linkStart < 0 {
		return name, lspRange{}
	}
	return name, byteRange(text, line, linkStart, linkStart+len(entry.LinkName))
}

func normalizedTarPath(value string) string {
	return strings.TrimPrefix(path.Clean("/"+value), "/")
}

func resolvedTarLink(entry tarEntry) string {
	target := entry.LinkName
	if entry.Kind == "symlink" && !strings.HasPrefix(target, "/") {
		target = path.Join(path.Dir(entry.Name), target)
	}
	return normalizedTarPath(target)
}
