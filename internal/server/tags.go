package server

import (
	"errors"
	"io/fs"
	"net/http"
	"sort"

	"github.com/jvzantvoort/bundle/tag"
)

// apiVersion is the version of openapi.yaml this server implements.
const apiVersion = "1.2.0"

type tagsResponse struct {
	Tags  []string `json:"tags"`
	Total int      `json:"total"`
}

type poolTag struct {
	Tag         string `json:"tag"`
	BundleCount int    `json:"bundle_count"`
}

type tagChangeFailure struct {
	Checksum string `json:"checksum"`
	Code     string `json:"code"`
	Message  string `json:"message"`
}

type tagChangeResult struct {
	Tag     string             `json:"tag"`
	NewTag  string             `json:"new_tag,omitempty"`
	Changed int                `json:"changed"`
	Failed  []tagChangeFailure `json:"failed"`
}

// normalizeTag returns the stored form of a tag, or false if the bundle
// library would not accept it.
func normalizeTag(value string) (string, bool) {
	tags := &tag.Tags{}
	tags.Add(value)
	if len(tags.Tags) != 1 {
		return "", false
	}
	return tags.Tags[0], true
}

func writeTags(w http.ResponseWriter, record bundleRecord) {
	tags := record.Tags.List()
	if revision, err := revision(record.Path); err == nil {
		w.Header().Set("ETag", revision)
	}
	writeJSON(w, http.StatusOK, tagsResponse{Tags: tags, Total: len(tags)})
}

// bundleTags lists, adds or removes the tags of one bundle.
func (a *App) bundleTags(w http.ResponseWriter, r *http.Request, poolName, checksum string) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost && r.Method != http.MethodDelete {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method is not allowed")
		return
	}
	if r.Method != http.MethodGet {
		a.writeMu.Lock()
		defer a.writeMu.Unlock()
	}
	record, err := a.bundle(poolName, checksum)
	if err != nil {
		a.bundleError(w, err)
		return
	}
	if r.Method == http.MethodGet {
		writeTags(w, record)
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
		for _, value := range request.Tags {
			if _, ok := normalizeTag(value); !ok {
				writeError(w, http.StatusBadRequest, "invalid_tag", "tags must be 1-64 letters, digits, '.', '_' or '-': "+value)
				return
			}
		}
		record.Tags.Add(request.Tags...)
	} else {
		record.Tags.Remove(request.Tags...)
	}
	if err := record.Tags.Save(record.Path); err != nil {
		writeError(w, http.StatusInternalServerError, "tag_update_failed", err.Error())
		return
	}
	a.invalidateMetadata(poolName)
	writeTags(w, record)
}

// renameBundleTag replaces one tag of a bundle with another in a single write.
func (a *App) renameBundleTag(w http.ResponseWriter, r *http.Request, poolName, checksum, oldValue string) {
	oldTag, newTag, ok := a.renameRequest(w, r, oldValue)
	if !ok {
		return
	}
	a.writeMu.Lock()
	defer a.writeMu.Unlock()
	record, err := a.bundle(poolName, checksum)
	if err != nil {
		a.bundleError(w, err)
		return
	}
	if !matchesRevision(r, record.Revision) {
		writeError(w, http.StatusPreconditionFailed, "stale_revision", "bundle changed; fetch its current ETag and retry")
		return
	}
	if !contains(record.Tags.List(), oldTag) {
		writeError(w, http.StatusNotFound, "tag_not_found", "bundle does not have tag "+oldTag)
		return
	}
	if err := replaceTag(record, oldTag, newTag); err != nil {
		writeError(w, http.StatusInternalServerError, "tag_update_failed", err.Error())
		return
	}
	a.invalidateMetadata(poolName)
	writeTags(w, record)
}

// renameRequest reads the tag in the path and the new tag in the body.
func (a *App) renameRequest(w http.ResponseWriter, r *http.Request, oldValue string) (string, string, bool) {
	oldTag, ok := normalizeTag(oldValue)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_tag", "invalid tag: "+oldValue)
		return "", "", false
	}
	var request struct {
		Tag string `json:"tag"`
	}
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "request must be {\"tag\": \"<new tag>\"}")
		return "", "", false
	}
	newTag, ok := normalizeTag(request.Tag)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_tag", "tags must be 1-64 letters, digits, '.', '_' or '-': "+request.Tag)
		return "", "", false
	}
	return oldTag, newTag, true
}

// replaceTag removes oldTag and adds newTag, or only removes oldTag when
// newTag is empty, and saves the result.
func replaceTag(record bundleRecord, oldTag, newTag string) error {
	record.Tags.Remove(oldTag)
	if newTag != "" {
		record.Tags.Add(newTag)
	}
	return record.Tags.Save(record.Path)
}

// listPoolTags returns every tag in a pool with the number of bundles that
// carry it.
func (a *App) listPoolTags(w http.ResponseWriter, _ *http.Request, poolName string) {
	records, err := a.bundles(poolName)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "pool_read_failed", err.Error())
		return
	}
	counts := make(map[string]int)
	for _, record := range records {
		for _, value := range record.Tags.List() {
			counts[value]++
		}
	}
	items := make([]poolTag, 0, len(counts))
	for value, count := range counts {
		items = append(items, poolTag{Tag: value, BundleCount: count})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Tag < items[j].Tag })
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "total": len(items)})
}

// changePoolTag renames (PUT) or removes (DELETE) a tag on every bundle in a
// pool that carries it. Each bundle is changed in one write; a bundle that
// fails is reported and the others are still changed, so a retry finishes the
// job.
func (a *App) changePoolTag(w http.ResponseWriter, r *http.Request, poolName, oldValue string) {
	var oldTag, newTag string
	switch r.Method {
	case http.MethodPut:
		var ok bool
		if oldTag, newTag, ok = a.renameRequest(w, r, oldValue); !ok {
			return
		}
	case http.MethodDelete:
		var ok bool
		if oldTag, ok = normalizeTag(oldValue); !ok {
			writeError(w, http.StatusBadRequest, "invalid_tag", "invalid tag: "+oldValue)
			return
		}
	default:
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method is not allowed")
		return
	}

	records, err := a.bundles(poolName)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "pool_read_failed", err.Error())
		return
	}
	result := tagChangeResult{Tag: oldTag, NewTag: newTag, Failed: []tagChangeFailure{}}
	a.writeMu.Lock()
	defer a.writeMu.Unlock()
	for _, candidate := range records {
		// The list may come from the cache, so check each bundle on disk
		if !contains(candidate.Tags.List(), oldTag) {
			continue
		}
		checksum := candidate.Metadata.BundleChecksum
		record, err := a.bundle(poolName, checksum)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			result.Failed = append(result.Failed, tagChangeFailure{Checksum: checksum, Code: "bundle_read_failed", Message: err.Error()})
			continue
		}
		if !contains(record.Tags.List(), oldTag) {
			continue
		}
		if err := replaceTag(record, oldTag, newTag); err != nil {
			result.Failed = append(result.Failed, tagChangeFailure{Checksum: checksum, Code: "tag_update_failed", Message: err.Error()})
			continue
		}
		result.Changed++
	}
	a.invalidateMetadata(poolName)
	writeJSON(w, http.StatusOK, result)
}
