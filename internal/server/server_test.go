package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jvzantvoort/bundle-server/internal/config"
	"github.com/jvzantvoort/bundle-server/internal/taskqueue"
	bundlelib "github.com/jvzantvoort/bundle/bundle"
	"github.com/jvzantvoort/bundle/checksum"
)

func TestBundleEndpoints(t *testing.T) {
	app, checksum := testApp(t)
	handler := app.Handler()

	response := request(t, handler, http.MethodGet, "/default/"+checksum+"/hello.txt", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("serve bundle file status = %d, want %d: %s", response.Code, http.StatusOK, response.Body.String())
	}
	if contentType := response.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "text/plain") {
		t.Fatalf("serve bundle file content type = %q, want text/plain", contentType)
	}
	if response.Body.String() != "hello" {
		t.Fatalf("serve bundle file body = %q, want %q", response.Body.String(), "hello")
	}

	response = request(t, handler, http.MethodGet, "/api/v1/pools/default/bundles", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("list bundles status = %d, want %d", response.Code, http.StatusOK)
	}

	response = request(t, handler, http.MethodGet, "/api/v1/pools/default/bundles/"+checksum, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("get bundle status = %d, want %d", response.Code, http.StatusOK)
	}
	etag := response.Header().Get("ETag")
	if etag == "" {
		t.Fatal("get bundle did not provide an ETag")
	}

	rename := httptest.NewRequest(http.MethodPatch, "/api/v1/pools/default/bundles/"+checksum, strings.NewReader(`{"title":"Renamed"}`))
	rename.Header.Set("Content-Type", "application/json")
	rename.Header.Set("If-Match", etag)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, rename)
	if response.Code != http.StatusOK {
		t.Fatalf("rename status = %d, want %d: %s", response.Code, http.StatusOK, response.Body.String())
	}

	response = request(t, handler, http.MethodPost, "/api/v1/pools/default/bundles/query", strings.NewReader(`{"file":{"checksum":"`+fileChecksum+`"}}`))
	if response.Code != http.StatusOK {
		t.Fatalf("query status = %d, want %d: %s", response.Code, http.StatusOK, response.Body.String())
	}
	var result struct {
		Total int `json:"total"`
	}
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if result.Total != 1 {
		t.Fatalf("query total = %d, want 1", result.Total)
	}
}

func TestVerifyQueuesTaskAndSwaggerIsAvailable(t *testing.T) {
	app, checksum := testApp(t)
	handler := app.Handler()

	response := request(t, handler, http.MethodGet, "/swagger/", nil)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "SwaggerUIBundle") {
		t.Fatalf("swagger response = %d %q", response.Code, response.Body.String())
	}
	response = request(t, handler, http.MethodPost, "/api/v1/pools/default/bundles/"+checksum+"/verify", nil)
	if response.Code != http.StatusAccepted {
		t.Fatalf("verify status = %d, want %d: %s", response.Code, http.StatusAccepted, response.Body.String())
	}
	var task taskqueue.Task
	if err := json.NewDecoder(response.Body).Decode(&task); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for task.Status == taskqueue.StatusQueued || task.Status == taskqueue.StatusRunning {
		if time.Now().After(deadline) {
			t.Fatal("queued task did not finish")
		}
		time.Sleep(time.Millisecond)
		response = request(t, handler, http.MethodGet, "/api/v1/tasks/"+task.ID, nil)
		if err := json.NewDecoder(response.Body).Decode(&task); err != nil {
			t.Fatal(err)
		}
	}
	if task.Status != taskqueue.StatusDone {
		t.Fatalf("task status = %q, want %q; error = %s", task.Status, taskqueue.StatusDone, task.Error)
	}
}

func TestUploadPublishesVerifiedBundle(t *testing.T) {
	app, _ := testApp(t)
	handler := app.Handler()
	bundleChecksum := checksum.ComputeBundleChecksum([]checksum.ChecksumRecord{{FilePath: "upload.txt", Checksum: fileChecksum}})
	body := fmt.Sprintf(`{"checksum":%q,"metadata":{"title":"Uploaded","created_at":"2026-01-02T03:04:05Z","author":"test","version":2},"files":[{"path":"upload.txt","checksum":%q,"size_bytes":5}]}`, bundleChecksum, fileChecksum)
	response := request(t, handler, http.MethodPost, "/api/v1/pools/default/uploads", strings.NewReader(body))
	if response.Code != http.StatusCreated {
		t.Fatalf("create upload status = %d, want %d: %s", response.Code, http.StatusCreated, response.Body.String())
	}
	var upload struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(response.Body).Decode(&upload); err != nil {
		t.Fatal(err)
	}
	put := httptest.NewRequest(http.MethodPut, "/api/v1/pools/default/uploads/"+upload.ID+"/files/upload.txt", strings.NewReader("hello"))
	put.Header.Set("Content-SHA256", fileChecksum)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, put)
	if response.Code != http.StatusNoContent {
		t.Fatalf("upload file status = %d, want %d: %s", response.Code, http.StatusNoContent, response.Body.String())
	}
	response = request(t, handler, http.MethodPost, "/api/v1/pools/default/uploads/"+upload.ID+"/complete", nil)
	if response.Code != http.StatusCreated {
		t.Fatalf("complete upload status = %d, want %d: %s", response.Code, http.StatusCreated, response.Body.String())
	}
}

func TestConfiguredRoutesAndSwaggerSpec(t *testing.T) {
	app, _ := testApp(t)
	cfg := app.config
	cfg.Server.APIBasePath = "/bundle-api"
	cfg.Server.HealthPath = "/status"
	cfg.Server.OpenAPIPath = "/documentation/openapi.yaml"
	cfg.Server.SwaggerPath = "/documentation/swagger/"
	cfg.Server.PublicURL = "https://bundles.example.test"
	configured, err := New(cfg, app.queue, []byte("servers:\n  - url: https://{host}/api/v1\n"))
	if err != nil {
		t.Fatal(err)
	}
	handler := configured.Handler()
	if response := request(t, handler, http.MethodGet, "/status", nil); response.Code != http.StatusOK {
		t.Fatalf("health status = %d, want %d", response.Code, http.StatusOK)
	}
	response := request(t, handler, http.MethodGet, "/documentation/swagger/", nil)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `url: "/documentation/openapi.yaml"`) {
		t.Fatalf("swagger response = %d %q", response.Code, response.Body.String())
	}
	response = request(t, handler, http.MethodGet, "/documentation/openapi.yaml", nil)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "https://bundles.example.test/bundle-api") {
		t.Fatalf("OpenAPI response = %d %q", response.Code, response.Body.String())
	}
}

func TestCacheMetadataScheduleAction(t *testing.T) {
	app, _ := testApp(t)
	result, err := app.runScheduledAction("default", "cache_metadata")
	if err != nil {
		t.Fatal(err)
	}
	if result.(map[string]int)["cached_bundles"] != 1 {
		t.Fatalf("cached bundles = %d, want 1", result.(map[string]int)["cached_bundles"])
	}
	if _, ok := app.cache.get("default"); !ok {
		t.Fatal("metadata cache was not populated")
	}
}

const fileChecksum = "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"

func testApp(t *testing.T) (*App, string) {
	t.Helper()
	root := t.TempDir()
	poolRoot := filepath.Join(root, "pool")
	bundlePath := filepath.Join(root, "source")
	if err := os.MkdirAll(bundlePath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundlePath, "hello.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	bundle, err := bundlelib.Create(bundlePath, "Test Bundle")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(poolRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(bundlePath, filepath.Join(poolRoot, bundle.Metadata.BundleChecksum)); err != nil {
		t.Fatal(err)
	}
	queue := taskqueue.NewMemoryQueue(1, nil)
	t.Cleanup(queue.Close)
	app, err := New(config.Config{
		Server: config.ServerConfig{
			Address: ":0", TaskWorkers: 1, MaxManifestEntries: 1,
			MaxFileBytes: 1 << 20, MaxBundleBytes: 1 << 20, UploadTTLSeconds: 1,
		},
		Pools: map[string]config.PoolConfig{"default": {Root: poolRoot, Title: "Default"}},
	}, queue, []byte("openapi: 3.1.0\n"))
	if err != nil {
		t.Fatal(err)
	}
	return app, bundle.Metadata.BundleChecksum
}

func request(t *testing.T, handler http.Handler, method, target string, body io.Reader) *httptest.ResponseRecorder {
	t.Helper()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(method, target, body))
	return response
}
