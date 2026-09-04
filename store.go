package prompton

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

// The snapshot store is three tiers and nothing else: memory, one local file,
// and a file shipped inside the app. No database, no Redis, no shared cache —
// instances never coordinate, and ETag polling makes that cheap. Several
// processes on one host may share the disk file: writes are atomic, readers
// tolerate a concurrent rename, and a corrupt or partial file is ignored rather
// than raised.

type snapshotEntry struct {
	snapshot     *Snapshot
	etag         string
	lastModified string
	source       Source
	fetchedAt    time.Time
	staleSince   time.Time
}

type snapshotStore struct {
	mu    sync.RWMutex
	entry *snapshotEntry
}

func (s *snapshotStore) get() *snapshotEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.entry
}

func (s *snapshotStore) put(e *snapshotEntry) {
	s.mu.Lock()
	s.entry = e
	s.mu.Unlock()
}

// markStale records that a refresh failed while keeping the document in place.
// A generation must never fail because PromptOn did: config is stale in the
// worst case, not absent.
func (s *snapshotStore) markStale(at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.entry != nil && s.entry.staleSince.IsZero() {
		clone := *s.entry
		clone.staleSince = at
		s.entry = &clone
	}
}

// markFresh promotes an entry after the server confirmed its ETag is current.
func (s *snapshotStore) markFresh(source Source, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.entry == nil {
		return
	}
	clone := *s.entry
	clone.source = source
	clone.staleSince = time.Time{}
	clone.fetchedAt = at
	s.entry = &clone
}

// SnapshotInfo describes the document resolution is currently reading.
type SnapshotInfo struct {
	// Source is remote, disk or bundle; empty when no document is loaded.
	Source Source
	// Project and Environment are the document's own, not the configuration's.
	Project     string
	Environment string
	ETag        string
	// LastModified is the header the server sent, unparsed.
	LastModified string
	FetchedAt    time.Time
	// Stale is true when the last refresh failed, or when the document did not
	// come from the server.
	Stale bool
	// Age is how long ago the document was fetched.
	Age time.Duration
	// Loaded is false when no tier held a document.
	Loaded bool
}

// ---------------------------------------------------------------------------
// disk cache

type sidecar struct {
	ETag         string `json:"etag"`
	LastModified string `json:"last_modified"`
	Environment  string `json:"environment"`
	Project      string `json:"project"`
	FetchedAt    string `json:"fetched_at"`
}

func sidecarPath(path string) string { return path + ".meta.json" }

// readSnapshotFile loads a snapshot document and its sidecar, refusing a
// document from another environment or project. A staging process must not boot
// on a production document, and the file records both so the mismatch is
// visible.
func readSnapshotFile(path, environment, project string) (*snapshotEntry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	snap, err := ParseSnapshot(data)
	if err != nil {
		return nil, err
	}
	if err := guardDocument(snap, environment, project); err != nil {
		return nil, err
	}
	entry := &snapshotEntry{snapshot: snap}
	if meta, err := os.ReadFile(sidecarPath(path)); err == nil {
		var sc sidecar
		if err := decodeJSON(meta, &sc); err == nil {
			entry.etag = sc.ETag
			entry.lastModified = sc.LastModified
			if t, err := time.Parse(time.RFC3339Nano, sc.FetchedAt); err == nil {
				entry.fetchedAt = t
			}
		}
	}
	return entry, nil
}

// guardDocument refuses a document that belongs to another environment or
// project. A document that names neither is refused too: a hand-assembled or
// legacy bundle with no environment key is exactly the artefact a migration
// produces, and accepting it in every process is the one thing that must never
// happen.
func guardDocument(snap *Snapshot, environment, project string) error {
	if environment != "" && snap.Environment != environment {
		return fmt.Errorf("prompton: snapshot is for environment %s, this process reads %q", documentLabel(snap.Environment), environment)
	}
	if project != "" && snap.Project != project {
		return fmt.Errorf("prompton: snapshot is for project %s, this process reads %q", documentLabel(snap.Project), project)
	}
	return nil
}

func documentLabel(value string) string {
	if value == "" {
		return "nothing in particular (the document names none)"
	}
	return strconv.Quote(value)
}

// writeSnapshotFile writes the document and its sidecar atomically, so a reader
// in another process sees either the old file or the new one.
func writeSnapshotFile(path string, body []byte, meta sidecar) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := atomicWrite(path, body); err != nil {
		return err
	}
	return atomicWrite(sidecarPath(path), canonicalJSON(meta))
}

func atomicWrite(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() {
		if name != "" {
			_ = os.Remove(name)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(name, 0o644); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	name = ""
	return nil
}
