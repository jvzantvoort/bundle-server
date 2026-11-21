package server

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jvzantvoort/bundle/metadata"
)

// Upload sessions survive a restart. Each session keeps three entries in the
// pool root, all hidden from bundle listings because they start with a dot:
//
//	.upload-<id>/       staging directory with the files received so far
//	.upload-<id>.json   the session: manifest, metadata, Idempotency-Key, expiry
//	.upload-<id>.done   journal of accepted files, one JSON string per line
//
// A file is only journaled after it was written, synced and checked, so a file
// in the staging directory that is not in the journal is a partial write.

const uploadPrefix = ".upload-"

type storedUpload struct {
	ID          string                `json:"id"`
	Key         string                `json:"idempotency_key,omitempty"`
	RequestHash string                `json:"request_hash"`
	Checksum    string                `json:"checksum"`
	Metadata    metadata.Metadata     `json:"metadata"`
	Tags        []string              `json:"tags"`
	Files       map[string]uploadFile `json:"files"`
	CreatedAt   time.Time             `json:"created_at"`
	ExpiresAt   time.Time             `json:"expires_at"`
}

func uploadStagingPath(root, id string) string { return filepath.Join(root, uploadPrefix+id) }
func uploadSessionPath(root, id string) string { return uploadStagingPath(root, id) + ".json" }
func uploadJournalPath(root, id string) string { return uploadStagingPath(root, id) + ".done" }

// saveSession writes the session record atomically.
func saveSession(root string, upload *upload) error {
	data, err := json.Marshal(storedUpload{
		ID: upload.ID, Key: upload.Key, RequestHash: upload.RequestHash, Checksum: upload.Checksum,
		Metadata: upload.Metadata, Tags: upload.Tags, Files: upload.Files,
		CreatedAt: upload.CreatedAt, ExpiresAt: upload.ExpiresAt,
	})
	if err != nil {
		return err
	}
	target := uploadSessionPath(root, upload.ID)
	temp := target + ".tmp"
	if err := writeSynced(temp, data, os.O_CREATE|os.O_TRUNC|os.O_WRONLY); err != nil {
		_ = os.Remove(temp)
		return err
	}
	return os.Rename(temp, target)
}

// journalFile records that a file was accepted.
func journalFile(root, id, filePath string) error {
	line, err := json.Marshal(filePath)
	if err != nil {
		return err
	}
	return writeSynced(uploadJournalPath(root, id), append(line, '\n'), os.O_CREATE|os.O_APPEND|os.O_WRONLY)
}

func writeSynced(name string, data []byte, flag int) error {
	file, err := os.OpenFile(name, flag, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

// removeSession deletes the session record and journal. The staging directory
// is left alone: on completion it has become the bundle.
func removeSession(root, id string) {
	for _, name := range []string{uploadSessionPath(root, id), uploadJournalPath(root, id)} {
		if err := os.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
			log.Printf("failed to remove upload file %s: %v", name, err)
		}
	}
}

// discardUpload removes everything an upload left in the pool.
func discardUpload(root, id string) error {
	removeSession(root, id)
	return os.RemoveAll(uploadStagingPath(root, id))
}

// loadUploads restores the open sessions of a pool. Expired and damaged
// sessions, and staging directories without a session, are removed.
func loadUploads(poolName, root string, now time.Time) ([]*upload, error) {
	entries, err := os.ReadDir(root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	sessions := make(map[string]bool)
	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() && strings.HasPrefix(name, uploadPrefix) && strings.HasSuffix(name, ".json") {
			sessions[strings.TrimSuffix(strings.TrimPrefix(name, uploadPrefix), ".json")] = true
		}
	}

	var loaded []*upload
	for id := range sessions {
		upload, err := loadUpload(poolName, root, id)
		switch {
		case err != nil:
			log.Printf("discarding upload %s in pool %s: %v", id, poolName, err)
		case !now.Before(upload.ExpiresAt):
			log.Printf("discarding expired upload %s in pool %s", id, poolName)
		default:
			loaded = append(loaded, upload)
			continue
		}
		if err := discardUpload(root, id); err != nil {
			log.Printf("failed to remove upload %s in pool %s: %v", id, poolName, err)
		}
	}

	// Staging directories from before sessions were stored cannot be resumed,
	// and a journal can outlive a session cancelled during an upload
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, uploadPrefix) || strings.HasSuffix(name, ".json") {
			continue
		}
		id := strings.TrimPrefix(name, uploadPrefix)
		if !entry.IsDir() {
			id = strings.TrimSuffix(id, ".done")
		}
		if !sessions[id] {
			log.Printf("removing %s without an upload session in pool %s", name, poolName)
			if err := os.RemoveAll(filepath.Join(root, name)); err != nil {
				log.Printf("failed to remove %s: %v", name, err)
			}
		}
	}
	return loaded, nil
}

func loadUpload(poolName, root, id string) (*upload, error) {
	data, err := os.ReadFile(uploadSessionPath(root, id))
	if err != nil {
		return nil, err
	}
	var stored storedUpload
	if err := json.Unmarshal(data, &stored); err != nil {
		return nil, fmt.Errorf("invalid session: %w", err)
	}
	if stored.ID != id || len(stored.Files) == 0 {
		return nil, errors.New("invalid session")
	}
	staging := uploadStagingPath(root, id)
	if info, err := os.Stat(staging); err != nil || !info.IsDir() {
		return nil, errors.New("staging directory is missing")
	}

	accepted, err := readJournal(uploadJournalPath(root, id))
	if err != nil {
		return nil, err
	}
	for filePath, file := range stored.Files {
		file.Uploaded = accepted[filePath]
		stored.Files[filePath] = file
		if file.Uploaded {
			continue
		}
		// A partial write: remove it so the file can be sent again
		if err := os.Remove(filepath.Join(staging, filepath.FromSlash(filePath))); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
	}

	return &upload{
		ID: id, Pool: poolName, Path: staging, Checksum: stored.Checksum,
		Key: stored.Key, RequestHash: stored.RequestHash,
		Metadata: stored.Metadata, Tags: stored.Tags, Files: stored.Files,
		CreatedAt: stored.CreatedAt, ExpiresAt: stored.ExpiresAt, Status: "open",
	}, nil
}

func readJournal(name string) (map[string]bool, error) {
	accepted := make(map[string]bool)
	file, err := os.Open(name)
	if errors.Is(err, fs.ErrNotExist) {
		return accepted, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		var filePath string
		// A torn last line is a file whose acceptance was never confirmed
		if err := json.Unmarshal(scanner.Bytes(), &filePath); err != nil {
			continue
		}
		accepted[filePath] = true
	}
	return accepted, scanner.Err()
}
