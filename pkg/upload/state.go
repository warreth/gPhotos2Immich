package upload

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// State is the persistent record of what was uploaded to Google Photos.
// It is deliberately separate from the import dedup state so a repair in one
// direction cannot corrupt the other.
type State struct {
	mu   sync.Mutex
	path string
	data stateData
}

type stateData struct {
	Version int                    `json:"version"`
	Albums  map[string]*AlbumState `json:"albums"`
}

// AlbumState is the upload state for one Immich album.
type AlbumState struct {
	GoogleAlbumKey string                 `json:"googleAlbumKey,omitempty"`
	GoogleAlbumURL string                 `json:"googleAlbumUrl,omitempty"`
	LastSync       time.Time              `json:"lastSync,omitempty"`
	Assets         map[string]*AssetState `json:"assets"`
}

// AssetState is the upload state for one Immich asset.
type AssetState struct {
	GoogleMediaKey string    `json:"googleMediaKey,omitempty"`
	Filename       string    `json:"filename,omitempty"`
	Checksum       string    `json:"checksum,omitempty"`
	IsLivePhoto    bool      `json:"isLivePhoto,omitempty"`
	Status         string    `json:"status"` // uploaded, skipped, failed
	AddedToAlbum   bool      `json:"addedToAlbum,omitempty"`
	UploadedAt     time.Time `json:"uploadedAt,omitempty"`
	Error          string    `json:"error,omitempty"`
}

// PendingAlbumAdd is an item that is in the Google library but not yet in the
// target album. It lets a failed shared-album add be retried without uploading
// the file again.
type PendingAlbumAdd struct {
	AssetID  string
	MediaKey string
}

// NewState creates a state backed by path.
func NewState(path string) *State {
	return &State{
		path: path,
		data: stateData{Version: stateVersion, Albums: make(map[string]*AlbumState)},
	}
}

// stateVersion 2 adds AddedToAlbum. Version 1 uploaded straight into the
// album, so every uploaded entry from that version is already in it.
const stateVersion = 2

// Load reads the state file. A missing file is not an error.
func (s *State) Load() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	content, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read upload state: %w", err)
	}
	var data stateData
	if err := json.Unmarshal(content, &data); err != nil {
		return fmt.Errorf("parse upload state: %w", err)
	}
	if data.Albums == nil {
		data.Albums = make(map[string]*AlbumState)
	}
	if data.Version < stateVersion {
		for _, album := range data.Albums {
			for _, asset := range album.Assets {
				if asset != nil && asset.Status == "uploaded" && asset.GoogleMediaKey != "" {
					asset.AddedToAlbum = true
				}
			}
		}
		data.Version = stateVersion
	}
	s.data = data
	return nil
}

// Save writes the state atomically.
func (s *State) Save() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := os.MkdirAll(filepath.Dir(s.path), 0755); err != nil {
		return fmt.Errorf("create upload state directory: %w", err)
	}
	content, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal upload state: %w", err)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, content, 0644); err != nil {
		return fmt.Errorf("write upload state: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("replace upload state: %w", err)
	}
	return nil
}

// Album returns the state for one album, creating it if needed.
func (s *State) Album(albumID string) *AlbumState {
	s.mu.Lock()
	defer s.mu.Unlock()
	album, ok := s.data.Albums[albumID]
	if !ok {
		album = &AlbumState{Assets: make(map[string]*AssetState)}
		s.data.Albums[albumID] = album
	}
	if album.Assets == nil {
		album.Assets = make(map[string]*AssetState)
	}
	return album
}

// Set stores one asset state.
func (s *State) Set(albumID, assetID string, asset AssetState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	album, ok := s.data.Albums[albumID]
	if !ok {
		album = &AlbumState{Assets: make(map[string]*AssetState)}
		s.data.Albums[albumID] = album
	}
	if album.Assets == nil {
		album.Assets = make(map[string]*AssetState)
	}
	copy := asset
	album.Assets[assetID] = &copy
}

// Get returns one asset state.
func (s *State) Get(albumID, assetID string) (AssetState, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	album, ok := s.data.Albums[albumID]
	if !ok || album.Assets == nil {
		return AssetState{}, false
	}
	asset, ok := album.Assets[assetID]
	if !ok {
		return AssetState{}, false
	}
	return *asset, true
}

// SetAlbumMeta records the resolved Google album and the last sync time.
func (s *State) SetAlbumMeta(albumID, googleKey, googleURL string, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	album, ok := s.data.Albums[albumID]
	if !ok {
		album = &AlbumState{Assets: make(map[string]*AssetState)}
		s.data.Albums[albumID] = album
	}
	album.GoogleAlbumKey = googleKey
	album.GoogleAlbumURL = googleURL
	album.LastSync = at
}

// PendingAlbumAdds returns uploaded assets whose album add still has to be
// retried.
func (s *State) PendingAlbumAdds(albumID string) []PendingAlbumAdd {
	s.mu.Lock()
	defer s.mu.Unlock()
	album, ok := s.data.Albums[albumID]
	if !ok || album.Assets == nil {
		return nil
	}
	pending := make([]PendingAlbumAdd, 0)
	for assetID, asset := range album.Assets {
		if asset == nil || asset.AddedToAlbum || asset.GoogleMediaKey == "" {
			continue
		}
		if asset.Status != "uploaded" {
			continue
		}
		pending = append(pending, PendingAlbumAdd{AssetID: assetID, MediaKey: asset.GoogleMediaKey})
	}
	return pending
}

// MarkAdded records that an uploaded asset is now in the album.
func (s *State) MarkAdded(albumID, assetID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	album, ok := s.data.Albums[albumID]
	if !ok || album.Assets == nil {
		return
	}
	asset, ok := album.Assets[assetID]
	if !ok {
		return
	}
	asset.AddedToAlbum = true
	asset.Error = ""
}

// AlbumSummary returns counts for the UI.
func (s *State) AlbumSummary(albumID string) (uploaded, skipped, failed int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	album, ok := s.data.Albums[albumID]
	if !ok {
		return 0, 0, 0
	}
	for _, asset := range album.Assets {
		switch asset.Status {
		case "uploaded":
			uploaded++
		case "skipped":
			skipped++
		case "failed":
			failed++
		}
	}
	return uploaded, skipped, failed
}
