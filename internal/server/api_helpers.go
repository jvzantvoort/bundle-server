package server

import (
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	bundlelib "github.com/jvzantvoort/bundle/bundle"
)

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]string{"code": code, "message": message})
}

func decodeJSON(r *http.Request, destination any) error {
	defer r.Body.Close()
	decoder := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if decoder.More() {
		return errors.New("request body must contain one JSON value")
	}
	return nil
}

func matchesRevision(r *http.Request, revision string) bool {
	return r.Header.Get("If-Match") == revision
}

func (a *App) bundleError(w http.ResponseWriter, err error) {
	if errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusNotFound, "bundle_not_found", "bundle does not exist")
		return
	}
	writeError(w, http.StatusInternalServerError, "bundle_read_failed", err.Error())
}

func queryInt(r *http.Request, name string, fallback, min, max int) int {
	value := r.URL.Query().Get(name)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < min || parsed > max {
		return fallback
	}
	return parsed
}

type bundleQueryRequest struct {
	Checksums     []string   `json:"checksums"`
	Title         string     `json:"title"`
	Author        string     `json:"author"`
	Tags          []string   `json:"tags"`
	Verified      *bool      `json:"verified"`
	CreatedAfter  *time.Time `json:"created_after"`
	CreatedBefore *time.Time `json:"created_before"`
	MinSizeBytes  *int64     `json:"min_size_bytes"`
	MaxSizeBytes  *int64     `json:"max_size_bytes"`
	File          *fileQuery `json:"file"`
	Page          int        `json:"page"`
	PageSize      int        `json:"page_size"`
}

type fileQuery struct {
	Path      string `json:"path"`
	PathMatch string `json:"path_match"`
	Checksum  string `json:"checksum"`
}

func (a *App) queryBundles(w http.ResponseWriter, r *http.Request, poolName string) {
	var request bundleQueryRequest
	if err := decodeJSON(r, &request); err != nil || !request.hasFilter() {
		writeError(w, http.StatusBadRequest, "invalid_query", "at least one query filter is required")
		return
	}
	if request.Page < 1 {
		request.Page = 1
	}
	if request.PageSize < 1 || request.PageSize > 100 {
		request.PageSize = 50
	}
	records, err := a.bundles(poolName)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "pool_read_failed", err.Error())
		return
	}
	results := make([]map[string]any, 0)
	for _, record := range records {
		matches := matchingFiles(record, request.File)
		if request.matches(record, matches) {
			results = append(results, map[string]any{"bundle": record.summary(), "matching_files": matches})
		}
	}
	start, end := pageBounds(len(results), request.Page, request.PageSize)
	writeJSON(w, http.StatusOK, map[string]any{"items": results[start:end], "page": request.Page, "page_size": request.PageSize, "total": len(results)})
}

func (q bundleQueryRequest) hasFilter() bool {
	return len(q.Checksums) > 0 || q.Title != "" || q.Author != "" || len(q.Tags) > 0 || q.Verified != nil ||
		q.CreatedAfter != nil || q.CreatedBefore != nil || q.MinSizeBytes != nil || q.MaxSizeBytes != nil || q.File != nil
}

func (q bundleQueryRequest) matches(record bundleRecord, matches []manifestResponse) bool {
	if len(q.Checksums) > 0 && !contains(q.Checksums, record.Metadata.BundleChecksum) {
		return false
	}
	if q.Title != "" && !strings.Contains(strings.ToLower(record.Metadata.Title), strings.ToLower(q.Title)) {
		return false
	}
	if q.Author != "" && record.Metadata.Author != q.Author {
		return false
	}
	for _, wanted := range q.Tags {
		if !contains(record.Tags.List(), wanted) {
			return false
		}
	}
	if q.Verified != nil && record.State.Verified != *q.Verified {
		return false
	}
	if q.CreatedAfter != nil && record.Metadata.CreatedAt.Before(*q.CreatedAfter) {
		return false
	}
	if q.CreatedBefore != nil && !record.Metadata.CreatedAt.Before(*q.CreatedBefore) {
		return false
	}
	if q.MinSizeBytes != nil && record.State.SizeBytes < *q.MinSizeBytes {
		return false
	}
	if q.MaxSizeBytes != nil && record.State.SizeBytes > *q.MaxSizeBytes {
		return false
	}
	return q.File == nil || len(matches) > 0
}

func matchingFiles(record bundleRecord, query *fileQuery) []manifestResponse {
	if query == nil {
		return []manifestResponse{}
	}
	if query.PathMatch == "" {
		query.PathMatch = "contains"
	}
	matches := make([]manifestResponse, 0)
	for _, entry := range manifest(record) {
		if query.Checksum != "" && entry.Checksum != query.Checksum {
			continue
		}
		if query.Path != "" && !pathMatches(entry.Path, query.Path, query.PathMatch) {
			continue
		}
		matches = append(matches, entry)
	}
	return matches
}

func pathMatches(value, query, mode string) bool {
	switch mode {
	case "exact":
		return value == query
	case "prefix":
		return strings.HasPrefix(value, query)
	case "suffix":
		return strings.HasSuffix(value, query)
	default:
		return strings.Contains(value, query)
	}
}

func (a *App) stats(w http.ResponseWriter, _ *http.Request, poolName string) {
	records, err := a.bundles(poolName)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "pool_read_failed", err.Error())
		return
	}

	var files, contentBytes, metadataBytes, verified, failed, unchecked int64
	var latest time.Time
	for _, record := range records {
		files += int64(len(record.Files.Records))
		contentBytes += record.State.SizeBytes
		size, err := directorySize(filepath.Join(record.Path, ".bundle"))
		if err != nil {
			writeError(w, http.StatusInternalServerError, "stats_failed", err.Error())
			return
		}
		metadataBytes += size
		if record.State.LastChecked.IsZero() {
			unchecked++
		} else if record.State.Verified {
			verified++
		} else {
			failed++
		}
		if record.State.LastChecked.After(latest) {
			latest = record.State.LastChecked
		}
	}
	integrity := map[string]any{"verified_bundles": verified, "failed_bundles": failed, "unchecked_bundles": unchecked, "last_verified_at": nil}
	if !latest.IsZero() {
		integrity["last_verified_at"] = latest
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"pool":         poolName,
		"generated_at": time.Now().UTC(),
		"storage": map[string]any{
			"bundle_count":   len(records),
			"file_count":     files,
			"content_bytes":  contentBytes,
			"metadata_bytes": metadataBytes,
			"total_bytes":    contentBytes + metadataBytes,
		},
		"integrity": integrity,
	})
}

func (a *App) migrateBundle(w http.ResponseWriter, _ *http.Request, poolName, checksum string) {
	record, err := a.bundle(poolName, checksum)
	if err != nil {
		a.bundleError(w, err)
		return
	}
	newChecksum, changed, err := bundlelib.Migrate(record.Path)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "migration_failed", err.Error())
		return
	}
	if !changed {
		writeJSON(w, http.StatusOK, map[string]any{"status": "current", "checksum": newChecksum, "version": 2})
		return
	}
	destination := filepath.Join(a.config.Pools[poolName].Root, newChecksum)
	if err := os.Rename(record.Path, destination); err != nil {
		if errors.Is(err, fs.ErrExist) {
			writeError(w, http.StatusConflict, "bundle_exists", "migrated checksum already exists in this pool")
			return
		}
		writeError(w, http.StatusInternalServerError, "migration_failed", err.Error())
		return
	}
	a.invalidateMetadata(poolName)
	migrated, err := a.bundle(poolName, newChecksum)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "migration_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "migrated", "checksum": newChecksum, "version": 2, "bundle": a.bundleResponse(poolName, migrated)})
}

func directorySize(root string) (int64, error) {
	var size int64
	err := filepath.WalkDir(root, func(_ string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		size += info.Size()
		return nil
	})
	return size, err
}
