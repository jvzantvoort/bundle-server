package server

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
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
	config config.Config
	// createMu serializes session creation, so that two requests with the same
	// Idempotency-Key cannot both create a session
	createMu sync.Mutex
	mu       sync.RWMutex
	uploads  map[string]*upload
	keys     map[string]string // pool and Idempotency-Key to upload ID
}

type upload struct {
	ID          string
	Pool        string
	Path        string
	Checksum    string
	Key         string
	RequestHash string
	Metadata    metadata.Metadata
	Tags        []string
	Files       map[string]uploadFile
	CreatedAt   time.Time
	ExpiresAt   time.Time
	Status      string
	// completing is set while a complete request publishes the upload
	completing bool
}

type uploadFile struct {
	Checksum string `json:"checksum"`
	Size     int64  `json:"size_bytes"`
	Uploaded bool   `json:"-"`
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

const maxIdempotencyKeyLength = 255

// newUploadManager restores the upload sessions stored in the pools.
func newUploadManager(cfg config.Config) (*uploadManager, error) {
	m := &uploadManager{config: cfg, uploads: make(map[string]*upload), keys: make(map[string]string)}
	now := time.Now()
	for poolName, pool := range cfg.Pools {
		loaded, err := loadUploads(poolName, pool.Root, now)
		if err != nil {
			return nil, fmt.Errorf("failed to load uploads of pool %s: %w", poolName, err)
		}
		for _, upload := range loaded {
			m.register(upload)
		}
	}
	return m, nil
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
	key := r.Header.Get("Idempotency-Key")
	if len(key) > maxIdempotencyKeyLength {
		writeError(w, http.StatusBadRequest, "invalid_request", "Idempotency-Key is too long")
		return
	}
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
	requestHash, err := hashRequest(request)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "upload_create_failed", err.Error())
		return
	}

	a.uploads.createMu.Lock()
	defer a.uploads.createMu.Unlock()
	a.uploads.pruneExpired()

	// A retried request continues the session it created before
	if key != "" {
		if id, hash, ok := a.uploads.byKey(poolName, key); ok {
			if hash != requestHash {
				writeError(w, http.StatusConflict, "idempotency_key_reused", "Idempotency-Key was used for a different request")
				return
			}
			writeJSON(w, http.StatusCreated, a.uploadResponse(id))
			return
		}
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
	id, err := newUploadID()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "upload_create_failed", err.Error())
		return
	}
	staging := uploadStagingPath(pool.Root, id)
	if err := os.Mkdir(staging, 0o755); err != nil {
		writeError(w, http.StatusInternalServerError, "upload_create_failed", err.Error())
		return
	}
	files := make(map[string]uploadFile, len(request.Files))
	for _, file := range request.Files {
		files[file.Path] = uploadFile{Checksum: file.Checksum, Size: file.SizeBytes}
	}
	now := time.Now().UTC()
	upload := &upload{
		ID: id, Pool: poolName, Path: staging, Checksum: request.Checksum, Key: key, RequestHash: requestHash,
		Metadata: metadata.Metadata{Title: request.Metadata.Title, CreatedAt: request.Metadata.CreatedAt, Author: request.Metadata.Author, Version: 2},
		Tags:     request.Tags, Files: files, CreatedAt: now,
		ExpiresAt: now.Add(time.Duration(a.config.Server.UploadTTLSeconds) * time.Second), Status: "open",
	}
	if err := saveSession(pool.Root, upload); err != nil {
		_ = discardUpload(pool.Root, id)
		writeError(w, http.StatusInternalServerError, "upload_create_failed", err.Error())
		return
	}
	a.uploads.register(upload)
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
	upload, err := a.uploads.take(poolName, id)
	if err != nil {
		a.uploadError(w, err)
		return
	}
	if err := discardUpload(a.config.Pools[poolName].Root, upload.ID); err != nil {
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
	syncErr := file.Sync()
	closeErr := file.Close()
	if copyErr != nil || syncErr != nil || closeErr != nil {
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
	// The journal lets a restarted server know which files it already has
	if err := journalFile(a.config.Pools[poolName].Root, id, filePath); err != nil {
		_ = os.Remove(target)
		writeError(w, http.StatusInternalServerError, "upload_write_failed", err.Error())
		return
	}
	a.uploads.markUploaded(id, filePath)
	w.WriteHeader(http.StatusNoContent)
}

func (a *App) completeUpload(w http.ResponseWriter, r *http.Request, poolName, id string) {
	upload, err := a.uploads.ready(poolName, id)
	if errors.Is(err, errUploadComplete) && r.Header.Get("Idempotency-Key") != "" {
		// A retried complete request gets the bundle it published
		a.writePublished(w, poolName, upload.Checksum)
		return
	}
	if err != nil {
		a.uploadError(w, err)
		return
	}
	if status, code, err := a.publishUpload(poolName, upload); err != nil {
		a.uploads.reopen(id)
		writeError(w, status, code, err.Error())
		return
	}
	a.uploads.complete(id)
	removeSession(a.config.Pools[poolName].Root, id)
	a.invalidateMetadata(poolName)
	a.writePublished(w, poolName, upload.Checksum)
}

// publishUpload turns the staging directory into a bundle and moves it into the
// pool. On failure it returns the HTTP status and error code to report.
func (a *App) publishUpload(poolName string, upload *upload) (int, string, error) {
	created, err := bundlelib.Create(upload.Path, upload.Metadata.Title)
	if err != nil {
		a.resetStaging(upload)
		return http.StatusUnprocessableEntity, "bundle_create_failed", err
	}
	if created.Metadata.BundleChecksum != upload.Checksum {
		a.resetStaging(upload)
		return http.StatusUnprocessableEntity, "checksum_mismatch", errors.New("uploaded files do not produce the declared bundle checksum")
	}
	created.Metadata.Author = upload.Metadata.Author
	created.Metadata.CreatedAt = upload.Metadata.CreatedAt
	if err := created.Metadata.Save(upload.Path); err != nil {
		a.resetStaging(upload)
		return http.StatusInternalServerError, "bundle_publish_failed", err
	}
	tags := &tag.Tags{}
	tags.Add(upload.Tags...)
	if err := tags.Save(upload.Path); err != nil {
		a.resetStaging(upload)
		return http.StatusInternalServerError, "bundle_publish_failed", err
	}
	destination := filepath.Join(a.config.Pools[poolName].Root, upload.Checksum)
	if err := os.Rename(upload.Path, destination); err != nil {
		a.resetStaging(upload)
		if errors.Is(err, fs.ErrExist) {
			return http.StatusConflict, "bundle_exists", errors.New("bundle already exists in this pool")
		}
		return http.StatusInternalServerError, "bundle_publish_failed", err
	}
	return 0, "", nil
}

// resetStaging removes the .bundle directory a failed completion left behind,
// so that the upload can be completed again.
func (a *App) resetStaging(upload *upload) {
	if err := os.RemoveAll(filepath.Join(upload.Path, ".bundle")); err != nil {
		log.Printf("failed to reset upload %s: %v", upload.ID, err)
	}
}

func (a *App) writePublished(w http.ResponseWriter, poolName, bundleChecksum string) {
	record, err := a.bundle(poolName, bundleChecksum)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "bundle_publish_failed", err.Error())
		return
	}
	w.Header().Set("Location", a.apiPath("/pools/"+poolName+"/bundles/"+bundleChecksum))
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

// hashRequest identifies an upload request, to recognise a retry of it.
func hashRequest(request createUploadRequest) (string, error) {
	data, err := json.Marshal(request)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
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

func (m *uploadManager) register(upload *upload) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.uploads[upload.ID] = upload
	if upload.Key != "" {
		m.keys[upload.Pool+"\x00"+upload.Key] = upload.ID
	}
}

// byKey finds the session created with an Idempotency-Key.
func (m *uploadManager) byKey(pool, key string) (id, requestHash string, ok bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	id, ok = m.keys[pool+"\x00"+key]
	if !ok {
		return "", "", false
	}
	upload, ok := m.uploads[id]
	if !ok {
		return "", "", false
	}
	return id, upload.RequestHash, true
}

// forget removes a session from memory. The caller holds m.mu.
func (m *uploadManager) forget(upload *upload) {
	delete(m.uploads, upload.ID)
	if upload.Key != "" && m.keys[upload.Pool+"\x00"+upload.Key] == upload.ID {
		delete(m.keys, upload.Pool+"\x00"+upload.Key)
	}
}

// pruneExpired drops expired sessions and removes the files of open ones.
func (m *uploadManager) pruneExpired() {
	var discard []*upload
	m.mu.Lock()
	for _, upload := range m.uploads {
		if !expired(upload) || upload.completing {
			continue
		}
		m.forget(upload)
		if upload.Status == "open" {
			discard = append(discard, upload)
		}
	}
	m.mu.Unlock()
	for _, upload := range discard {
		if err := discardUpload(m.config.Pools[upload.Pool].Root, upload.ID); err != nil {
			log.Printf("failed to remove expired upload %s: %v", upload.ID, err)
		}
	}
}

func (m *uploadManager) take(pool, id string) (*upload, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	upload, ok := m.uploads[id]
	if !ok || upload.Pool != pool {
		return nil, fs.ErrNotExist
	}
	if upload.Status == "complete" {
		return nil, errUploadComplete
	}
	if upload.completing {
		return nil, errUploadCompleting
	}
	m.forget(upload)
	return upload, nil
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
	if upload.completing {
		return nil, errUploadCompleting
	}
	if _, ok := upload.Files[filePath]; !ok {
		return nil, errUploadFileNotFound
	}
	return upload, nil
}

func (m *uploadManager) markUploaded(id, filePath string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	upload, ok := m.uploads[id]
	if !ok {
		return
	}
	file := upload.Files[filePath]
	file.Uploaded = true
	upload.Files[filePath] = file
}

// ready claims a fully uploaded session for completion. For a completed session
// it returns the session together with errUploadComplete.
func (m *uploadManager) ready(pool, id string) (*upload, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	upload, ok := m.uploads[id]
	if !ok || upload.Pool != pool {
		return nil, fs.ErrNotExist
	}
	if upload.Status == "complete" {
		return upload, errUploadComplete
	}
	if expired(upload) {
		return nil, errUploadExpired
	}
	if upload.completing {
		return nil, errUploadCompleting
	}
	for _, file := range upload.Files {
		if !file.Uploaded {
			return nil, errUploadIncomplete
		}
	}
	upload.completing = true
	return upload, nil
}

func (m *uploadManager) reopen(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.uploads[id].completing = false
}

func (m *uploadManager) complete(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.uploads[id].completing = false
	m.uploads[id].Status = "complete"
}

var (
	errUploadExpired      = errors.New("upload has expired")
	errUploadComplete     = errors.New("upload is complete")
	errUploadCompleting   = errors.New("upload is being completed")
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
	case errors.Is(err, errUploadCompleting):
		writeError(w, http.StatusConflict, "upload_completing", err.Error())
	case errors.Is(err, errUploadExpired):
		writeError(w, http.StatusConflict, "upload_expired", err.Error())
	case errors.Is(err, errUploadFileNotFound):
		writeError(w, http.StatusNotFound, "file_not_found", err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "upload_failed", err.Error())
	}
}
