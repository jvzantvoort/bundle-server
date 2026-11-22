package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// mediaFileName is the file in which bundle-python stores the media metadata
// (EXIF, resolution, capture time, GPS, device) of a bundle's files. It lives
// in .bundle/, outside the bundle checksum, so it never changes a bundle's
// identity. The server only reads it.
const mediaFileName = "MEDIA.json"

// maxMediaBytes bounds how much of MEDIA.json the server reads.
const maxMediaBytes = 64 << 20

type mediaResolution struct {
	Width  *int `json:"width"`
	Height *int `json:"height"`
}

type mediaGPS struct {
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
}

type mediaDevice struct {
	Make     string `json:"make,omitempty"`
	Model    string `json:"model,omitempty"`
	Lens     string `json:"lens,omitempty"`
	Software string `json:"software,omitempty"`
	Files    int    `json:"files,omitempty"`
}

type mediaFile struct {
	Path            string           `json:"path"`
	Type            string           `json:"type"`
	Format          string           `json:"format,omitempty"`
	Resolution      *mediaResolution `json:"resolution,omitempty"`
	TakenAt         string           `json:"taken_at,omitempty"`
	Device          *mediaDevice     `json:"device,omitempty"`
	GPS             *mediaGPS        `json:"gps,omitempty"`
	DurationSeconds *float64         `json:"duration_seconds,omitempty"`
	Codec           string           `json:"codec,omitempty"`
	SHA256          string           `json:"sha256,omitempty"`
	EXIF            map[string]any   `json:"exif,omitempty"`
}

type mediaDocument struct {
	Version   int                  `json:"version"`
	Generator string               `json:"generator"`
	UpdatedAt string               `json:"updated_at"`
	Devices   []mediaDevice        `json:"devices"`
	Files     map[string]mediaFile `json:"files"`
}

type mediaIndex struct {
	Version   int           `json:"version"`
	Generator string        `json:"generator"`
	UpdatedAt string        `json:"updated_at"`
	Devices   []mediaDevice `json:"devices"`
	FileCount int           `json:"file_count"`
	Files     []mediaFile   `json:"files"`
}

func mediaPath(bundlePath string) string {
	return filepath.Join(bundlePath, ".bundle", mediaFileName)
}

func hasMedia(bundlePath string) bool {
	info, err := os.Stat(mediaPath(bundlePath))
	return err == nil && info.Mode().IsRegular()
}

// loadMedia reads MEDIA.json of a bundle. It returns fs.ErrNotExist when the
// bundle has none.
func loadMedia(bundlePath string) (mediaDocument, error) {
	var doc mediaDocument
	file, err := os.Open(mediaPath(bundlePath))
	if err != nil {
		return doc, err
	}
	defer file.Close()
	content, err := io.ReadAll(io.LimitReader(file, maxMediaBytes+1))
	if err != nil {
		return doc, err
	}
	if len(content) > maxMediaBytes {
		return doc, fmt.Errorf("%s is larger than %d bytes", mediaFileName, maxMediaBytes)
	}
	if err := json.Unmarshal(content, &doc); err != nil {
		return doc, fmt.Errorf("parse %s: %w", mediaFileName, err)
	}
	files := make(map[string]mediaFile, len(doc.Files))
	for key, entry := range doc.Files {
		key = strings.TrimPrefix(filepath.ToSlash(key), "./")
		entry.Path = key
		files[key] = entry
	}
	doc.Files = files
	if doc.Devices == nil {
		doc.Devices = []mediaDevice{}
	}
	return doc, nil
}

func (a *App) mediaRoutes(w http.ResponseWriter, r *http.Request, poolName, checksum string, rest []string) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method is not allowed")
		return
	}
	record, err := a.bundle(poolName, checksum)
	if err != nil {
		a.bundleError(w, err)
		return
	}
	doc, err := loadMedia(record.Path)
	if errors.Is(err, fs.ErrNotExist) {
		writeError(w, http.StatusNotFound, "media_not_found", "bundle has no media metadata; run 'bundle-python scan' on it")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "media_read_failed", err.Error())
		return
	}
	if len(rest) == 0 {
		writeJSON(w, http.StatusOK, mediaSummary(doc))
		return
	}
	filePath, err := safeFilePath(strings.Join(rest, "/"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_file_path", err.Error())
		return
	}
	entry, ok := doc.Files[filePath]
	if !ok {
		writeError(w, http.StatusNotFound, "media_not_found", "no media metadata for this file")
		return
	}
	writeJSON(w, http.StatusOK, entry)
}

// mediaSummary lists every file without its EXIF data, which can be large;
// the per-file request returns it.
func mediaSummary(doc mediaDocument) mediaIndex {
	files := make([]mediaFile, 0, len(doc.Files))
	for _, entry := range doc.Files {
		entry.EXIF = nil
		files = append(files, entry)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return mediaIndex{
		Version:   doc.Version,
		Generator: doc.Generator,
		UpdatedAt: doc.UpdatedAt,
		Devices:   doc.Devices,
		FileCount: len(files),
		Files:     files,
	}
}
