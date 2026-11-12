package lsp

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/jonjohnsonjr/targz/deflate"
)

const tarCatalogVersion = 1

type cachedTarCatalog struct {
	Version   int               `json:"version"`
	Revision  [sha256.Size]byte `json:"revision"`
	Raw       bool              `json:"raw,omitempty"`
	Entries   []tarEntry        `json:"-"`
	Disk      []cachedTarEntry  `json:"entries"`
	GSIPIndex json.RawMessage   `json:"gsip,omitempty"`
}

type cachedTarEntry struct {
	Name        string `json:"name"`
	LinkName    string `json:"link,omitempty"`
	Kind        string `json:"kind"`
	Mode        string `json:"mode"`
	Size        int64  `json:"size"`
	Modified    string `json:"modified,omitempty"`
	HeaderStart uint64 `json:"header"`
	DataStart   uint64 `json:"data"`
	SymbolKind  int    `json:"symbolKind"`
}

func loadTarCatalog(revision [sha256.Size]byte, raw bool) (cachedTarCatalog, bool) {
	path, err := tarCatalogPath(revision, raw)
	if err != nil {
		return cachedTarCatalog{}, false
	}
	f, err := os.Open(path)
	if err != nil {
		return cachedTarCatalog{}, false
	}
	defer f.Close()
	var catalog cachedTarCatalog
	if err := json.NewDecoder(f).Decode(&catalog); err != nil || catalog.Version != tarCatalogVersion || catalog.Revision != revision || catalog.Raw != raw {
		return cachedTarCatalog{}, false
	}
	if len(catalog.GSIPIndex) != 0 && !json.Valid(catalog.GSIPIndex) {
		return cachedTarCatalog{}, false
	}
	catalog.Entries = make([]tarEntry, len(catalog.Disk))
	for i, entry := range catalog.Disk {
		if entry.Size < 0 || entry.DataStart < entry.HeaderStart || uint64(entry.Size) > ^uint64(0)-entry.DataStart {
			return cachedTarCatalog{}, false
		}
		catalog.Entries[i] = tarEntry{
			Index: i, Name: entry.Name, LinkName: entry.LinkName, Kind: entry.Kind,
			Mode: entry.Mode, Size: entry.Size, Modified: entry.Modified,
			Header:     deflate.OutRange{Start: deflate.OutPos(entry.HeaderStart), End: deflate.OutPos(entry.DataStart)},
			Data:       deflate.OutRange{Start: deflate.OutPos(entry.DataStart), End: deflate.OutPos(entry.DataStart + uint64(entry.Size))},
			SymbolKind: entry.SymbolKind,
		}
	}
	return catalog, true
}

func saveTarCatalog(revision [sha256.Size]byte, raw bool, entries []tarEntry, index []byte) error {
	path, err := tarCatalogPath(revision, raw)
	if err != nil {
		return err
	}
	catalog := cachedTarCatalog{
		Version: tarCatalogVersion, Revision: revision, Raw: raw,
		Disk: make([]cachedTarEntry, len(entries)), GSIPIndex: json.RawMessage(index),
	}
	for i, entry := range entries {
		catalog.Disk[i] = cachedTarEntry{
			Name: entry.Name, LinkName: entry.LinkName, Kind: entry.Kind, Mode: entry.Mode,
			Size: entry.Size, Modified: entry.Modified, HeaderStart: uint64(entry.Header.Start),
			DataStart: uint64(entry.Data.Start), SymbolKind: entry.SymbolKind,
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".catalog-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	encoder := json.NewEncoder(tmp)
	if err := encoder.Encode(&catalog); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err == nil {
		return nil
	}
	// Windows does not replace an existing destination. A corrupt cache is
	// disposable, so remove it and publish the fully written temporary file.
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return os.Rename(tmpPath, path)
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
	return filepath.Join(root, fmt.Sprintf("tar-v%d-%s-%x.json", tarCatalogVersion, mode, revision[:16])), nil
}
