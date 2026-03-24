package config

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

type GooglePhotosConfig struct {
	URL           string `json:"url"`
	ImmichAlbumID string `json:"immichAlbumId"`     // Optional, if existing
	AlbumName     string `json:"albumName"`         // Optional, to create new
	SyncInterval  string `json:"syncInterval"`      // e.g., "12h", "60m"
}

type Config struct {
	ApiKey         string               `json:"apiKey"`
	ApiURL         string               `json:"apiURL"`
	Debug          bool                 `json:"debug"`          // Optional, enable verbose logging
	Workers        int                  `json:"workers"`        // Optional, default 1
	AlbumWorkers   int                  `json:"albumWorkers"`   // Optional, concurrent album processing (default 1)
	StrictMetadata bool                 `json:"strictMetadata"` // Optional, skip items with missing dates
	SkipVideos     bool                 `json:"skipVideos"`     // Optional, skip video items entirely
	DataDir        string               `json:"dataDir"`        // Optional, directory for persistent state (default "./data")
	GooglePhotos   []GooglePhotosConfig `json:"googlePhotos"`
}

func ReadConfig(path string) (*Config, error) {
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			// Try ENV vars for basic config
			apiKey := os.Getenv("IMMICH_API_KEY")
			apiURL := os.Getenv("IMMICH_API_URL")
			
			if apiKey == "" || apiURL == "" {
				return nil, fmt.Errorf("config file not found and ENV vars missing")
			}
			
			return &Config{
				ApiKey: apiKey,
				ApiURL: apiURL,
			}, nil
		}
		return nil, err
	}
	defer file.Close()
	
	bytefile, err := io.ReadAll(file)
	if err != nil {
		return nil, fmt.Errorf("error reading config: %w", err)
	}
	var config Config
	if err := json.Unmarshal(bytefile, &config); err != nil {
		return nil, err
	}

	// Override/Fallback with ENV
	if config.ApiKey == "" { config.ApiKey = os.Getenv("IMMICH_API_KEY") }
	if config.ApiURL == "" { config.ApiURL = os.Getenv("IMMICH_API_URL") }

	config.ApiKey = strings.TrimSpace(config.ApiKey)
	config.ApiURL = strings.TrimSpace(config.ApiURL)
	for i := range config.GooglePhotos {
		config.GooglePhotos[i].URL = strings.TrimSpace(config.GooglePhotos[i].URL)
		config.GooglePhotos[i].ImmichAlbumID = strings.TrimSpace(config.GooglePhotos[i].ImmichAlbumID)
		config.GooglePhotos[i].AlbumName = strings.TrimSpace(config.GooglePhotos[i].AlbumName)
		config.GooglePhotos[i].SyncInterval = strings.TrimSpace(config.GooglePhotos[i].SyncInterval)
	}
	
	return &config, nil
}

// Validate checks that all required config fields are present and valid
func (c *Config) Validate() error {
	if c.ApiKey == "" {
		return fmt.Errorf("apiKey is required (set in config or IMMICH_API_KEY env var)")
	}
	if c.ApiURL == "" {
		return fmt.Errorf("apiURL is required (set in config or IMMICH_API_URL env var)")
	}
	if c.Workers < 0 {
		return fmt.Errorf("workers must be non-negative")
	}
	if c.AlbumWorkers < 0 {
		return fmt.Errorf("albumWorkers must be non-negative")
	}
	for i, gp := range c.GooglePhotos {
		if gp.URL == "" {
			return fmt.Errorf("googlePhotos[%d].url is required", i)
		}
		if gp.SyncInterval != "" {
			if _, err := time.ParseDuration(gp.SyncInterval); err != nil {
				return fmt.Errorf("googlePhotos[%d].syncInterval %q is invalid: %w", i, gp.SyncInterval, err)
			}
		}
	}
	return nil
}
