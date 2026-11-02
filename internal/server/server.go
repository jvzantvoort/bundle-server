package server

import (
	"context"
	_ "embed"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jvzantvoort/bundle-server/internal/config"
	"github.com/jvzantvoort/bundle-server/internal/scheduler"
	"github.com/jvzantvoort/bundle-server/internal/taskqueue"
	bundlelib "github.com/jvzantvoort/bundle/bundle"
	"github.com/jvzantvoort/bundle/checksum"
	"github.com/jvzantvoort/bundle/metadata"
	"github.com/jvzantvoort/bundle/state"
	"github.com/jvzantvoort/bundle/tag"
)

//go:embed swagger.html
var swaggerHTML []byte

type App struct {
	config  config.Config
	queue   taskqueue.Queue
	spec    []byte
	uploads *uploadManager
	cache   metadataCache
}

func New(cfg config.Config, queue taskqueue.Queue, spec []byte) (*App, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if queue == nil {
		return nil, errors.New("task queue is required")
	}
	if len(spec) == 0 {
		return nil, errors.New("OpenAPI specification is required")
	}
	apiURL := cfg.Server.APIPath()
	if cfg.Server.PublicURL != "" {
		apiURL = strings.TrimRight(cfg.Server.PublicURL, "/") + apiURL
	}
	renderedSpec := []byte(strings.ReplaceAll(string(spec), "https://{host}/api/v1", apiURL))
	return &App{config: cfg, queue: queue, spec: renderedSpec, uploads: newUploadManager(cfg), cache: metadataCache{pools: make(map[string]cacheEntry)}}, nil
}

func (a *App) Handler() http.Handler {
	mux := http.NewServeMux()
	api := a.config.Server.APIPath()
	mux.HandleFunc("GET "+a.config.Server.HealthURL(), a.health)
	mux.HandleFunc("GET "+a.config.Server.OpenAPIURL(), a.openAPI)
	mux.HandleFunc("GET "+a.config.Server.SwaggerURL(), a.swagger)
	mux.HandleFunc("GET "+api+"/capabilities", a.withAuth(a.capabilities))
	mux.HandleFunc("GET "+api+"/pools", a.withAuth(a.listPools))
	mux.HandleFunc("GET "+api+"/pools/{pool}", a.withAuth(a.getPool))
	mux.HandleFunc(api+"/pools/{pool}/", a.withAuth(a.poolRoutes))
	mux.HandleFunc("GET "+api+"/tasks/{id}", a.withAuth(a.getTask))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && !strings.HasPrefix(r.URL.Path, api+"/") {
			parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
			if len(parts) >= 3 {
				if _, ok := a.config.Pools[parts[0]]; ok {
					a.serveBundleFile(w, r, parts[0], parts[1], strings.Join(parts[2:], "/"))
					return
				}
			}
		}
		mux.ServeHTTP(w, r)
	})
}

func (a *App) apiPath(suffix string) string {
	return strings.TrimRight(a.config.Server.APIPath(), "/") + "/" + strings.TrimLeft(suffix, "/")
}

func (a *App) StartSchedules(ctx context.Context) (*scheduler.Scheduler, error) {
	jobs := make([]scheduler.Job, 0, len(a.config.Schedules))
	for _, scheduleConfig := range a.config.Schedules {
		interval, _ := time.ParseDuration(scheduleConfig.Interval)
		scheduleConfig := scheduleConfig
		jobs = append(jobs, scheduler.Job{
			Name: scheduleConfig.Name, Interval: interval, RunOnStart: scheduleConfig.RunOnStart,
			Run: func(ctx context.Context) { a.enqueueScheduledAction(ctx, scheduleConfig) },
		})
	}
	return scheduler.Start(ctx, jobs)
}

func (a *App) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a.config.Server.BearerToken != "" {
			if r.Header.Get("Authorization") != "Bearer "+a.config.Server.BearerToken {
				writeError(w, http.StatusUnauthorized, "unauthorized", "a valid bearer token is required")
				return
			}
		}
		next(w, r)
	}
}

func (a *App) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (a *App) openAPI(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
	_, _ = w.Write(a.spec)
}

func (a *App) swagger(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	page := strings.ReplaceAll(string(swaggerHTML), "__OPENAPI_URL__", a.config.Server.OpenAPIURL())
	_, _ = w.Write([]byte(page))
}

func (a *App) serveBundleFile(w http.ResponseWriter, r *http.Request, poolName, checksum, filePath string) {
	a.downloadFile(w, r, poolName, checksum, filePath)
}

func (a *App) capabilities(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"api_version":         "1.0.0",
		"server_version":      "dev",
		"bundle_versions":     []int{2},
		"checksum_algorithms": []string{"sha256"},
		"upload_limits": map[string]any{
			"max_manifest_entries": a.config.Server.MaxManifestEntries,
			"max_file_bytes":       a.config.Server.MaxFileBytes,
			"max_bundle_bytes":     a.config.Server.MaxBundleBytes,
			"upload_ttl_seconds":   a.config.Server.UploadTTLSeconds,
		},
	})
}

func (a *App) listPools(w http.ResponseWriter, _ *http.Request) {
	names := make([]string, 0, len(a.config.Pools))
	for name := range a.config.Pools {
		names = append(names, name)
	}
	sort.Strings(names)
	pools := make([]poolResponse, 0, len(names))
	for _, name := range names {
		pool := a.config.Pools[name]
		bundles, err := a.bundles(name)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "pool_read_failed", err.Error())
			return
		}
		pools = append(pools, poolResponse{Name: name, Title: pool.Title, BundleCount: len(bundles)})
	}
	writeJSON(w, http.StatusOK, map[string]any{"pools": pools})
}

func (a *App) getPool(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("pool")
	pool, ok := a.config.Pools[name]
	if !ok {
		writeError(w, http.StatusNotFound, "pool_not_found", "pool does not exist")
		return
	}
	bundles, err := a.bundles(name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "pool_read_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, poolResponse{Name: name, Title: pool.Title, BundleCount: len(bundles)})
}

func (a *App) poolRoutes(w http.ResponseWriter, r *http.Request) {
	poolName := r.PathValue("pool")
	if _, ok := a.config.Pools[poolName]; !ok {
		writeError(w, http.StatusNotFound, "pool_not_found", "pool does not exist")
		return
	}
	suffix := strings.TrimPrefix(r.URL.Path, a.apiPath("/pools/"+poolName+"/"))
	switch {
	case suffix == "bundles" && r.Method == http.MethodGet:
		a.listBundles(w, r, poolName)
	case suffix == "bundles/query" && r.Method == http.MethodPost:
		a.queryBundles(w, r, poolName)
	case suffix == "stats" && r.Method == http.MethodGet:
		a.stats(w, r, poolName)
	case suffix == "uploads" && r.Method == http.MethodPost:
		a.createUpload(w, r, poolName)
	case strings.HasPrefix(suffix, "uploads/"):
		a.uploadRoutes(w, r, poolName, strings.TrimPrefix(suffix, "uploads/"))
	case strings.HasPrefix(suffix, "bundles/"):
		a.bundleRoutes(w, r, poolName, strings.TrimPrefix(suffix, "bundles/"))
	default:
		writeError(w, http.StatusNotFound, "not_found", "resource does not exist")
	}
}

func (a *App) bundleRoutes(w http.ResponseWriter, r *http.Request, poolName, suffix string) {
	parts := strings.Split(suffix, "/")
	if len(parts) == 0 || !validChecksum(parts[0]) {
		writeError(w, http.StatusNotFound, "bundle_not_found", "bundle does not exist")
		return
	}
	checksum := parts[0]
	if len(parts) == 1 {
		switch r.Method {
		case http.MethodGet:
			a.getBundle(w, r, poolName, checksum)
		case http.MethodPatch:
			a.updateBundle(w, r, poolName, checksum)
		default:
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method is not allowed")
		}
		return
	}
	switch parts[1] {
	case "files":
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method is not allowed")
			return
		}
		if len(parts) == 2 {
			a.listFiles(w, r, poolName, checksum)
			return
		}
		a.downloadFile(w, r, poolName, checksum, strings.Join(parts[2:], "/"))
	case "tags":
		a.tags(w, r, poolName, checksum)
	case "verify":
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method is not allowed")
			return
		}
		a.enqueueVerify(w, r, poolName, checksum)
	case "migrate":
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method is not allowed")
			return
		}
		a.migrateBundle(w, r, poolName, checksum)
	default:
		writeError(w, http.StatusNotFound, "not_found", "resource does not exist")
	}
}

func (a *App) listBundles(w http.ResponseWriter, r *http.Request, poolName string) {
	bundles, err := a.bundles(poolName)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "pool_read_failed", err.Error())
		return
	}
	title, author, requiredTag := strings.ToLower(r.URL.Query().Get("title")), r.URL.Query().Get("author"), r.URL.Query().Get("tag")
	verifiedFilter, hasVerified, err := optionalBool(r.URL.Query().Get("verified"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_query", "verified must be true or false")
		return
	}
	filtered := make([]bundleRecord, 0, len(bundles))
	for _, bundle := range bundles {
		if title != "" && !strings.Contains(strings.ToLower(bundle.Metadata.Title), title) {
			continue
		}
		if author != "" && bundle.Metadata.Author != author {
			continue
		}
		if requiredTag != "" && !contains(bundle.Tags.List(), requiredTag) {
			continue
		}
		if hasVerified && bundle.State.Verified != verifiedFilter {
			continue
		}
		filtered = append(filtered, bundle)
	}
	sort.Slice(filtered, func(i, j int) bool { return filtered[i].Metadata.Title < filtered[j].Metadata.Title })
	page, pageSize := pagination(r)
	writeJSON(w, http.StatusOK, pageBundles(filtered, page, pageSize))
}

func (a *App) getBundle(w http.ResponseWriter, _ *http.Request, poolName, checksum string) {
	record, err := a.bundle(poolName, checksum)
	if errors.Is(err, fs.ErrNotExist) {
		writeError(w, http.StatusNotFound, "bundle_not_found", "bundle does not exist")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "bundle_read_failed", err.Error())
		return
	}
	w.Header().Set("ETag", record.Revision)
	writeJSON(w, http.StatusOK, a.bundleResponse(poolName, record))
}

func (a *App) updateBundle(w http.ResponseWriter, r *http.Request, poolName, checksum string) {
	record, err := a.bundle(poolName, checksum)
	if err != nil {
		a.bundleError(w, err)
		return
	}
	if !matchesRevision(r, record.Revision) {
		writeError(w, http.StatusPreconditionFailed, "stale_revision", "bundle changed; fetch its current ETag and retry")
		return
	}
	var request struct {
		Title string `json:"title"`
	}
	if err := decodeJSON(r, &request); err != nil || len(request.Title) > 256 {
		writeError(w, http.StatusBadRequest, "invalid_request", "title is required and must be at most 256 characters")
		return
	}
	if err := metadata.UpdateTitle(record.Path, request.Title); err != nil {
		writeError(w, http.StatusInternalServerError, "bundle_update_failed", err.Error())
		return
	}
	a.invalidateMetadata(poolName)
	a.getBundle(w, r, poolName, checksum)
}

func (a *App) listFiles(w http.ResponseWriter, r *http.Request, poolName, checksum string) {
	record, err := a.bundle(poolName, checksum)
	if err != nil {
		a.bundleError(w, err)
		return
	}
	entries := manifest(record)
	page, pageSize := pagination(r)
	start, end := pageBounds(len(entries), page, pageSize)
	writeJSON(w, http.StatusOK, map[string]any{"items": entries[start:end], "page": page, "page_size": pageSize, "total": len(entries)})
}

func (a *App) downloadFile(w http.ResponseWriter, r *http.Request, poolName, checksum, encodedPath string) {
	record, err := a.bundle(poolName, checksum)
	if err != nil {
		a.bundleError(w, err)
		return
	}
	filePath, err := safeFilePath(encodedPath)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_file_path", err.Error())
		return
	}
	var expected string
	for _, entry := range record.Files.Records {
		if entry.FilePath == filePath {
			expected = entry.Checksum
			break
		}
	}
	if expected == "" {
		writeError(w, http.StatusNotFound, "file_not_found", "file does not exist in bundle manifest")
		return
	}
	file, err := os.Open(filepath.Join(record.Path, filepath.FromSlash(filePath)))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			writeError(w, http.StatusNotFound, "file_not_found", "file does not exist")
			return
		}
		writeError(w, http.StatusInternalServerError, "file_read_failed", err.Error())
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "file_read_failed", err.Error())
		return
	}
	w.Header().Set("ETag", expected)
	w.Header().Set("Content-SHA256", expected)
	w.Header().Set("Accept-Ranges", "bytes")
	http.ServeContent(w, r, path.Base(filePath), info.ModTime(), file)
}

func (a *App) tags(w http.ResponseWriter, r *http.Request, poolName, checksum string) {
	record, err := a.bundle(poolName, checksum)
	if err != nil {
		a.bundleError(w, err)
		return
	}
	if r.Method == http.MethodGet {
		tags := record.Tags.List()
		w.Header().Set("ETag", record.Revision)
		writeJSON(w, http.StatusOK, map[string]any{"tags": tags, "total": len(tags)})
		return
	}
	if r.Method != http.MethodPost && r.Method != http.MethodDelete {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method is not allowed")
		return
	}
	if !matchesRevision(r, record.Revision) {
		writeError(w, http.StatusPreconditionFailed, "stale_revision", "bundle changed; fetch its current ETag and retry")
		return
	}
	var request struct {
		Tags []string `json:"tags"`
	}
	if err := decodeJSON(r, &request); err != nil || len(request.Tags) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_request", "tags must contain at least one value")
		return
	}
	if r.Method == http.MethodPost {
		record.Tags.Add(request.Tags...)
	} else {
		record.Tags.Remove(request.Tags...)
	}
	if err := record.Tags.Save(record.Path); err != nil {
		writeError(w, http.StatusInternalServerError, "tag_update_failed", err.Error())
		return
	}
	a.invalidateMetadata(poolName)
	tags := record.Tags.List()
	revision, _ := revision(record.Path)
	w.Header().Set("ETag", revision)
	writeJSON(w, http.StatusOK, map[string]any{"tags": tags, "total": len(tags)})
}

func (a *App) enqueueVerify(w http.ResponseWriter, r *http.Request, poolName, checksum string) {
	record, err := a.bundle(poolName, checksum)
	if err != nil {
		a.bundleError(w, err)
		return
	}
	task, err := a.queue.Enqueue(r.Context(), "verify", func(context context.Context) (any, error) {
		verified, corrupted, err := bundlelib.Verify(record.Path)
		if err != nil {
			return nil, err
		}
		a.invalidateMetadata(poolName)
		return map[string]any{
			"verified":        verified,
			"checked_at":      time.Now().UTC(),
			"files_checked":   len(record.Files.Records),
			"corrupted_files": corrupted,
		}, nil
	})
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "queue_unavailable", err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, task)
}

func (a *App) getTask(w http.ResponseWriter, r *http.Request) {
	task, ok := a.queue.Get(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "task_not_found", "task does not exist")
		return
	}
	writeJSON(w, http.StatusOK, task)
}

type bundleRecord struct {
	Path     string
	Metadata *metadata.Metadata
	State    *state.State
	Tags     *tag.Tags
	Files    *checksum.ChecksumFile
	Revision string
}
