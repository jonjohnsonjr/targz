package lsp

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io"
	"strings"
	"unicode"

	"github.com/jonjohnsonjr/targz/deflate"
)

const tarEntryFirstLine = 4

type tarEntry struct {
	Name       string
	LinkName   string
	Kind       string
	Mode       string
	Size       int64
	Modified   string
	Header     deflate.OutRange
	Data       deflate.OutRange
	Line       int
	SymbolKind int
}

type tarCountReader struct {
	r io.Reader
	n int64
}

func (r *tarCountReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	r.n += int64(n)
	return n, err
}

// parseTarView deliberately relies on archive/tar's checksum validation rather
// than extensions or MIME guesses. Calling Next skips the previous payload and
// padding, leaving the count at the exact start of the next entry's data.
func parseTarView(output []byte) ([]tarEntry, bool) {
	cr := &tarCountReader{r: bytes.NewReader(output)}
	tr := tar.NewReader(cr)
	var entries []tarEntry
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, false
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
	return entries, len(entries) != 0
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

func renderTarView(meta []byte, trace *deflate.Trace, entries []tarEntry) []byte {
	var out bytes.Buffer
	fmt.Fprintf(&out, "%s%s\n", headerPrefix, meta)
	var payload int64
	for _, entry := range entries {
		payload += entry.Size
	}
	fmt.Fprintf(&out, "# tar archive · %d entries · %d payload bytes · %d decoded bytes\n", len(entries), payload, len(trace.Output))
	fmt.Fprintln(&out, "kind     mode             size  modified          data range             name")
	fmt.Fprintln(&out, "──────── ────────── ──────────  ────────────────  ─────────────────────  ────")
	for _, entry := range entries {
		name := entry.Name
		if entry.LinkName != "" {
			name += " → " + entry.LinkName
		}
		dataRange := fmt.Sprintf("out[%d:%d]", entry.Data.Start, entry.Data.End)
		fmt.Fprintf(&out, "%-8s %-10s %10d  %-16s  %-21s  %s\n",
			entry.Kind, entry.Mode, entry.Size, entry.Modified, dataRange, name)
	}
	return out.Bytes()
}
