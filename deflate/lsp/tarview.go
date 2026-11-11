package lsp

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"unicode"

	"github.com/jonjohnsonjr/targz/deflate"
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
	if len(entries) != 0 {
		return entries, true
	}
	// archive/tar reports EOF for both an empty input and the two zero blocks
	// which constitute an empty archive. Require the actual end marker here.
	return entries, len(output) >= 1024 && bytes.Equal(output[:1024], make([]byte, 1024))
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

type tarTreeNode struct {
	segment  string
	fullName string
	entries  []tarEntry
	children map[string]*tarTreeNode
}

func layoutTarEntries(entries []tarEntry) []tarEntry {
	root := &tarTreeNode{children: make(map[string]*tarTreeNode)}
	for _, entry := range entries {
		clean := strings.Trim(entry.Name, "/")
		if clean == "" {
			clean = entry.Name
		}
		parts := strings.Split(clean, "/")
		node := root
		for i, part := range parts {
			child := node.children[part]
			if child == nil {
				child = &tarTreeNode{segment: part, fullName: strings.Join(parts[:i+1], "/"), children: make(map[string]*tarTreeNode)}
				node.children[part] = child
			}
			node = child
		}
		node.entries = append(node.entries, entry)
	}
	var rows []tarEntry
	flattenTarTree(root, nil, &rows)
	return rows
}

func flattenTarTree(parent *tarTreeNode, ancestorLast []bool, rows *[]tarEntry) {
	names := make([]string, 0, len(parent.children))
	for name := range parent.children {
		names = append(names, name)
	}
	sort.Strings(names)
	for i, name := range names {
		node := parent.children[name]
		last := i == len(names)-1
		entry := tarEntry{
			Name: node.fullName, DisplayName: node.segment, Kind: "dir", Mode: "drwxr-xr-x",
			Depth: len(ancestorLast), Synthetic: true, SymbolKind: 3,
		}
		if len(node.entries) != 0 {
			entry = node.entries[0]
			entry.DisplayName = node.segment
			entry.Depth = len(ancestorLast)
		}
		if entry.Kind == "dir" || len(node.children) != 0 {
			entry.DisplayName = strings.TrimSuffix(entry.DisplayName, "/") + "/"
		}
		entry.TreePrefix = tarTreePrefix(ancestorLast, last)
		*rows = append(*rows, entry)
		if len(node.entries) > 1 {
			for _, duplicate := range node.entries[1:] {
				duplicate.DisplayName = node.segment
				duplicate.Depth = len(ancestorLast)
				duplicate.TreePrefix = tarTreePrefix(ancestorLast, last)
				*rows = append(*rows, duplicate)
			}
		}
		flattenTarTree(node, append(ancestorLast, last), rows)
	}
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

func renderTarView(meta []byte, trace *deflate.Trace, entries []tarEntry) []byte {
	var out bytes.Buffer
	var payload int64
	for _, entry := range entries {
		payload += entry.Size
	}
	fmt.Fprintf(&out, "# tar · %d entries · %d payload bytes\n", len(entries), payload)
	fmt.Fprintln(&out, "/")
	for _, entry := range layoutTarEntries(entries) {
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
