package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
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

func TestGoogleUploadDefaults(t *testing.T) {
	path := writeTestConfig(t, `{
		"apiKey": "file-key",
		"apiURL": "http://file-immich/api",
		"googlePhotos": [{"url": "https://photos.app.goo.gl/x"}]
	}`)

	cfg, err := ReadConfig(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.GoogleUpload.StorageMode != StorageModeQuota {
		t.Fatalf("storage mode = %q, want %q", cfg.GoogleUpload.StorageMode, StorageModeQuota)
	}
	if cfg.GoogleUpload.Quality != QualityOriginal {
		t.Fatalf("quality = %q, want %q", cfg.GoogleUpload.Quality, QualityOriginal)
	}
	if cfg.GoogleUpload.Threads != 1 {
		t.Fatalf("threads = %d, want 1", cfg.GoogleUpload.Threads)
	}
	if cfg.GoogleUpload.MaxRetries != 3 {
		t.Fatalf("maxRetries = %d, want 3", cfg.GoogleUpload.MaxRetries)
	}
	if cfg.GoogleUpload.GotohpBinary != "gotohp-cli" {
		t.Fatalf("gotohpBinary = %q", cfg.GoogleUpload.GotohpBinary)
	}
	if cfg.GoogleUpload.GotohpConfig != "./data/gotohp.config" {
		t.Fatalf("gotohpConfig = %q", cfg.GoogleUpload.GotohpConfig)
	}
	if cfg.GoogleUpload.WebCookiesFile != "./data/gphotos-web-cookies.txt" {
		t.Fatalf("webCookiesFile = %q", cfg.GoogleUpload.WebCookiesFile)
	}
	if cfg.GoogleUpload.ReuploadDeleted {
		t.Fatal("reuploadDeleted must default to false")
	}
	if cfg.Notifications.Immich {
		t.Fatal("Immich notifications must default to false")
	}
	if cfg.EffectiveStorageMode() != StorageModeQuota {
		t.Fatalf("effective storage mode = %q", cfg.EffectiveStorageMode())
	}
}

func TestGoogleUploadValidation(t *testing.T) {
	base := func() *Config {
		return &Config{ApiKey: "k", ApiURL: "http://x/api"}
	}

	cfg := base()
	cfg.GoogleUpload.StorageMode = "nonsense"
	if err := cfg.Validate(); err == nil {
		t.Fatal("invalid storageMode must fail validation")
	}

	cfg = base()
	cfg.GoogleUpload.Quality = "nonsense"
	if err := cfg.Validate(); err == nil {
		t.Fatal("invalid quality must fail validation")
	}

	cfg = base()
	cfg.GoogleUpload.Threads = -1
	if err := cfg.Validate(); err == nil {
		t.Fatal("negative threads must fail validation")
	}

	cfg = base()
	cfg.GooglePhotos = []GooglePhotosConfig{{URL: "x", UploadToGoogle: true}}
	if err := cfg.Validate(); err == nil {
		t.Fatal("uploadToGoogle without immichAlbumId must fail validation")
	}

	cfg = base()
	cfg.GooglePhotos = []GooglePhotosConfig{{
		URL: "x", ImmichAlbumID: "album-1", UploadToGoogle: true,
		Upload: &GoogleUploadOverrides{StorageMode: "nonsense"},
	}}
	if err := cfg.Validate(); err == nil {
		t.Fatal("invalid per-album storageMode must fail validation")
	}
}

func TestAlbumUploadOverrides(t *testing.T) {
	cfg := &Config{
		ApiKey: "k", ApiURL: "http://x/api",
		GoogleUpload: GoogleUploadConfig{StorageMode: StorageModeUnlimited, Quality: QualityOriginal},
	}
	quota := GooglePhotosConfig{
		URL: "x", ImmichAlbumID: "a",
		Upload: &GoogleUploadOverrides{StorageMode: StorageModeQuota, Quality: QualitySaver},
	}
	if got := cfg.AlbumStorageMode(quota); got != StorageModeQuota {
		t.Fatalf("album storage mode = %q, want quota", got)
	}
	if got := cfg.AlbumQuality(quota); got != QualitySaver {
		t.Fatalf("album quality = %q, want saver", got)
	}

	skip := true
	live := GooglePhotosConfig{
		URL: "x", ImmichAlbumID: "a",
		Upload: &GoogleUploadOverrides{SkipLivePhotos: &skip},
	}
	if !cfg.AlbumSkipLivePhotos(live) {
		t.Fatal("per-album skipLivePhotos override was not applied")
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

func TestEffectiveUIMode(t *testing.T) {
	tests := []struct {
		name     string
		mode     string
		accounts []AccountMapping
		upload   bool
		want     string
	}{
		{name: "empty defaults to standard", want: UIModeStandard},
		{name: "empty with upload enabled", upload: true, want: UIModeStandard},
		{name: "accounts imply advanced", accounts: []AccountMapping{{ImmichUserID: "u"}}, want: UIModeAdvanced},
		{name: "explicit simple wins over accounts", mode: " Simple ", accounts: []AccountMapping{{ImmichUserID: "u"}}, want: UIModeSimple},
		{name: "explicit standard", mode: UIModeStandard, accounts: []AccountMapping{{ImmichUserID: "u"}}, want: UIModeStandard},
		{name: "explicit advanced", mode: UIModeAdvanced, want: UIModeAdvanced},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{UIMode: tt.mode, Accounts: tt.accounts}
			cfg.GoogleUpload.Enabled = tt.upload
			if got := cfg.EffectiveUIMode(); got != tt.want {
				t.Fatalf("EffectiveUIMode() = %q, want %q", got, tt.want)
			}
		})
	}
	var nilCfg *Config
	if got := nilCfg.EffectiveUIMode(); got != UIModeStandard {
		t.Fatalf("nil config mode = %q", got)
	}
}

func TestValidateRejectsUnknownUIMode(t *testing.T) {
	cfg := &Config{ApiKey: "k", ApiURL: "http://x/api", UIMode: "fancy"}
	if err := cfg.Validate(); err == nil {
		t.Fatal("unknown uiMode must fail validation")
	}
}

func TestReadConfigNormalizesAccounts(t *testing.T) {
	path := writeTestConfig(t, `{
		"apiKey": "k",
		"apiURL": "http://immich/api",
		"uiMode": "  Advanced ",
		"accounts": [
			{
				"name": "  Warre  ",
				"immichUserId": " owner-warre ",
				"immichApiKey": " key-warre ",
				"googleEmail": " warre@gmail.com ",
				"isDefault": true
			},
			{
				"id": " acc-lotte ",
				"name": "Lotte",
				"immichUserId": "owner-lotte",
				"googleEmail": "lotte@gmail.com",
				"webCookiesFile": " /data/lotte-cookies.txt "
			}
		]
	}`)

	cfg, err := ReadConfig(path)
	if err != nil {
		t.Fatalf("ReadConfig failed: %v", err)
	}
	if cfg.UIMode != UIModeAdvanced {
		t.Fatalf("uiMode = %q", cfg.UIMode)
	}
	if len(cfg.Accounts) != 2 {
		t.Fatalf("accounts = %#v", cfg.Accounts)
	}
	warre := cfg.Accounts[0]
	if warre.ID != "owner-warre" || warre.Name != "Warre" || warre.ImmichUserID != "owner-warre" {
		t.Fatalf("first account was not normalized: %#v", warre)
	}
	if warre.ImmichAPIKey != "key-warre" || warre.GoogleEmail != "warre@gmail.com" || !warre.IsDefault {
		t.Fatalf("first account fields = %#v", warre)
	}
	if warre.WebCookiesFile != "./data/gphotos-web-cookies-owner-warre.txt" {
		t.Fatalf("auto cookies path = %q", warre.WebCookiesFile)
	}
	lotte := cfg.Accounts[1]
	if lotte.ID != "acc-lotte" || lotte.WebCookiesFile != "/data/lotte-cookies.txt" {
		t.Fatalf("second account = %#v", lotte)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("normalized config must validate: %v", err)
	}
}

func TestEffectiveAccountCookiesFile(t *testing.T) {
	tests := []struct {
		name string
		acc  AccountMapping
		want string
	}{
		{
			name: "explicit path wins",
			acc:  AccountMapping{ID: "acc-1", WebCookiesFile: " /tmp/custom.txt "},
			want: "/tmp/custom.txt",
		},
		{
			name: "id is sanitized",
			acc:  AccountMapping{ID: "acc 1/lotte", GoogleEmail: "lotte@gmail.com"},
			want: "./data/gphotos-web-cookies-acc-1-lotte.txt",
		},
		{
			name: "email when the id is missing",
			acc:  AccountMapping{GoogleEmail: "Warre@GMail.com"},
			want: "./data/gphotos-web-cookies-warre-gmail.com.txt",
		},
		{
			name: "falls back to the immich user id",
			acc:  AccountMapping{ImmichUserID: "8c4b2d0e"},
			want: "./data/gphotos-web-cookies-8c4b2d0e.txt",
		},
		{
			name: "empty mapping still yields a usable path",
			acc:  AccountMapping{},
			want: "./data/gphotos-web-cookies-account.txt",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := EffectiveAccountCookiesFile(tt.acc); got != tt.want {
				t.Fatalf("EffectiveAccountCookiesFile() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestValidateAccounts(t *testing.T) {
	base := func(accounts ...AccountMapping) *Config {
		return &Config{ApiKey: "k", ApiURL: "http://x/api", Accounts: accounts}
	}

	if err := base(AccountMapping{ImmichUserID: "u1"}, AccountMapping{ImmichUserID: "u2"}).Validate(); err != nil {
		t.Fatalf("valid accounts rejected: %v", err)
	}
	if err := base(AccountMapping{}).Validate(); err == nil {
		t.Fatal("an account without immichUserId must fail validation")
	}
	if err := base(
		AccountMapping{ID: "same", ImmichUserID: "u1"},
		AccountMapping{ID: "same", ImmichUserID: "u2"},
	).Validate(); err == nil {
		t.Fatal("duplicate account ids must fail validation")
	}
	if err := base(
		AccountMapping{ImmichUserID: "u1"},
		AccountMapping{ImmichUserID: "u1"},
	).Validate(); err == nil {
		t.Fatal("duplicate immichUserId values must fail validation")
	}
	if err := base(
		AccountMapping{ImmichUserID: "u1", IsDefault: true},
		AccountMapping{ImmichUserID: "u2", IsDefault: true},
	).Validate(); err == nil {
		t.Fatal("more than one default account must fail validation")
	}
}

func TestStrictAccountMatchingAndImmichAccountIDRoundTrip(t *testing.T) {
	path := writeTestConfig(t, `{
		"apiKey": "k",
		"apiURL": "http://immich/api",
		"googleUpload": {"strictAccountMatching": true},
		"googlePhotos": [{
			"url": "https://photos.app.goo.gl/x",
			"immichAlbumId": "album-1",
			"immichAccountId": " acc-lotte "
		}]
	}`)
	cfg, err := ReadConfig(path)
	if err != nil {
		t.Fatalf("ReadConfig failed: %v", err)
	}
	if !cfg.GoogleUpload.StrictAccountMatching {
		t.Fatal("strictAccountMatching was not read")
	}
	if cfg.GooglePhotos[0].ImmichAccountID != "acc-lotte" {
		t.Fatalf("immichAccountId = %q", cfg.GooglePhotos[0].ImmichAccountID)
	}

	cfg.GooglePhotos[0].ImmichAccountID = ""
	if strings.Contains(string(mustJSON(t, cfg.GooglePhotos[0])), "immichAccountId") {
		t.Fatal("an empty immichAccountId must be omitted from JSON")
	}
}
