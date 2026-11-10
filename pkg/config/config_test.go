package config

import (
	"os"
	"path/filepath"
	"testing"
)

func writeTestConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

func setEnv(t *testing.T, key, value string) {
	t.Helper()
	old, had := os.LookupEnv(key)
	os.Setenv(key, value)
	t.Cleanup(func() {
		if had {
			os.Setenv(key, old)
		} else {
			os.Unsetenv(key)
		}
	})
}

// Issue #9: ENV vars must take runtime precedence over an existing config.json,
// so a rotated IMMICH_API_KEY deployed via container ENV is actually used.
func TestEnvOverridesConfigFile(t *testing.T) {
	path := writeTestConfig(t, `{
		"apiKey": "file-key",
		"apiURL": "http://file-immich/api",
		"googlePhotos": [{"url": "https://photos.app.goo.gl/x"}]
	}`)
	setEnv(t, "IMMICH_API_KEY", "env-key")
	setEnv(t, "IMMICH_API_URL", "http://env-immich/api")

	cfg, err := ReadConfig(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.ApiKey != "env-key" {
		t.Fatalf("expected env API key to win, got %q", cfg.ApiKey)
	}
	if cfg.ApiURL != "http://env-immich/api" {
		t.Fatalf("expected env API URL to win, got %q", cfg.ApiURL)
	}
}

// With no ENV set, the file values must be used unchanged.
func TestFileUsedWithoutEnv(t *testing.T) {
	path := writeTestConfig(t, `{
		"apiKey": "file-key",
		"apiURL": "http://file-immich/api",
		"googlePhotos": [{"url": "https://photos.app.goo.gl/x"}]
	}`)

	cfg, err := ReadConfig(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.ApiKey != "file-key" || cfg.ApiURL != "http://file-immich/api" {
		t.Fatalf("file values must survive when no ENV is set, got %q / %q", cfg.ApiKey, cfg.ApiURL)
	}
}

// ENV still fills gaps when the file omits credentials (fallback path).
func TestEnvFillsMissingFileValues(t *testing.T) {
	path := writeTestConfig(t, `{
		"googlePhotos": [{"url": "https://photos.app.goo.gl/x"}]
	}`)
	setEnv(t, "IMMICH_API_KEY", "env-key")
	setEnv(t, "IMMICH_API_URL", "http://env-immich/api")

	cfg, err := ReadConfig(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.ApiKey != "env-key" || cfg.ApiURL != "http://env-immich/api" {
		t.Fatalf("expected env fallback, got %q / %q", cfg.ApiKey, cfg.ApiURL)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected valid config, got: %v", err)
	}
}

// No file and no ENV is a hard error that keeps the app waiting for config.
func TestMissingFileAndEnvFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.json")
	if _, err := ReadConfig(path); err == nil {
		t.Fatal("expected error when config file and ENV vars are both missing")
	}
}

func TestDedupModes(t *testing.T) {
	tests := []struct {
		mode         string
		wantMode     string
		wantVerify   bool
		wantAdopt    bool
		wantReupload bool
	}{
		{"", DedupModeSafe, true, true, false},
		{"safe", DedupModeSafe, true, true, false},
		{"repair", DedupModeRepair, true, true, true},
		{"fast", DedupModeFast, false, false, false},
	}
	for _, tt := range tests {
		cfg := &Config{DedupMode: tt.mode}
		if got := cfg.EffectiveDedupMode(); got != tt.wantMode {
			t.Errorf("mode %q: EffectiveDedupMode() = %q, want %q", tt.mode, got, tt.wantMode)
		}
		if got := cfg.VerifyDedupState(); got != tt.wantVerify {
			t.Errorf("mode %q: VerifyDedupState() = %v", tt.mode, got)
		}
		if got := cfg.AdoptReplacements(); got != tt.wantAdopt {
			t.Errorf("mode %q: AdoptReplacements() = %v", tt.mode, got)
		}
		if got := cfg.ReuploadMissing(); got != tt.wantReupload {
			t.Errorf("mode %q: ReuploadMissing() = %v", tt.mode, got)
		}
	}
}

func TestValidateRejectsUnknownDedupMode(t *testing.T) {
	cfg := &Config{ApiKey: "k", ApiURL: "http://x/api", DedupMode: "nonsense"}
	if err := cfg.Validate(); err == nil {
		t.Fatal("unknown dedupMode must fail validation")
	}
}

func TestValidate(t *testing.T) {
	cfg := &Config{ApiKey: "k", ApiURL: "http://x/api"}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}

	cfg = &Config{ApiURL: "http://x/api"}
	if err := cfg.Validate(); err == nil || cfg.Validate().Error() == "" {
		t.Fatal("missing apiKey must fail validation")
	}

	cfg = &Config{ApiKey: "k"}
	if err := cfg.Validate(); err == nil {
		t.Fatal("missing apiURL must fail validation")
	}

	cfg = &Config{ApiKey: "k", ApiURL: "http://x/api", Workers: -1}
	if err := cfg.Validate(); err == nil {
		t.Fatal("negative workers must fail validation")
	}

	cfg = &Config{ApiKey: "k", ApiURL: "http://x/api", AlbumWorkers: -2}
	if err := cfg.Validate(); err == nil {
		t.Fatal("negative albumWorkers must fail validation")
	}

	cfg = &Config{
		ApiKey: "k", ApiURL: "http://x/api",
		GooglePhotos: []GooglePhotosConfig{{URL: "x", SyncInterval: "nope"}},
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("invalid syncInterval must fail validation")
	}
}
