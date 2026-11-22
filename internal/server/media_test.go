package server

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

const testMedia = `{
  "version": 1,
  "generator": "bundle-python 0.1.0",
  "updated_at": "2026-11-22T10:00:00Z",
  "devices": [{"make": "Canon", "model": "EOS R6", "files": 1}],
  "files": {
    "hello.txt": {
      "type": "image", "format": "JPEG",
      "resolution": {"width": 4000, "height": 3000},
      "taken_at": "2026:11:01 12:00:00",
      "device": {"make": "Canon", "model": "EOS R6"},
      "gps": {"latitude": 52.1, "longitude": 5.2},
      "sha256": "abc",
      "exif": {"Make": "Canon", "ISO": 100}
    }
  }
}`

func TestBundleMedia(t *testing.T) {
	app, checksum := testApp(t)
	handler := app.Handler()
	base := "/api/v1/pools/default/bundles/" + checksum

	var bundle struct {
		Links map[string]string `json:"links"`
	}
	call(t, handler, http.MethodGet, base, "", "", &bundle)
	if _, ok := bundle.Links["media"]; ok {
		t.Fatal("bundle without MEDIA.json has a media link")
	}
	if response := request(t, handler, http.MethodGet, base+"/media", nil); response.Code != http.StatusNotFound {
		t.Fatalf("media without MEDIA.json = %d, want 404", response.Code)
	}

	bundlePath := filepath.Join(app.config.Pools["default"].Root, checksum)
	if err := os.WriteFile(filepath.Join(bundlePath, ".bundle", mediaFileName), []byte(testMedia), 0o644); err != nil {
		t.Fatal(err)
	}

	call(t, handler, http.MethodGet, base, "", "", &bundle)
	if bundle.Links["media"] != base+"/media" {
		t.Fatalf("media link = %q, want %q", bundle.Links["media"], base+"/media")
	}

	var index mediaIndex
	if response := call(t, handler, http.MethodGet, base+"/media", "", "", &index); response.Code != http.StatusOK {
		t.Fatalf("media status = %d", response.Code)
	}
	if index.FileCount != 1 || len(index.Files) != 1 || index.Files[0].Path != "hello.txt" {
		t.Fatalf("media files = %+v", index.Files)
	}
	if index.Files[0].EXIF != nil {
		t.Fatal("media index includes EXIF data")
	}
	if len(index.Devices) != 1 || index.Devices[0].Model != "EOS R6" || index.Devices[0].Files != 1 {
		t.Fatalf("media devices = %+v", index.Devices)
	}

	var file mediaFile
	if response := call(t, handler, http.MethodGet, base+"/media/hello.txt", "", "", &file); response.Code != http.StatusOK {
		t.Fatalf("media file status = %d", response.Code)
	}
	if file.EXIF["Make"] != "Canon" || file.Resolution == nil || *file.Resolution.Width != 4000 || file.GPS == nil {
		t.Fatalf("media file = %+v", file)
	}

	if response := request(t, handler, http.MethodGet, base+"/media/other.jpg", nil); response.Code != http.StatusNotFound {
		t.Fatalf("media of unknown file = %d, want 404", response.Code)
	}
	if response := request(t, handler, http.MethodGet, base+"/media/.bundle/META.json", nil); response.Code != http.StatusBadRequest {
		t.Fatalf("media of .bundle file = %d, want 400", response.Code)
	}
	if response := request(t, handler, http.MethodPost, base+"/media", nil); response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST media = %d, want 405", response.Code)
	}

	if err := os.WriteFile(filepath.Join(bundlePath, ".bundle", mediaFileName), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if response := request(t, handler, http.MethodGet, base+"/media", nil); response.Code != http.StatusInternalServerError {
		t.Fatalf("corrupt MEDIA.json = %d, want 500", response.Code)
	}
}
