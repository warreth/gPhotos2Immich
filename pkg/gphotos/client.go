// Package gphotos is the adapter for the optional Immich -> Google Photos
// direction. It wraps the gotohp-cli binary and deliberately knows nothing
// about public album scraping, Immich, or the web UI.
package gphotos

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Account is the Google account connected through gotohp.
type Account struct {
	Email     string
	Version   string
	CheckedAt time.Time
}

// UploadRequest describes one gotohp upload run.
type UploadRequest struct {
	StagingDir     string
	AlbumKey       string
	UseQuota       bool
	Saver          bool
	PairLivePhotos bool
	Threads        int
	ConfigPath     string
}

// UploadResult is one item from the gotohp JSON summary.
type UploadResult struct {
	Path        string
	Paths       []string
	MediaKey    string
	IsLivePhoto bool
	Skipped     bool
	SkipCode    string
	SkipReason  string
	Error       string
}

// Client is the seam used by the upload service and its tests.
type Client interface {
	AddAccount(ctx context.Context, oauthToken string) (string, error)
	ListAccounts(ctx context.Context) ([]string, error)
	RemoveAccount(ctx context.Context, email string) error
	Upload(ctx context.Context, req UploadRequest) ([]UploadResult, error)
	Version(ctx context.Context) (string, error)
}

// CLIClient runs the pinned gotohp-cli binary.
type CLIClient struct {
	Binary     string
	ConfigPath string
}

// NewCLIClient creates a client for the given binary and credential config.
func NewCLIClient(binary, configPath string) *CLIClient {
	return &CLIClient{Binary: binary, ConfigPath: configPath}
}

func (c *CLIClient) run(ctx context.Context, stdin string, args ...string) (string, string, error) {
	cmd := exec.CommandContext(ctx, c.Binary, args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		return stdout.String(), stderr.String(), fmt.Errorf("%s %s: %w: %s", c.Binary, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), stderr.String(), nil
}

// AddAccount exchanges an Embedded Setup oauth_token for a saved credential.
// The token is written to stdin so it never appears in the process list.
func (c *CLIClient) AddAccount(ctx context.Context, oauthToken string) (string, error) {
	stdout, _, err := c.run(ctx, oauthToken, "creds", "add", "-", "--config", c.ConfigPath)
	if err != nil {
		return "", err
	}
	return parseConnectedEmail(stdout)
}

func parseConnectedEmail(stdout string) (string, error) {
	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimSpace(line)
		idx := strings.Index(line, "Account ")
		if idx == -1 {
			continue
		}
		rest := line[idx+len("Account "):]
		if end := strings.Index(rest, " connected"); end != -1 {
			rest = rest[:end]
		}
		rest = strings.TrimSpace(rest)
		if rest != "" {
			return rest, nil
		}
	}
	return "", fmt.Errorf("gotohp did not report a connected account: %s", strings.TrimSpace(stdout))
}

// ListAccounts returns the emails stored in the gotohp config.
func (c *CLIClient) ListAccounts(ctx context.Context) ([]string, error) {
	stdout, _, err := c.run(ctx, "", "creds", "list", "--config", c.ConfigPath)
	if err != nil {
		return nil, err
	}
	var accounts []string
	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "*"))
		if line == "" || !strings.Contains(line, "@") {
			continue
		}
		if strings.HasPrefix(line, "Use ") {
			continue
		}
		accounts = append(accounts, line)
	}
	return accounts, nil
}

// RemoveAccount removes one stored credential.
func (c *CLIClient) RemoveAccount(ctx context.Context, email string) error {
	_, _, err := c.run(ctx, "", "creds", "remove", email, "--config", c.ConfigPath)
	return err
}

// Version returns the gotohp-cli version.
func (c *CLIClient) Version(ctx context.Context) (string, error) {
	stdout, _, err := c.run(ctx, "", "version")
	if err != nil {
		return "", err
	}
	fields := strings.Fields(strings.TrimSpace(stdout))
	if len(fields) == 0 {
		return "", fmt.Errorf("gotohp version output was empty")
	}
	return fields[len(fields)-1], nil
}

type uploadSummary struct {
	Total     int `json:"total"`
	Succeeded int `json:"succeeded"`
	Failed    int `json:"failed"`
	Skipped   int `json:"skipped"`
	Results   []struct {
		Path        string   `json:"path"`
		Paths       []string `json:"paths"`
		Success     bool     `json:"success"`
		Skipped     bool     `json:"skipped"`
		MediaKey    string   `json:"mediaKey"`
		IsLivePhoto bool     `json:"isLivePhoto"`
		SkipCode    string   `json:"skipCode"`
		SkipReason  string   `json:"skipReason"`
		Error       string   `json:"error"`
	} `json:"results"`
	Album *struct {
		Name       string   `json:"name"`
		ItemsAdded int      `json:"itemsAdded"`
		AlbumKeys  []string `json:"albumKeys"`
		Error      string   `json:"error"`
	} `json:"album"`
}

// AlbumError means the files were uploaded to the library but gotohp could not
// add them to the requested album. The results still carry the media keys.
type AlbumError struct {
	Message string
}

func (e *AlbumError) Error() string {
	return "gotohp could not add items to the album: " + e.Message
}

// Upload runs gotohp-cli upload and parses its JSON summary.
func (c *CLIClient) Upload(ctx context.Context, req UploadRequest) ([]UploadResult, error) {
	if req.StagingDir == "" {
		return nil, fmt.Errorf("staging directory is required")
	}
	if req.Threads < 1 {
		req.Threads = 1
	}
	args := []string{
		"upload", req.StagingDir,
		"--config", c.ConfigPath,
		"--no-tui",
		"--log-level", "error",
		"--threads", fmt.Sprintf("%d", req.Threads),
	}
	if req.AlbumKey != "" {
		args = append(args, "--album", req.AlbumKey)
	}
	if req.UseQuota {
		args = append(args, "--use-quota")
	}
	if req.Saver {
		args = append(args, "--saver")
	}
	if req.PairLivePhotos {
		args = append(args, "--pair-live-photos")
	}

	stdout, stderr, runErr := c.run(ctx, "", args...)
	summary, parseErr := parseUploadSummary(stdout)
	if parseErr != nil {
		if runErr != nil {
			return nil, fmt.Errorf("gotohp upload failed: %w (%s)", runErr, strings.TrimSpace(stderr))
		}
		return nil, parseErr
	}

	results := make([]UploadResult, 0, len(summary.Results))
	for _, item := range summary.Results {
		results = append(results, UploadResult{
			Path:        item.Path,
			Paths:       item.Paths,
			MediaKey:    item.MediaKey,
			IsLivePhoto: item.IsLivePhoto,
			Skipped:     item.Skipped,
			SkipCode:    item.SkipCode,
			SkipReason:  item.SkipReason,
			Error:       item.Error,
		})
	}
	if summary.Album != nil && summary.Album.Error != "" {
		return results, &AlbumError{Message: summary.Album.Error}
	}
	if runErr != nil && summary.Failed == 0 && summary.Skipped == 0 {
		return results, fmt.Errorf("gotohp upload failed: %w", runErr)
	}
	return results, nil
}

func parseUploadSummary(stdout string) (*uploadSummary, error) {
	trimmed := strings.TrimSpace(stdout)
	if trimmed == "" {
		return nil, fmt.Errorf("gotohp returned no JSON summary")
	}
	// The CLI prints exactly one JSON object. If extra lines appear, take the
	// last complete object.
	start := strings.Index(trimmed, "{")
	end := strings.LastIndex(trimmed, "}")
	if start == -1 || end == -1 || end <= start {
		return nil, fmt.Errorf("gotohp returned an unexpected summary: %s", trimmed)
	}
	var summary uploadSummary
	if err := json.Unmarshal([]byte(trimmed[start:end+1]), &summary); err != nil {
		return nil, fmt.Errorf("parse gotohp summary: %w", err)
	}
	return &summary, nil
}
