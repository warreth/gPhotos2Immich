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
	URL            string                 `json:"url"`
	ImmichAlbumID  string                 `json:"immichAlbumId"` // Optional, if existing
	AlbumName      string                 `json:"albumName"`     // Optional, to create new
	SyncInterval   string                 `json:"syncInterval"`  // e.g., "12h", "60m"
	UploadToGoogle bool                   `json:"uploadToGoogle"`
	Upload         *GoogleUploadOverrides `json:"upload,omitempty"`
	// ImmichAccountID names the AccountMapping that imports this album into
	// Immich. It may be an account ID or an Immich user ID. Empty means the
	// primary apiKey is used.
	ImmichAccountID string `json:"immichAccountId,omitempty"`
}

// GoogleUploadOverrides lets one album override the global upload defaults.
type GoogleUploadOverrides struct {
	StorageMode    string `json:"storageMode,omitempty"`
	Quality        string `json:"quality,omitempty"`
	SkipLivePhotos *bool  `json:"skipLivePhotos,omitempty"`
}

// GoogleUploadConfig controls the optional Immich -> Google Photos direction.
type GoogleUploadConfig struct {
	Enabled         bool   `json:"enabled"`
	GotohpBinary    string `json:"gotohpBinary"`
	GotohpConfig    string `json:"gotohpConfig"`
	WebCookiesFile  string `json:"webCookiesFile"`
	StorageMode     string `json:"storageMode"` // "quota" (default) or "unlimited"
	Quality         string `json:"quality"`     // "original" (default) or "saver"
	SkipLivePhotos  bool   `json:"skipLivePhotos"`
	ReuploadDeleted bool   `json:"reuploadDeleted"`
	Threads         int    `json:"threads"`
	MaxRetries      int    `json:"maxRetries"`
	AutoUpdate      bool   `json:"autoUpdate"`
	// StrictAccountMatching skips Immich assets whose owner has no Google
	// account mapping. When false, those assets fall back to the default
	// account.
	StrictAccountMatching bool `json:"strictAccountMatching,omitempty"`
}

// AccountMapping links one Immich user to one Google Photos account, so each
// person's photos are uploaded with their own credentials and browser session.
type AccountMapping struct {
	ID             string `json:"id"`                     // stable ID, e.g. "acc-1" or immichUserId
	Name           string `json:"name"`                   // display name, e.g. "Lotte (Zus)"
	ImmichUserID   string `json:"immichUserId"`           // Immich user UUID (matched against asset.OwnerID)
	ImmichAPIKey   string `json:"immichApiKey,omitempty"` // optional: allows importing Google->Immich as this user
	GoogleEmail    string `json:"googleEmail,omitempty"`  // email in gotohp-cli (--account <email>)
	WebCookiesFile string `json:"webCookiesFile,omitempty"`
	IsDefault      bool   `json:"isDefault,omitempty"` // fallback account for unmapped owners
}

// UIMode values select how much of the interface is shown.
const (
	UIModeSimple   = "simple"
	UIModeStandard = "standard"
	UIModeAdvanced = "advanced"
)

// NotificationsConfig controls optional notifications sent through Immich.
// Immich notifications use the admin notification API, so the API key needs
// the notification.create permission (or all permissions).
type NotificationsConfig struct {
	Immich           bool `json:"immich"`
	OnSyncFailed     bool `json:"onSyncFailed"`
	OnImportComplete bool `json:"onImportComplete"`
	OnUploadComplete bool `json:"onUploadComplete"`
}

const (
	StorageModeQuota     = "quota"
	StorageModeUnlimited = "unlimited"
	QualityOriginal      = "original"
	QualitySaver         = "saver"
)

// Dedup modes control what happens when the persistent state points at an
// asset that is no longer present in Immich.
const (
	// DedupModeSafe respects deletions: a missing asset is skipped, but a
	// replacement with the same Google item ID (for example a compressed
	// asset created by immich-compressor) is adopted automatically.
	DedupModeSafe = "safe"
	// DedupModeRepair re-uploads assets that are missing from Immich and have
	// no replacement. Use this to restore photos you deleted by accident.
	DedupModeRepair = "repair"
	// DedupModeFast trusts the local state without checking Immich. It is the
	// fastest mode, but stale IDs are not repaired automatically.
	DedupModeFast = "fast"
)

type Config struct {
	ApiKey                 string               `json:"apiKey"`
	ApiURL                 string               `json:"apiURL"`
	Debug                  bool                 `json:"debug"`                  // Optional, enable verbose logging
	Workers                int                  `json:"workers"`                // Optional, default 1
	AlbumWorkers           int                  `json:"albumWorkers"`           // Optional, concurrent album processing (default 1)
	StrictMetadata         bool                 `json:"strictMetadata"`         // Optional, skip items with missing dates
	SkipVideos             bool                 `json:"skipVideos"`             // Optional, skip video items entirely
	DataDir                string               `json:"dataDir"`                // Optional, directory for persistent state (default "./data")
	DedupMode              string               `json:"dedupMode"`              // Optional: safe (default), repair, fast
	UpdateExistingMetadata bool                 `json:"updateExistingMetadata"` // Optional, backfill descriptions of existing assets
	UIMode                 string               `json:"uiMode,omitempty"`       // Optional: simple, standard, advanced
	Notifications          NotificationsConfig  `json:"notifications"`
	GoogleUpload           GoogleUploadConfig   `json:"googleUpload"`
	Accounts               []AccountMapping     `json:"accounts,omitempty"`
	GooglePhotos           []GooglePhotosConfig `json:"googlePhotos"`
}

// EffectiveUIMode returns the interface mode the user should see. An empty
// value keeps existing installations on standard, while a multi-account setup
// implies advanced because its routing needs the extra controls.
func (c *Config) EffectiveUIMode() string {
	if c != nil {
		switch mode := strings.ToLower(strings.TrimSpace(c.UIMode)); mode {
		case UIModeSimple:
			return UIModeSimple
		case UIModeAdvanced:
			return UIModeAdvanced
		case UIModeStandard:
			return UIModeStandard
		case "":
			if len(c.Accounts) > 0 {
				return UIModeAdvanced
			}
		}
	}
	return UIModeStandard
}

// EffectiveDedupMode returns the configured dedup mode, defaulting to safe.
func (c *Config) EffectiveDedupMode() string {
	switch strings.ToLower(strings.TrimSpace(c.DedupMode)) {
	case DedupModeSafe, "":
		return DedupModeSafe
	case DedupModeRepair:
		return DedupModeRepair
	case DedupModeFast:
		return DedupModeFast
	default:
		return DedupModeSafe
	}
}

// VerifyDedupState reports whether state entries should be checked against
// the assets that currently exist in Immich.
func (c *Config) VerifyDedupState() bool {
	return c.EffectiveDedupMode() != DedupModeFast
}

// AdoptReplacements reports whether a missing state asset may be replaced by
// an existing asset with the same Google item ID (e.g. a compressed copy).
func (c *Config) AdoptReplacements() bool {
	return c.EffectiveDedupMode() != DedupModeFast
}

// ReuploadMissing reports whether missing assets without a replacement should
// be downloaded and uploaded again.
func (c *Config) ReuploadMissing() bool {
	return c.EffectiveDedupMode() == DedupModeRepair
}

// EffectiveStorageMode returns "quota" unless the user explicitly enabled the
// unofficial unlimited-storage mode.
func (c *Config) EffectiveStorageMode() string {
	if strings.EqualFold(strings.TrimSpace(c.GoogleUpload.StorageMode), StorageModeUnlimited) {
		return StorageModeUnlimited
	}
	return StorageModeQuota
}

// EffectiveQuality returns "original" unless storage saver was chosen.
func (c *Config) EffectiveQuality() string {
	if strings.EqualFold(strings.TrimSpace(c.GoogleUpload.Quality), QualitySaver) {
		return QualitySaver
	}
	return QualityOriginal
}

// EffectiveThreads returns a safe upload concurrency.
func (c *Config) EffectiveThreads() int {
	if c.GoogleUpload.Threads < 1 {
		return 1
	}
	return c.GoogleUpload.Threads
}

// EffectiveMaxRetries returns the configured retry count, defaulting to 3.
func (c *Config) EffectiveMaxRetries() int {
	if c.GoogleUpload.MaxRetries < 0 {
		return 0
	}
	if c.GoogleUpload.MaxRetries == 0 {
		return 3
	}
	return c.GoogleUpload.MaxRetries
}

// EffectiveGotohpBinary returns the CLI binary path or name.
func (c *Config) EffectiveGotohpBinary() string {
	if strings.TrimSpace(c.GoogleUpload.GotohpBinary) == "" {
		return "gotohp-cli"
	}
	return strings.TrimSpace(c.GoogleUpload.GotohpBinary)
}

// EffectiveGotohpConfig returns the credential config path.
func (c *Config) EffectiveGotohpConfig() string {
	if strings.TrimSpace(c.GoogleUpload.GotohpConfig) == "" {
		return "./data/gotohp.config"
	}
	return strings.TrimSpace(c.GoogleUpload.GotohpConfig)
}

// EffectiveWebCookiesFile returns the browser-session cookies path.
func (c *Config) EffectiveWebCookiesFile() string {
	if strings.TrimSpace(c.GoogleUpload.WebCookiesFile) == "" {
		return "./data/gphotos-web-cookies.txt"
	}
	return strings.TrimSpace(c.GoogleUpload.WebCookiesFile)
}

// EffectiveAccountCookiesFile returns the browser-session cookies path for one
// account. Accounts without an explicit path get their own file, so two Google
// accounts never share a session.
func EffectiveAccountCookiesFile(acc AccountMapping) string {
	if path := strings.TrimSpace(acc.WebCookiesFile); path != "" {
		return path
	}
	key := strings.TrimSpace(acc.ID)
	if key == "" {
		key = strings.TrimSpace(acc.GoogleEmail)
	}
	if key == "" {
		key = strings.TrimSpace(acc.ImmichUserID)
	}
	return "./data/gphotos-web-cookies-" + sanitizeAccountKey(key) + ".txt"
}

// NormalizeAccount trims an account mapping and fills its stable ID and
// per-account cookies path.
func NormalizeAccount(acc AccountMapping) AccountMapping {
	acc.ID = strings.TrimSpace(acc.ID)
	acc.Name = strings.TrimSpace(acc.Name)
	acc.ImmichUserID = strings.TrimSpace(acc.ImmichUserID)
	acc.ImmichAPIKey = strings.TrimSpace(acc.ImmichAPIKey)
	acc.GoogleEmail = strings.TrimSpace(acc.GoogleEmail)
	acc.WebCookiesFile = strings.TrimSpace(acc.WebCookiesFile)
	if acc.ID == "" {
		acc.ID = acc.ImmichUserID
	}
	if acc.WebCookiesFile == "" && acc.GoogleEmail != "" {
		acc.WebCookiesFile = EffectiveAccountCookiesFile(acc)
	}
	return acc
}

// sanitizeAccountKey makes an account id or email safe for a file name.
func sanitizeAccountKey(key string) string {
	key = strings.ToLower(strings.TrimSpace(key))
	var b strings.Builder
	for _, r := range key {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	key = strings.Trim(b.String(), "-.")
	if key == "" {
		return "account"
	}
	return key
}

// normalizeAccounts applies NormalizeAccount to every mapping.
func normalizeAccounts(accounts []AccountMapping) []AccountMapping {
	if len(accounts) == 0 {
		return accounts
	}
	out := make([]AccountMapping, len(accounts))
	for i, acc := range accounts {
		out[i] = NormalizeAccount(acc)
	}
	return out
}

// ValidateAccounts checks that mappings are usable and unambiguous.
func (c *Config) ValidateAccounts() error {
	ids := make(map[string]int, len(c.Accounts))
	users := make(map[string]int, len(c.Accounts))
	defaults := 0
	for i, acc := range c.Accounts {
		acc = NormalizeAccount(acc)
		if acc.ImmichUserID == "" {
			return fmt.Errorf("accounts[%d].immichUserId is required", i)
		}
		if acc.ID == "" {
			return fmt.Errorf("accounts[%d].id is required", i)
		}
		if previous, ok := ids[acc.ID]; ok {
			return fmt.Errorf("accounts[%d].id %q duplicates accounts[%d]", i, acc.ID, previous)
		}
		ids[acc.ID] = i
		if previous, ok := users[acc.ImmichUserID]; ok {
			return fmt.Errorf("accounts[%d].immichUserId duplicates accounts[%d]", i, previous)
		}
		users[acc.ImmichUserID] = i
		if acc.IsDefault {
			defaults++
			if defaults > 1 {
				return fmt.Errorf("only one account may be marked isDefault")
			}
		}
	}
	return nil
}

// AlbumStorageMode resolves a per-album storage override.
func (c *Config) AlbumStorageMode(gp GooglePhotosConfig) string {
	if gp.Upload != nil {
		switch strings.ToLower(strings.TrimSpace(gp.Upload.StorageMode)) {
		case StorageModeUnlimited:
			return StorageModeUnlimited
		case StorageModeQuota:
			return StorageModeQuota
		}
	}
	return c.EffectiveStorageMode()
}

// AlbumQuality resolves a per-album quality override.
func (c *Config) AlbumQuality(gp GooglePhotosConfig) string {
	if gp.Upload != nil {
		switch strings.ToLower(strings.TrimSpace(gp.Upload.Quality)) {
		case QualitySaver:
			return QualitySaver
		case QualityOriginal:
			return QualityOriginal
		}
	}
	return c.EffectiveQuality()
}

// AlbumSkipLivePhotos resolves a per-album Live Photo override.
func (c *Config) AlbumSkipLivePhotos(gp GooglePhotosConfig) bool {
	if gp.Upload != nil && gp.Upload.SkipLivePhotos != nil {
		return *gp.Upload.SkipLivePhotos
	}
	return c.GoogleUpload.SkipLivePhotos
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

	// ENV vars take runtime precedence over the config file so rotated
	// keys (e.g. a new IMMICH_API_KEY deployed via container ENV) are picked
	// up on the next config read instead of causing auth crash loops.
	if v := os.Getenv("IMMICH_API_KEY"); v != "" {
		config.ApiKey = v
	}
	if v := os.Getenv("IMMICH_API_URL"); v != "" {
		config.ApiURL = v
	}

	config.ApiKey = strings.TrimSpace(config.ApiKey)
	config.ApiURL = strings.TrimSpace(config.ApiURL)
	config.DedupMode = strings.TrimSpace(config.DedupMode)
	config.UIMode = strings.ToLower(strings.TrimSpace(config.UIMode))
	config.Accounts = normalizeAccounts(config.Accounts)

	config.GoogleUpload.GotohpBinary = strings.TrimSpace(config.GoogleUpload.GotohpBinary)
	config.GoogleUpload.GotohpConfig = strings.TrimSpace(config.GoogleUpload.GotohpConfig)
	config.GoogleUpload.WebCookiesFile = strings.TrimSpace(config.GoogleUpload.WebCookiesFile)
	config.GoogleUpload.StorageMode = strings.TrimSpace(config.GoogleUpload.StorageMode)
	config.GoogleUpload.Quality = strings.TrimSpace(config.GoogleUpload.Quality)
	if config.GoogleUpload.GotohpBinary == "" {
		config.GoogleUpload.GotohpBinary = "gotohp-cli"
	}
	if config.GoogleUpload.GotohpConfig == "" {
		config.GoogleUpload.GotohpConfig = "./data/gotohp.config"
	}
	if config.GoogleUpload.WebCookiesFile == "" {
		config.GoogleUpload.WebCookiesFile = "./data/gphotos-web-cookies.txt"
	}
	if config.GoogleUpload.StorageMode == "" {
		config.GoogleUpload.StorageMode = StorageModeQuota
	}
	if config.GoogleUpload.Quality == "" {
		config.GoogleUpload.Quality = QualityOriginal
	}
	if config.GoogleUpload.Threads < 1 {
		config.GoogleUpload.Threads = 1
	}
	if config.GoogleUpload.MaxRetries == 0 {
		config.GoogleUpload.MaxRetries = 3
	}

	for i := range config.GooglePhotos {
		config.GooglePhotos[i].URL = strings.TrimSpace(config.GooglePhotos[i].URL)
		config.GooglePhotos[i].ImmichAlbumID = strings.TrimSpace(config.GooglePhotos[i].ImmichAlbumID)
		config.GooglePhotos[i].AlbumName = strings.TrimSpace(config.GooglePhotos[i].AlbumName)
		config.GooglePhotos[i].SyncInterval = strings.TrimSpace(config.GooglePhotos[i].SyncInterval)
		config.GooglePhotos[i].ImmichAccountID = strings.TrimSpace(config.GooglePhotos[i].ImmichAccountID)
		if config.GooglePhotos[i].Upload != nil {
			config.GooglePhotos[i].Upload.StorageMode = strings.TrimSpace(config.GooglePhotos[i].Upload.StorageMode)
			config.GooglePhotos[i].Upload.Quality = strings.TrimSpace(config.GooglePhotos[i].Upload.Quality)
		}
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
	switch strings.ToLower(strings.TrimSpace(c.DedupMode)) {
	case "", DedupModeSafe, DedupModeRepair, DedupModeFast:
	default:
		return fmt.Errorf("dedupMode must be one of %q, %q or %q", DedupModeSafe, DedupModeRepair, DedupModeFast)
	}
	switch strings.ToLower(strings.TrimSpace(c.UIMode)) {
	case "", UIModeSimple, UIModeStandard, UIModeAdvanced:
	default:
		return fmt.Errorf("uiMode must be one of %q, %q or %q", UIModeSimple, UIModeStandard, UIModeAdvanced)
	}
	if err := c.ValidateAccounts(); err != nil {
		return err
	}
	if err := validateStorageMode("googleUpload.storageMode", c.GoogleUpload.StorageMode); err != nil {
		return err
	}
	if err := validateQuality("googleUpload.quality", c.GoogleUpload.Quality); err != nil {
		return err
	}
	if c.GoogleUpload.Threads < 0 {
		return fmt.Errorf("googleUpload.threads must be non-negative")
	}
	if c.GoogleUpload.MaxRetries < 0 {
		return fmt.Errorf("googleUpload.maxRetries must be non-negative")
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
		if gp.UploadToGoogle && gp.ImmichAlbumID == "" {
			return fmt.Errorf("googlePhotos[%d].immichAlbumId is required when uploadToGoogle is enabled", i)
		}
		if gp.Upload != nil {
			if err := validateStorageMode(fmt.Sprintf("googlePhotos[%d].upload.storageMode", i), gp.Upload.StorageMode); err != nil {
				return err
			}
			if err := validateQuality(fmt.Sprintf("googlePhotos[%d].upload.quality", i), gp.Upload.Quality); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateStorageMode(field, value string) error {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", StorageModeQuota, StorageModeUnlimited:
		return nil
	default:
		return fmt.Errorf("%s must be %q or %q", field, StorageModeQuota, StorageModeUnlimited)
	}
}

func validateQuality(field, value string) error {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", QualityOriginal, QualitySaver:
		return nil
	default:
		return fmt.Errorf("%s must be %q or %q", field, QualityOriginal, QualitySaver)
	}
}
