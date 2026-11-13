package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// SyncState maintains a persistent mapping of baseName -> assetId for dedup across container restarts.
// Assets uploaded by other clients (mobile, web) have different deviceId and originalFileName,
// so Immich's device-based search can't find them. This local cache bridges that gap.
type SyncState struct {
	mu       sync.RWMutex
	assets   map[string]string
	filePath string
	dirty    bool
}

// New creates a SyncState backed by the given file path
func New(filePath string) *SyncState {
	return &SyncState{
		assets:   make(map[string]string),
		filePath: filePath,
	}
}

// Load reads the state file from disk (no-op if file doesn't exist)
func (s *SyncState) Load() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := os.ReadFile(s.filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("failed to read state file: %w", err)
	}

	if err := json.Unmarshal(data, &s.assets); err != nil {
		return fmt.Errorf("failed to parse state file: %w", err)
	}
	return nil
}

// Save writes the state file to disk (only if dirty)
func (s *SyncState) Save() error {
	s.mu.Lock()
	if !s.dirty {
		s.mu.Unlock()
		return nil
	}
	// Copy data under lock
	data := make(map[string]string, len(s.assets))
	for k, v := range s.assets {
		data[k] = v
	}
	s.dirty = false
	s.mu.Unlock()

	jsonData, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal state: %w", err)
	}

	dir := filepath.Dir(s.filePath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create state directory: %w", err)
	}

	// Atomic write via temp file
	tmpPath := s.filePath + ".tmp"
	if err := os.WriteFile(tmpPath, jsonData, 0644); err != nil {
		return fmt.Errorf("failed to write state file: %w", err)
	}
	if err := os.Rename(tmpPath, s.filePath); err != nil {
		return fmt.Errorf("failed to rename state file: %w", err)
	}
	return nil
}

// Get returns the asset ID for a baseName (thread-safe)
func (s *SyncState) Get(baseName string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	id, ok := s.assets[baseName]
	return id, ok
}

// Set stores a baseName -> assetId mapping (thread-safe)
func (s *SyncState) Set(baseName, assetId string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.assets[baseName] = assetId
	s.dirty = true
}

// AssetIDs returns the set of Immich asset IDs that were imported from Google
// Photos. The uploader uses it to avoid sending those assets back.
func (s *SyncState) AssetIDs() map[string]bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ids := make(map[string]bool, len(s.assets))
	for _, id := range s.assets {
		if id != "" {
			ids[id] = true
		}
	}
	return ids
}

// Count returns the number of cached entries
func (s *SyncState) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.assets)
}
