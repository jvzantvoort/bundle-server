package server

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jvzantvoort/bundle-server/internal/config"
	bundlelib "github.com/jvzantvoort/bundle/bundle"
	"github.com/jvzantvoort/bundle/checksum"
	"github.com/jvzantvoort/bundle/metadata"
	"github.com/jvzantvoort/bundle/tag"
)

type uploadManager struct {
	config  config.Config
	mu      sync.RWMutex
	uploads map[string]*upload
}

type upload struct {
	ID        string
	Pool      string
	Path      string
	Checksum  string
	Metadata  metadata.Metadata
	Tags      []string
	Files     map[string]uploadFile
	CreatedAt time.Time
	ExpiresAt time.Time
	Status    string
}

type uploadFile struct {
	Checksum string
	Size     int64
	Uploaded bool
}

type createUploadRequest struct {
	Checksum string             `json:"checksum"`
	Metadata uploadMetadata     `json:"metadata"`
	Tags     []string           `json:"tags"`
	Files    []manifestResponse `json:"files"`
}

type uploadMetadata struct {
	Title     string    `json:"title"`
	CreatedAt time.Time `json:"created_at"`
	Author    string    `json:"author"`
	Version   int       `json:"version"`
}

func newUploadManager(cfg config.Config) *uploadManager {
	return &uploadManager{config: cfg, uploads: make(map[string]*upload)}
}

func (a *App) uploadRoutes(w http.ResponseWriter, r *http.Request, poolName, suffix string) {
	parts := strings.Split(suffix, "/")
	if len(parts) == 0 || parts[0] == "" {
		writeError(w, http.StatusNotFound, "upload_not_found", "upload does not exist")
		return
	}
	id := parts[0]
	switch {
	case len(parts) == 1 && r.Method == http.MethodGet:
		a.getUpload(w, r, poolName, id)
	case len(parts) == 1 && r.Method == http.MethodDelete:
		a.cancelUpload(w, r, poolName, id)
	case len(parts) == 2 && parts[1] == "complete" && r.Method == http.MethodPost:
		a.completeUpload(w, r, poolName, id)
	case len(parts) >= 3 && parts[1] == "files" && r.Method == http.MethodPut:
		a.uploadFile(w, r, poolName, id, strings.Join(parts[2:], "/"))
	default:
		writeError(w, http.StatusNotFound, "not_found", "resource does not exist")
	}
}

func (a *App) createUpload(w http.ResponseWriter, r *http.Request, poolName string) {
	var request createUploadRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "upload request must be valid JSON")
		return
	}
	if err := validateUploadRequest(a.config, request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if expected := checksum.ComputeBundleChecksum(toChecksumRecords(request.Files)); expected != request.Checksum {
		writeError(w, http.StatusUnprocessableEntity, "invalid_checksum", "checksum does not match the manifest")
		return
	}
	pool := a.config.Pools[poolName]
	if _, err := os.Stat(filepath.Join(pool.Root, request.Checksum)); err == nil {
		writeError(w, http.StatusConflict, "bundle_exists", "bundle already exists in this pool")
		return
	} else if !errors.Is(err, fs.ErrNotExist) {
		writeError(w, http.StatusInternalServerError, "upload_create_failed", err.Error())
		return
	}
	if err := os.MkdirAll(pool.Root, 0o755); err != nil {
		writeError(w, http.StatusInternalServerError, "upload_create_failed", err.Error())
		return
	}
	staging, err := os.MkdirTemp(pool.Root, ".upload-")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "upload_create_failed", err.Error())
		return
	}
	id, err := newUploadID()
	if err != nil {
		_ = os.RemoveAll(staging)
		writeError(w, http.StatusInternalServerError, "upload_create_failed", err.Error())
		return
	}
	files := make(map[string]uploadFile, len(request.Files))
	for _, file := range request.Files {
		files[file.Path] = uploadFile{Checksum: file.Checksum, Size: file.SizeBytes}
	}
	now := time.Now().UTC()
	a.uploads.mu.Lock()
	a.uploads.uploads[id] = &upload{
		ID: id, Pool: poolName, Path: staging, Checksum: request.Checksum,
		Metadata: metadata.Metadata{Title: request.Metadata.Title, CreatedAt: request.Metadata.CreatedAt, Author: request.Metadata.Author, Version: 2},
		Tags:     request.Tags, Files: files, CreatedAt: now,
		ExpiresAt: now.Add(time.Duration(a.config.Server.UploadTTLSeconds) * time.Second), Status: "open",
	}
	a.uploads.mu.Unlock()
	writeJSON(w, http.StatusCreated, a.uploadResponse(id))
}

func (a *App) getUpload(w http.ResponseWriter, _ *http.Request, poolName, id string) {
	if !a.uploads.belongsTo(poolName, id) {
		writeError(w, http.StatusNotFound, "upload_not_found", "upload does not exist")
		return
	}
	writeJSON(w, http.StatusOK, a.uploadResponse(id))
}

func (a *App) cancelUpload(w http.ResponseWriter, _ *http.Request, poolName, id string) {
	upload, ok := a.uploads.take(poolName, id)
	if !ok {
		writeError(w, http.StatusNotFound, "upload_not_found", "upload does not exist")
		return
	}
	if upload.Status == "complete" {
		writeError(w, http.StatusConflict, "upload_complete", "completed uploads cannot be cancelled")
		return
	}
	if err := os.RemoveAll(upload.Path); err != nil {
		writeError(w, http.StatusInternalServerError, "upload_cancel_failed", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *App) uploadFile(w http.ResponseWriter, r *http.Request, poolName, id, rawPath string) {
	filePath, err := safeFilePath(rawPath)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_file_path", err.Error())
		return
	}
	upload, err := a.uploads.file(poolName, id, filePath)
	if err != nil {
		a.uploadError(w, err)
		return
	}
	if r.Header.Get("Content-SHA256") != upload.Files[filePath].Checksum {
		writeError(w, http.StatusUnprocessableEntity, "checksum_mismatch", "Content-SHA256 must match the manifest")
		return
	}
	target := filepath.Join(upload.Path, filepath.FromSlash(filePath))
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		writeError(w, http.StatusInternalServerError, "upload_write_failed", err.Error())
		return
	}
	file, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if errors.Is(err, fs.ErrExist) {
		writeError(w, http.StatusConflict, "file_already_uploaded", "file is already uploaded")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "upload_write_failed", err.Error())
		return
	}
	hash := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(file, hash), io.LimitReader(r.Body, upload.Files[filePath].Size+1))
	closeErr := file.Close()
	if copyErr != nil || closeErr != nil {
		_ = os.Remove(target)
		writeError(w, http.StatusInternalServerError, "upload_write_failed", "could not write upload body")
		return
	}
	actual := hex.EncodeToString(hash.Sum(nil))
	expected := upload.Files[filePath]
	if written != expected.Size || actual != expected.Checksum {
		_ = os.Remove(target)
		writeError(w, http.StatusUnprocessableEntity, "checksum_mismatch", "file content does not match the manifest")
		return
	}
	a.uploads.markUploaded(id, filePath)
	w.WriteHeader(http.StatusNoContent)
}

func (a *App) completeUpload(w http.ResponseWriter, _ *http.Request, poolName, id string) {
	upload, err := a.uploads.ready(poolName, id)
	if err != nil {
		a.uploadError(w, err)
		return
	}
	created, err := bundlelib.Create(upload.Path, upload.Metadata.Title)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "bundle_create_failed", err.Error())
		return
	}
	if created.Metadata.BundleChecksum != upload.Checksum {
		writeError(w, http.StatusUnprocessableEntity, "checksum_mismatch", "uploaded files do not produce the declared bundle checksum")
		return
	}
	created.Metadata.Author = upload.Metadata.Author
	created.Metadata.CreatedAt = upload.Metadata.CreatedAt
	if err := created.Metadata.Save(upload.Path); err != nil {
		writeError(w, http.StatusInternalServerError, "bundle_publish_failed", err.Error())
		return
	}
	tags := &tag.Tags{}
	tags.Add(upload.Tags...)
	if err := tags.Save(upload.Path); err != nil {
		writeError(w, http.StatusInternalServerError, "bundle_publish_failed", err.Error())
		return
	}
	destination := filepath.Join(a.config.Pools[poolName].Root, upload.Checksum)
	if err := os.Rename(upload.Path, destination); err != nil {
		if errors.Is(err, fs.ErrExist) {
			writeError(w, http.StatusConflict, "bundle_exists", "bundle already exists in this pool")
			return
		}
		writeError(w, http.StatusInternalServerError, "bundle_publish_failed", err.Error())
		return
	}
	a.uploads.complete(id)
	a.invalidateMetadata(poolName)
	record, err := a.bundle(poolName, upload.Checksum)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "bundle_publish_failed", err.Error())
		return
	}
	w.Header().Set("Location", a.apiPath("/pools/"+poolName+"/bundles/"+upload.Checksum))
	writeJSON(w, http.StatusCreated, a.bundleResponse(poolName, record))
}

func validateUploadRequest(cfg config.Config, request createUploadRequest) error {
	if !validChecksum(request.Checksum) || request.Metadata.Version != 2 || request.Metadata.Author == "" || request.Metadata.CreatedAt.IsZero() {
		return errors.New("checksum, version 2 metadata, author, and created_at are required")
	}
	if len(request.Metadata.Title) > 256 || len(request.Files) == 0 || len(request.Files) > cfg.Server.MaxManifestEntries {
		return errors.New("manifest exceeds server limits")
	}
	var total int64
	seen := make(map[string]struct{}, len(request.Files))
	for _, file := range request.Files {
		if _, err := safeFilePath(file.Path); err != nil || !validChecksum(file.Checksum) || file.SizeBytes < 0 || file.SizeBytes > cfg.Server.MaxFileBytes {
			return errors.New("manifest contains an invalid file")
		}
		if _, exists := seen[file.Path]; exists {
			return errors.New("manifest contains duplicate paths")
		}
		seen[file.Path] = struct{}{}
		total += file.SizeBytes
		if total > cfg.Server.MaxBundleBytes {
			return errors.New("manifest exceeds maximum bundle size")
		}
	}
	return nil
}

func toChecksumRecords(files []manifestResponse) []checksum.ChecksumRecord {
	records := make([]checksum.ChecksumRecord, 0, len(files))
	for _, file := range files {
		records = append(records, checksum.ChecksumRecord{FilePath: file.Path, Checksum: file.Checksum})
	}
	return records
}

func newUploadID() (string, error) {
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes), nil
}

func (a *App) uploadResponse(id string) map[string]any {
	a.uploads.mu.RLock()
	defer a.uploads.mu.RUnlock()
	upload := a.uploads.uploads[id]
	missing := make([]string, 0)
	var uploaded int
	var uploadedBytes int64
	for path, file := range upload.Files {
		if file.Uploaded {
			uploaded++
			uploadedBytes += file.Size
		} else {
			missing = append(missing, path)
		}
	}
	sort.Strings(missing)
	return map[string]any{
		"id": id, "pool": upload.Pool, "checksum": upload.Checksum, "status": upload.Status,
		"file_count": len(upload.Files), "uploaded_file_count": uploaded, "uploaded_bytes": uploadedBytes,
		"missing_files": missing, "created_at": upload.CreatedAt, "expires_at": upload.ExpiresAt,
		"links": map[string]string{
			"self":     a.apiPath("/pools/" + upload.Pool + "/uploads/" + id),
			"complete": a.apiPath("/pools/" + upload.Pool + "/uploads/" + id + "/complete"),
		},
	}
}

func (m *uploadManager) belongsTo(pool, id string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	upload, ok := m.uploads[id]
	return ok && upload.Pool == pool && !expired(upload)
}

func (m *uploadManager) take(pool, id string) (*upload, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	upload, ok := m.uploads[id]
	if !ok || upload.Pool != pool {
		return nil, false
	}
	delete(m.uploads, id)
	return upload, true
}

func (m *uploadManager) file(pool, id, filePath string) (*upload, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	upload, ok := m.uploads[id]
	if !ok || upload.Pool != pool {
		return nil, fs.ErrNotExist
	}
	if expired(upload) {
		return nil, errUploadExpired
	}
	if upload.Status != "open" {
		return nil, errUploadComplete
	}
	if _, ok := upload.Files[filePath]; !ok {
		return nil, errUploadFileNotFound
	}
	return upload, nil
}

func (m *uploadManager) markUploaded(id, filePath string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	upload := m.uploads[id]
	file := upload.Files[filePath]
	file.Uploaded = true
	upload.Files[filePath] = file
}

func (m *uploadManager) ready(pool, id string) (*upload, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	upload, ok := m.uploads[id]
	if !ok || upload.Pool != pool {
		return nil, fs.ErrNotExist
	}
	if expired(upload) {
		return nil, errUploadExpired
	}
	if upload.Status == "complete" {
		return nil, errUploadComplete
	}
	for _, file := range upload.Files {
		if !file.Uploaded {
			return nil, errUploadIncomplete
		}
	}
	return upload, nil
}

func (m *uploadManager) complete(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.uploads[id].Status = "complete"
}

var (
	errUploadExpired      = errors.New("upload has expired")
	errUploadComplete     = errors.New("upload is complete")
	errUploadIncomplete   = errors.New("upload has missing files")
	errUploadFileNotFound = errors.New("file is not declared in the manifest")
)

func expired(upload *upload) bool {
	return time.Now().After(upload.ExpiresAt)
}

func (a *App) uploadError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		writeError(w, http.StatusNotFound, "upload_not_found", "upload does not exist")
	case errors.Is(err, errUploadIncomplete):
		writeError(w, http.StatusConflict, "upload_incomplete", err.Error())
	case errors.Is(err, errUploadComplete):
		writeError(w, http.StatusConflict, "upload_complete", err.Error())
	case errors.Is(err, errUploadExpired):
		writeError(w, http.StatusConflict, "upload_expired", err.Error())
	case errors.Is(err, errUploadFileNotFound):
		writeError(w, http.StatusNotFound, "file_not_found", err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "upload_failed", err.Error())
	}
}
