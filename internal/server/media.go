package server

import (
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jvzantvoort/bundle/media"
)

// The media metadata of a bundle's files (EXIF, resolution, capture time,
// GPS, device) is stored in .bundle/MEDIA.json by `bundle update --media` or
// bundle-python. It lives outside the bundle checksum, so it never changes a
// bundle's identity. The server only reads it.

// maxMediaBytes bounds the size of MEDIA.json the server reads.
const maxMediaBytes = 64 << 20

// mediaFile is one file of the media metadata in an API response: the
// stored entry with its path.
type mediaFile struct {
	Path string `json:"path"`
	media.File
}

type mediaIndex struct {
	Version   int            `json:"version"`
	Generator string         `json:"generator"`
	UpdatedAt string         `json:"updated_at"`
	Devices   []media.Device `json:"devices"`
	FileCount int            `json:"file_count"`
	Files     []mediaFile    `json:"files"`
}

func hasMedia(bundlePath string) bool {
	info, err := os.Stat(media.Path(bundlePath))
	return err == nil && info.Mode().IsRegular()
}

// loadMedia reads MEDIA.json of a bundle, with paths normalized to the
// manifest's form. It returns fs.ErrNotExist when the bundle has none.
func loadMedia(bundlePath string) (*media.Document, error) {
	info, err := os.Stat(media.Path(bundlePath))
	if err != nil {
		return nil, err
	}
	if info.Size() > maxMediaBytes {
		return nil, fmt.Errorf("%s is larger than %d bytes", media.FileName, maxMediaBytes)
	}
	doc, err := media.Load(bundlePath)
	if err != nil {
		return nil, err
	}
	files := make(map[string]media.File, len(doc.Files))
	for key, entry := range doc.Files {
		files[strings.TrimPrefix(filepath.ToSlash(key), "./")] = entry
	}
	doc.Files = files
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
		writeError(w, http.StatusNotFound, "media_not_found", "bundle has no media metadata; run 'bundle update --media' on it")
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
	writeJSON(w, http.StatusOK, mediaFile{Path: filePath, File: entry})
}

// mediaSummary lists every file without its EXIF data, which can be large;
// the per-file request returns it.
func mediaSummary(doc *media.Document) mediaIndex {
	files := make([]mediaFile, 0, len(doc.Files))
	for path, entry := range doc.Files {
		entry.EXIF = nil
		files = append(files, mediaFile{Path: path, File: entry})
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
