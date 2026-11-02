package server

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	bundlelib "github.com/jvzantvoort/bundle/bundle"
	"net/http"
)

type poolResponse struct {
	Name        string `json:"name"`
	Title       string `json:"title"`
	BundleCount int    `json:"bundle_count"`
}

type manifestResponse struct {
	Path      string `json:"path"`
	Checksum  string `json:"checksum"`
	SizeBytes int64  `json:"size_bytes"`
}

type bundleSummary struct {
	Checksum  string    `json:"checksum"`
	Title     string    `json:"title"`
	Author    string    `json:"author"`
	CreatedAt time.Time `json:"created_at"`
	Version   int       `json:"version"`
	FileCount int       `json:"file_count"`
	SizeBytes int64     `json:"size_bytes"`
	Verified  bool      `json:"verified"`
}

func (a *App) bundles(poolName string) ([]bundleRecord, error) {
	if cached, ok := a.cache.get(poolName); ok {
		return cached, nil
	}
	return a.scanBundles(poolName)
}

func (a *App) scanBundles(poolName string) ([]bundleRecord, error) {
	pool := a.config.Pools[poolName]
	entries, err := os.ReadDir(pool.Root)
	if errors.Is(err, fs.ErrNotExist) {
		return []bundleRecord{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read pool: %w", err)
	}
	records := make([]bundleRecord, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() || !validChecksum(entry.Name()) {
			continue
		}
		record, err := loadBundle(filepath.Join(pool.Root, entry.Name()))
		if err != nil {
			continue
		}
		if record.Metadata.BundleChecksum != entry.Name() {
			continue
		}
		records = append(records, record)
	}
	return records, nil
}

func (a *App) refreshMetadata(poolName string) (int, error) {
	records, err := a.scanBundles(poolName)
	if err != nil {
		return 0, err
	}
	a.cache.set(poolName, records)
	return len(records), nil
}

func (a *App) invalidateMetadata(poolName string) {
	a.cache.remove(poolName)
}

func (a *App) bundle(poolName, checksum string) (bundleRecord, error) {
	if !validChecksum(checksum) {
		return bundleRecord{}, fs.ErrNotExist
	}
	pool := a.config.Pools[poolName]
	return loadBundle(filepath.Join(pool.Root, checksum))
}

func loadBundle(bundlePath string) (bundleRecord, error) {
	bundle, err := bundlelib.Load(bundlePath)
	if err != nil {
		return bundleRecord{}, err
	}
	if err := bundle.Metadata.Validate(); err != nil {
		return bundleRecord{}, err
	}
	currentRevision, err := revision(bundlePath)
	if err != nil {
		return bundleRecord{}, err
	}
	return bundleRecord{
		Path:     bundle.Path,
		Metadata: bundle.Metadata,
		State:    bundle.State,
		Tags:     bundle.Tags,
		Files:    bundle.Files,
		Revision: currentRevision,
	}, nil
}

func (r bundleRecord) summary() bundleSummary {
	return bundleSummary{
		Checksum:  r.Metadata.BundleChecksum,
		Title:     r.Metadata.Title,
		Author:    r.Metadata.Author,
		CreatedAt: r.Metadata.CreatedAt,
		Version:   r.Metadata.Version,
		FileCount: len(r.Files.Records),
		SizeBytes: r.State.SizeBytes,
		Verified:  r.State.Verified,
	}
}

func (a *App) bundleResponse(poolName string, r bundleRecord) map[string]any {
	return map[string]any{
		"checksum":   r.Metadata.BundleChecksum,
		"title":      r.Metadata.Title,
		"author":     r.Metadata.Author,
		"created_at": r.Metadata.CreatedAt,
		"version":    r.Metadata.Version,
		"file_count": len(r.Files.Records),
		"size_bytes": r.State.SizeBytes,
		"verified":   r.State.Verified,
		"metadata": map[string]any{
			"title":      r.Metadata.Title,
			"created_at": r.Metadata.CreatedAt,
			"author":     r.Metadata.Author,
			"version":    r.Metadata.Version,
		},
		"state": map[string]any{
			"verified":     r.State.Verified,
			"last_checked": r.State.LastChecked,
			"replicas":     r.State.Replicas,
			"size_bytes":   r.State.SizeBytes,
		},
		"tags":     r.Tags.List(),
		"revision": r.Revision,
		"links": map[string]string{
			"self":  a.apiPath("/pools/" + poolName + "/bundles/" + r.Metadata.BundleChecksum),
			"files": a.apiPath("/pools/" + poolName + "/bundles/" + r.Metadata.BundleChecksum + "/files"),
		},
	}
}

func manifest(record bundleRecord) []manifestResponse {
	entries := make([]manifestResponse, 0, len(record.Files.Records))
	for _, entry := range record.Files.Records {
		size := int64(0)
		if info, err := os.Stat(filepath.Join(record.Path, filepath.FromSlash(entry.FilePath))); err == nil {
			size = info.Size()
		}
		entries = append(entries, manifestResponse{Path: entry.FilePath, Checksum: entry.Checksum, SizeBytes: size})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	return entries
}

func revision(bundlePath string) (string, error) {
	hash := sha256.New()
	for _, name := range []string{"META.json", "TAGS.txt"} {
		content, err := os.ReadFile(filepath.Join(bundlePath, ".bundle", name))
		if err != nil {
			return "", err
		}
		_, _ = hash.Write(content)
	}
	return `"` + hex.EncodeToString(hash.Sum(nil)) + `"`, nil
}

func validChecksum(checksum string) bool {
	if len(checksum) != 64 {
		return false
	}
	for _, char := range checksum {
		if !((char >= 'a' && char <= 'f') || (char >= '0' && char <= '9')) {
			return false
		}
	}
	return true
}

func safeFilePath(value string) (string, error) {
	value = strings.TrimPrefix(path.Clean("/"+value), "/")
	if value == "" || value == "." || strings.HasPrefix(value, "../") || value == ".bundle" || strings.HasPrefix(value, ".bundle/") {
		return "", fmt.Errorf("path must identify a bundle file")
	}
	return value, nil
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func optionalBool(value string) (bool, bool, error) {
	if value == "" {
		return false, false, nil
	}
	parsed, err := strconv.ParseBool(value)
	return parsed, true, err
}

func pagination(r *http.Request) (int, int) {
	page := queryInt(r, "page", 1, 1, 1_000_000)
	pageSize := queryInt(r, "page_size", 50, 1, 100)
	return page, pageSize
}

func pageBounds(total, page, pageSize int) (int, int) {
	start := (page - 1) * pageSize
	if start > total {
		start = total
	}
	end := start + pageSize
	if end > total {
		end = total
	}
	return start, end
}

func pageBundles(records []bundleRecord, page, pageSize int) map[string]any {
	start, end := pageBounds(len(records), page, pageSize)
	items := make([]bundleSummary, 0, end-start)
	for _, record := range records[start:end] {
		items = append(items, record.summary())
	}
	return map[string]any{"items": items, "page": page, "page_size": pageSize, "total": len(records)}
}
