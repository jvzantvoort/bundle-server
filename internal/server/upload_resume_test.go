package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jvzantvoort/bundle-server/internal/config"
	"github.com/jvzantvoort/bundle-server/internal/taskqueue"
	"github.com/jvzantvoort/bundle/checksum"
)

// Two files, so that a push can be interrupted halfway
var resumeFiles = map[string]string{"a.txt": "alpha", "dir/b.txt": "bravo!"}

type uploadState struct {
	ID           string   `json:"id"`
	Status       string   `json:"status"`
	MissingFiles []string `json:"missing_files"`
}

func uploadApp(t *testing.T, poolRoot string, ttl int) http.Handler {
	t.Helper()
	queue := taskqueue.NewMemoryQueue(1, nil)
	t.Cleanup(queue.Close)
	app, err := New(config.Config{
		Server: config.ServerConfig{
			Address: ":0", TaskWorkers: 1, MaxManifestEntries: 10,
			MaxFileBytes: 1 << 20, MaxBundleBytes: 1 << 20, UploadTTLSeconds: ttl,
		},
		Pools: map[string]config.PoolConfig{"default": {Root: poolRoot, Title: "Default"}},
	}, queue, []byte("openapi: 3.1.0\n"))
	if err != nil {
		t.Fatal(err)
	}
	return app.Handler()
}

func sha(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

func resumeRequest(title string) (string, string) {
	var records []checksum.ChecksumRecord
	var entries []string
	for path, content := range resumeFiles {
		records = append(records, checksum.ChecksumRecord{FilePath: path, Checksum: sha(content)})
		entries = append(entries, fmt.Sprintf(`{"path":%q,"checksum":%q,"size_bytes":%d}`, path, sha(content), len(content)))
	}
	bundleChecksum := checksum.ComputeBundleChecksum(records)
	body := fmt.Sprintf(`{"checksum":%q,"metadata":{"title":%q,"created_at":"2026-01-02T03:04:05Z","author":"test","version":2},"tags":["x"],"files":[%s]}`,
		bundleChecksum, title, strings.Join(entries, ","))
	return bundleChecksum, body
}

func send(t *testing.T, handler http.Handler, method, target, body string, header map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	for name, value := range header {
		req.Header.Set(name, value)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	return response
}

func openUpload(t *testing.T, handler http.Handler, key, body string, want int) uploadState {
	t.Helper()
	response := send(t, handler, http.MethodPost, "/api/v1/pools/default/uploads", body, map[string]string{"Idempotency-Key": key})
	if response.Code != want {
		t.Fatalf("create upload status = %d, want %d: %s", response.Code, want, response.Body.String())
	}
	var state uploadState
	if want == http.StatusCreated {
		if err := json.NewDecoder(response.Body).Decode(&state); err != nil {
			t.Fatal(err)
		}
	}
	return state
}

func putFile(t *testing.T, handler http.Handler, id, path string, want int) {
	t.Helper()
	content := resumeFiles[path]
	response := send(t, handler, http.MethodPut, "/api/v1/pools/default/uploads/"+id+"/files/"+strings.ReplaceAll(path, "/", "%2F"),
		content, map[string]string{"Content-SHA256": sha(content)})
	if response.Code != want {
		t.Fatalf("upload %s status = %d, want %d: %s", path, response.Code, want, response.Body.String())
	}
}

func TestIdempotencyKeyReplaysUpload(t *testing.T) {
	handler := uploadApp(t, t.TempDir(), 3600)
	_, body := resumeRequest("Resumed")

	first := openUpload(t, handler, "key-1", body, http.StatusCreated)
	again := openUpload(t, handler, "key-1", body, http.StatusCreated)
	if again.ID != first.ID {
		t.Fatalf("replayed upload id = %s, want %s", again.ID, first.ID)
	}

	_, other := resumeRequest("Different title")
	openUpload(t, handler, "key-1", other, http.StatusConflict)

	if openUpload(t, handler, "key-2", body, http.StatusCreated).ID == first.ID {
		t.Fatal("a different key reused the upload")
	}
}

func TestUploadResumesAfterRestart(t *testing.T) {
	poolRoot := t.TempDir()
	bundleChecksum, body := resumeRequest("Resumed")

	handler := uploadApp(t, poolRoot, 3600)
	session := openUpload(t, handler, "push-key", body, http.StatusCreated)
	putFile(t, handler, session.ID, "a.txt", http.StatusNoContent)
	// A partial write of the second file when the server stops
	staging := uploadStagingPath(poolRoot, session.ID)
	if err := os.MkdirAll(filepath.Join(staging, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staging, "dir", "b.txt"), []byte("bra"), 0o644); err != nil {
		t.Fatal(err)
	}

	handler = uploadApp(t, poolRoot, 3600)
	resumed := openUpload(t, handler, "push-key", body, http.StatusCreated)
	if resumed.ID != session.ID || resumed.Status != "open" {
		t.Fatalf("resumed upload = %+v, want open upload %s", resumed, session.ID)
	}
	if len(resumed.MissingFiles) != 1 || resumed.MissingFiles[0] != "dir/b.txt" {
		t.Fatalf("missing files = %v, want [dir/b.txt]", resumed.MissingFiles)
	}
	putFile(t, handler, session.ID, "a.txt", http.StatusConflict)
	putFile(t, handler, session.ID, "dir/b.txt", http.StatusNoContent)

	complete := "/api/v1/pools/default/uploads/" + session.ID + "/complete"
	if response := send(t, handler, http.MethodPost, complete, "", nil); response.Code != http.StatusCreated {
		t.Fatalf("complete status = %d: %s", response.Code, response.Body.String())
	}
	if _, err := os.Stat(filepath.Join(poolRoot, bundleChecksum, "dir", "b.txt")); err != nil {
		t.Fatal(err)
	}
	for _, leftover := range []string{uploadSessionPath(poolRoot, session.ID), uploadJournalPath(poolRoot, session.ID), staging} {
		if _, err := os.Stat(leftover); !os.IsNotExist(err) {
			t.Errorf("%s was not removed after completion", leftover)
		}
	}

	// Retries after completion
	if response := send(t, handler, http.MethodPost, complete, "", map[string]string{"Idempotency-Key": "c"}); response.Code != http.StatusCreated {
		t.Fatalf("retried complete status = %d: %s", response.Code, response.Body.String())
	}
	if response := send(t, handler, http.MethodPost, complete, "", nil); response.Code != http.StatusConflict {
		t.Fatalf("complete without key status = %d, want 409", response.Code)
	}
	if response := send(t, handler, http.MethodDelete, "/api/v1/pools/default/uploads/"+session.ID, "", nil); response.Code != http.StatusConflict {
		t.Fatalf("cancel of completed upload status = %d, want 409", response.Code)
	}
	if replay := openUpload(t, handler, "push-key", body, http.StatusCreated); replay.Status != "complete" {
		t.Fatalf("replayed status = %q, want complete", replay.Status)
	}

	// After a restart the session is gone and the bundle exists
	handler = uploadApp(t, poolRoot, 3600)
	openUpload(t, handler, "push-key", body, http.StatusConflict)
}

func TestCancelledUploadStartsOverWithSameKey(t *testing.T) {
	poolRoot := t.TempDir()
	handler := uploadApp(t, poolRoot, 3600)
	_, body := resumeRequest("Cancelled")

	session := openUpload(t, handler, "key", body, http.StatusCreated)
	putFile(t, handler, session.ID, "a.txt", http.StatusNoContent)
	if response := send(t, handler, http.MethodDelete, "/api/v1/pools/default/uploads/"+session.ID, "", nil); response.Code != http.StatusNoContent {
		t.Fatalf("cancel status = %d: %s", response.Code, response.Body.String())
	}
	for _, leftover := range []string{uploadSessionPath(poolRoot, session.ID), uploadJournalPath(poolRoot, session.ID), uploadStagingPath(poolRoot, session.ID)} {
		if _, err := os.Stat(leftover); !os.IsNotExist(err) {
			t.Errorf("%s was not removed by cancel", leftover)
		}
	}
	fresh := openUpload(t, handler, "key", body, http.StatusCreated)
	if fresh.ID == session.ID || len(fresh.MissingFiles) != 2 {
		t.Fatalf("upload after cancel = %+v, want a new upload missing both files", fresh)
	}
}

func TestExpiredAndOrphanedUploadsAreRemovedAtStartup(t *testing.T) {
	poolRoot := t.TempDir()
	handler := uploadApp(t, poolRoot, 1)
	_, body := resumeRequest("Expired")
	session := openUpload(t, handler, "key", body, http.StatusCreated)
	putFile(t, handler, session.ID, "a.txt", http.StatusNoContent)

	orphan := filepath.Join(poolRoot, ".upload-123456")
	if err := os.Mkdir(orphan, 0o755); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)

	handler = uploadApp(t, poolRoot, 3600)
	for _, leftover := range []string{orphan, uploadSessionPath(poolRoot, session.ID), uploadJournalPath(poolRoot, session.ID), uploadStagingPath(poolRoot, session.ID)} {
		if _, err := os.Stat(leftover); !os.IsNotExist(err) {
			t.Errorf("%s was not removed at startup", leftover)
		}
	}
	if fresh := openUpload(t, handler, "key", body, http.StatusCreated); fresh.ID == session.ID {
		t.Fatal("expired upload was replayed")
	}
}
