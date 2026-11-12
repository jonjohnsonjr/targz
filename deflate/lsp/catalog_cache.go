package lsp

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jonjohnsonjr/targz/deflate"
)

const (
	tarCatalogVersion = 2
	catalogHeaderSize = 128
	catalogIndexSize  = 32
)

var catalogMagic = [8]byte{'G', 'Z', 'P', 'C', 'A', 'T', '0', '2'}

type catalogHeader struct {
	Revision               [sha256.Size]byte
	Raw                    bool
	Count, Payload         uint64
	GSIPOffset, GSIPLength uint64
	DirOffset, DirCount    uint64
	PathOffset, PathCount  uint64
	LinkOffset, LinkCount  uint64
}

type catalogIndexEntry struct {
	Hash           [16]byte
	Offset, Length uint64
}

type catalogRecord struct {
	Key     string           `json:"key"`
	Entries []cachedTarEntry `json:"entries"`
}

type cachedTarEntry struct {
	Index       int    `json:"index"`
	Name        string `json:"name"`
	LinkName    string `json:"link,omitempty"`
	Kind        string `json:"kind"`
	Mode        string `json:"mode"`
	Size        int64  `json:"size"`
	Modified    string `json:"modified,omitempty"`
	HeaderStart uint64 `json:"header"`
	DataStart   uint64 `json:"data"`
	SymbolKind  int    `json:"symbolKind"`
	Synthetic   bool   `json:"synthetic,omitempty"`
}

func loadTarDirectory(revision [sha256.Size]byte, raw bool, directory string) ([]tarEntry, catalogHeader, bool) {
	return loadCatalogRecord(revision, raw, "dir", cleanCatalogDir(directory))
}

func loadTarPath(revision [sha256.Size]byte, raw bool, name string) ([]tarEntry, bool) {
	entries, _, ok := loadCatalogRecord(revision, raw, "path", normalizedTarPath(name))
	return entries, ok
}

func loadTarLinks(revision [sha256.Size]byte, raw bool, name string) ([]tarEntry, bool) {
	entries, _, ok := loadCatalogRecord(revision, raw, "link", normalizedTarPath(name))
	return entries, ok
}

func loadGSIPIndex(revision [sha256.Size]byte, raw bool) ([]byte, bool) {
	f, header, ok := openTarCatalog(revision, raw)
	if !ok {
		return nil, false
	}
	defer f.Close()
	if header.GSIPLength == 0 || header.GSIPLength > uint64(^uint(0)>>1) {
		return nil, false
	}
	data := make([]byte, int(header.GSIPLength))
	if _, err := f.ReadAt(data, int64(header.GSIPOffset)); err != nil || !json.Valid(data) {
		return nil, false
	}
	return data, true
}

func loadCatalogRecord(revision [sha256.Size]byte, raw bool, kind, key string) ([]tarEntry, catalogHeader, bool) {
	f, header, ok := openTarCatalog(revision, raw)
	if !ok {
		return nil, catalogHeader{}, false
	}
	defer f.Close()
	var offset, count uint64
	switch kind {
	case "dir":
		offset, count = header.DirOffset, header.DirCount
	case "path":
		offset, count = header.PathOffset, header.PathCount
	case "link":
		offset, count = header.LinkOffset, header.LinkCount
	default:
		return nil, catalogHeader{}, false
	}
	record, ok := lookupCatalogRecord(f, offset, count, key)
	if !ok {
		return nil, catalogHeader{}, false
	}
	entries := make([]tarEntry, len(record.Entries))
	for i, entry := range record.Entries {
		converted, ok := entry.tarEntry()
		if !ok {
			return nil, catalogHeader{}, false
		}
		entries[i] = converted
	}
	if kind == "dir" {
		entries = decorateDirectoryEntries(entries)
	}
	return entries, header, true
}

func openTarCatalog(revision [sha256.Size]byte, raw bool) (*os.File, catalogHeader, bool) {
	name, err := tarCatalogPath(revision, raw)
	if err != nil {
		return nil, catalogHeader{}, false
	}
	f, err := os.Open(name)
	if err != nil {
		return nil, catalogHeader{}, false
	}
	buf := make([]byte, catalogHeaderSize)
	if _, err := io.ReadFull(f, buf); err != nil || string(buf[:8]) != string(catalogMagic[:]) {
		f.Close()
		return nil, catalogHeader{}, false
	}
	var header catalogHeader
	copy(header.Revision[:], buf[8:40])
	header.Raw = buf[40] != 0
	values := []*uint64{&header.Count, &header.Payload, &header.GSIPOffset, &header.GSIPLength, &header.DirOffset, &header.DirCount, &header.PathOffset, &header.PathCount, &header.LinkOffset, &header.LinkCount}
	for i, value := range values {
		*value = binary.LittleEndian.Uint64(buf[48+i*8:])
	}
	if header.Revision != revision || header.Raw != raw {
		f.Close()
		return nil, catalogHeader{}, false
	}
	return f, header, true
}

func lookupCatalogRecord(f *os.File, indexOffset, count uint64, key string) (catalogRecord, bool) {
	want := catalogHash(key)
	lo, hi := uint64(0), count
	buf := make([]byte, catalogIndexSize)
	for lo < hi {
		mid := lo + (hi-lo)/2
		if _, err := f.ReadAt(buf, int64(indexOffset+mid*catalogIndexSize)); err != nil {
			return catalogRecord{}, false
		}
		if strings.Compare(string(buf[:16]), string(want[:])) < 0 {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	for i := lo; i < count; i++ {
		if _, err := f.ReadAt(buf, int64(indexOffset+i*catalogIndexSize)); err != nil || string(buf[:16]) != string(want[:]) {
			break
		}
		offset := binary.LittleEndian.Uint64(buf[16:24])
		length := binary.LittleEndian.Uint64(buf[24:32])
		if length > uint64(^uint(0)>>1) {
			return catalogRecord{}, false
		}
		data := make([]byte, int(length))
		if _, err := f.ReadAt(data, int64(offset)); err != nil {
			return catalogRecord{}, false
		}
		var record catalogRecord
		if json.Unmarshal(data, &record) == nil && record.Key == key {
			return record, true
		}
	}
	return catalogRecord{}, false
}

func saveTarCatalog(revision [sha256.Size]byte, raw bool, entries []tarEntry, checkpoint []byte) error {
	name, err := tarCatalogPath(revision, raw)
	if err != nil {
		return err
	}
	dirs, paths, links := catalogGroups(entries)
	if err := os.MkdirAll(filepath.Dir(name), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(name), ".catalog-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(make([]byte, catalogHeaderSize)); err != nil {
		return err
	}
	header := catalogHeader{Revision: revision, Raw: raw, Count: uint64(len(entries)), GSIPOffset: catalogHeaderSize, GSIPLength: uint64(len(checkpoint))}
	for _, entry := range entries {
		header.Payload += uint64(max(entry.Size, 0))
	}
	if _, err := tmp.Write(checkpoint); err != nil {
		return err
	}
	dirIndex, err := writeCatalogRecords(tmp, dirs)
	if err != nil {
		return err
	}
	pathIndex, err := writeCatalogRecords(tmp, paths)
	if err != nil {
		return err
	}
	linkIndex, err := writeCatalogRecords(tmp, links)
	if err != nil {
		return err
	}
	if header.DirOffset, header.DirCount, err = writeCatalogIndex(tmp, dirIndex); err != nil {
		return err
	}
	if header.PathOffset, header.PathCount, err = writeCatalogIndex(tmp, pathIndex); err != nil {
		return err
	}
	if header.LinkOffset, header.LinkCount, err = writeCatalogIndex(tmp, linkIndex); err != nil {
		return err
	}
	if _, err := tmp.WriteAt(encodeCatalogHeader(header), 0); err != nil {
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, name); err == nil {
		return nil
	}
	if err := os.Remove(name); err != nil && !os.IsNotExist(err) {
		return err
	}
	return os.Rename(tmpName, name)
}

func catalogGroups(entries []tarEntry) (map[string][]tarEntry, map[string][]tarEntry, map[string][]tarEntry) {
	dirs := map[string][]tarEntry{"": {}}
	paths := make(map[string][]tarEntry)
	links := make(map[string][]tarEntry)
	knownDirs := map[string]bool{"": true}
	for _, entry := range entries {
		name := normalizedTarPath(entry.Name)
		paths[name] = append(paths[name], entry)
		parent := cleanCatalogDir(path.Dir(name))
		dirs[parent] = append(dirs[parent], entry)
		for current := parent; ; current = cleanCatalogDir(path.Dir(current)) {
			knownDirs[current] = true
			if current == "" {
				break
			}
		}
		if entry.LinkName != "" {
			target := resolvedTarLink(entry)
			links[target] = append(links[target], entry)
		}
	}
	for directory := range knownDirs {
		if directory == "" {
			continue
		}
		parent := cleanCatalogDir(path.Dir(directory))
		hasDirectory := false
		for _, entry := range dirs[parent] {
			if normalizedTarPath(entry.Name) == directory && entry.Kind == "dir" {
				hasDirectory = true
				break
			}
		}
		if !hasDirectory {
			dirs[parent] = append(dirs[parent], tarEntry{Index: -1, Name: directory, Kind: "dir", Mode: "drwxr-xr-x", Synthetic: true, SymbolKind: 3})
		}
		if _, ok := dirs[directory]; !ok {
			dirs[directory] = nil
		}
	}
	for directory, children := range dirs {
		dirs[directory] = decorateDirectoryEntries(children)
	}
	return dirs, paths, links
}

func decorateDirectoryEntries(entries []tarEntry) []tarEntry {
	sort.SliceStable(entries, func(i, j int) bool { return normalizedTarPath(entries[i].Name) < normalizedTarPath(entries[j].Name) })
	lastName := ""
	if len(entries) != 0 {
		lastName = normalizedTarPath(entries[len(entries)-1].Name)
	}
	for i := range entries {
		name := normalizedTarPath(entries[i].Name)
		entries[i].DisplayName = path.Base(name)
		if entries[i].Kind == "dir" {
			entries[i].DisplayName += "/"
		}
		entries[i].TreePrefix = tarTreePrefix(nil, name == lastName)
	}
	return entries
}

func writeCatalogRecords(f *os.File, groups map[string][]tarEntry) ([]catalogIndexEntry, error) {
	keys := make([]string, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	index := make([]catalogIndexEntry, 0, len(keys))
	for _, key := range keys {
		record := catalogRecord{Key: key, Entries: make([]cachedTarEntry, len(groups[key]))}
		for i, entry := range groups[key] {
			record.Entries[i] = cacheTarEntry(entry)
		}
		data, err := json.Marshal(record)
		if err != nil {
			return nil, err
		}
		offset, err := f.Seek(0, io.SeekCurrent)
		if err != nil {
			return nil, err
		}
		if _, err := f.Write(data); err != nil {
			return nil, err
		}
		index = append(index, catalogIndexEntry{Hash: catalogHash(key), Offset: uint64(offset), Length: uint64(len(data))})
	}
	return index, nil
}

func writeCatalogIndex(f *os.File, entries []catalogIndexEntry) (uint64, uint64, error) {
	sort.Slice(entries, func(i, j int) bool {
		return strings.Compare(string(entries[i].Hash[:]), string(entries[j].Hash[:])) < 0
	})
	offset, err := f.Seek(0, io.SeekCurrent)
	if err != nil {
		return 0, 0, err
	}
	buf := make([]byte, catalogIndexSize)
	for _, entry := range entries {
		copy(buf[:16], entry.Hash[:])
		binary.LittleEndian.PutUint64(buf[16:24], entry.Offset)
		binary.LittleEndian.PutUint64(buf[24:32], entry.Length)
		if _, err := f.Write(buf); err != nil {
			return 0, 0, err
		}
	}
	return uint64(offset), uint64(len(entries)), nil
}

func encodeCatalogHeader(header catalogHeader) []byte {
	buf := make([]byte, catalogHeaderSize)
	copy(buf[:8], catalogMagic[:])
	copy(buf[8:40], header.Revision[:])
	if header.Raw {
		buf[40] = 1
	}
	values := []uint64{header.Count, header.Payload, header.GSIPOffset, header.GSIPLength, header.DirOffset, header.DirCount, header.PathOffset, header.PathCount, header.LinkOffset, header.LinkCount}
	for i, value := range values {
		binary.LittleEndian.PutUint64(buf[48+i*8:], value)
	}
	return buf
}

func cacheTarEntry(entry tarEntry) cachedTarEntry {
	return cachedTarEntry{Index: entry.Index, Name: entry.Name, LinkName: entry.LinkName, Kind: entry.Kind, Mode: entry.Mode, Size: entry.Size, Modified: entry.Modified, HeaderStart: uint64(entry.Header.Start), DataStart: uint64(entry.Data.Start), SymbolKind: entry.SymbolKind, Synthetic: entry.Synthetic}
}

func (entry cachedTarEntry) tarEntry() (tarEntry, bool) {
	if entry.Size < 0 || entry.DataStart < entry.HeaderStart || uint64(entry.Size) > ^uint64(0)-entry.DataStart {
		return tarEntry{}, false
	}
	return tarEntry{Index: entry.Index, Name: entry.Name, LinkName: entry.LinkName, Kind: entry.Kind, Mode: entry.Mode, Size: entry.Size, Modified: entry.Modified, Header: deflate.OutRange{Start: deflate.OutPos(entry.HeaderStart), End: deflate.OutPos(entry.DataStart)}, Data: deflate.OutRange{Start: deflate.OutPos(entry.DataStart), End: deflate.OutPos(entry.DataStart + uint64(entry.Size))}, SymbolKind: entry.SymbolKind, Synthetic: entry.Synthetic}, true
}

func catalogHash(key string) [16]byte {
	sum := sha256.Sum256([]byte(key))
	var hash [16]byte
	copy(hash[:], sum[:16])
	return hash
}

func cleanCatalogDir(value string) string {
	if value == "." || value == "/" {
		return ""
	}
	return normalizedTarPath(value)
}

func tarCatalogPath(revision [sha256.Size]byte, raw bool) (string, error) {
	root := os.Getenv("GZPLS_CACHE_DIR")
	if root == "" {
		cache, err := os.UserCacheDir()
		if err != nil {
			return "", err
		}
		root = filepath.Join(cache, "gzpls")
	}
	mode := "gzip"
	if raw {
		mode = "raw"
	}
	return filepath.Join(root, fmt.Sprintf("tar-v%d-%s-%x.cat", tarCatalogVersion, mode, revision[:16])), nil
}
