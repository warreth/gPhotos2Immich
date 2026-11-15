package notify

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"warreth.dev/gphotos2immich/pkg/config"
	"warreth.dev/gphotos2immich/pkg/immich"
)

const defaultNtfyURL = "https://ntfy.sh"

// NtfyClient posts notifications to one ntfy server.
type NtfyClient struct {
	HTTP *http.Client
}

// NewNtfyClient creates a client with a bounded timeout so a slow ntfy server
// never stalls a sync.
func NewNtfyClient() *NtfyClient {
	return &NtfyClient{HTTP: &http.Client{Timeout: 15 * time.Second}}
}

// Send posts one plain-text notification to the configured topic.
func (c *NtfyClient) Send(ctx context.Context, cfg config.NtfyConfig, title, message, priority string) error {
	base := strings.TrimRight(strings.TrimSpace(cfg.URL), "/")
	if base == "" {
		base = defaultNtfyURL
	}
	topic := strings.TrimSpace(cfg.Topic)
	if topic == "" {
		return fmt.Errorf("ntfy topic is required")
	}
	endpoint := base + "/" + url.PathEscape(topic)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader([]byte(message)))
	if err != nil {
		return fmt.Errorf("create ntfy request: %w", err)
	}
	req.Header.Set("Content-Type", "text/plain; charset=utf-8")
	if title != "" {
		req.Header.Set("Title", title)
	}
	if priority = strings.TrimSpace(priority); priority != "" {
		req.Header.Set("Priority", priority)
	}
	if token := strings.TrimSpace(cfg.Token); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("send ntfy notification: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		detail := strings.TrimSpace(string(body))
		if detail == "" {
			return fmt.Errorf("ntfy returned %s", resp.Status)
		}
		return fmt.Errorf("ntfy returned %s: %s", resp.Status, detail)
	}
	// Drain a small response so the connection can be reused.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	return nil
}

// TestNtfy sends a test notification to verify the ntfy settings.
func TestNtfy(ctx context.Context, cfg config.NtfyConfig) error {
	priority := strings.TrimSpace(cfg.Priority)
	if priority == "" {
		priority = "default"
	}
	return NewNtfyClient().Send(ctx, cfg, "gPhotos2Immich test", "Notifications are working.", priority)
}

// TargetFromConfig resolves the Immich client and user ID notifications should
// go to. An empty ImmichAccountID means the primary profile.
func TargetFromConfig(ctx context.Context, cfg *config.Config) (Sender, string, error) {
	if cfg == nil {
		return nil, "", fmt.Errorf("configuration is unavailable")
	}

	if strings.TrimSpace(cfg.Notifications.ImmichAccountID) == "" {
		if strings.TrimSpace(cfg.ApiURL) == "" || strings.TrimSpace(cfg.ApiKey) == "" {
			return nil, "", fmt.Errorf("Immich URL and API key are required for notifications")
		}
		client := immich.NewClient(cfg.ApiURL, cfg.ApiKey, 1)
		userID, _, err := client.GetUser(ctx)
		if err != nil {
			return nil, "", fmt.Errorf("read the primary Immich user: %w", err)
		}
		return client, userID, nil
	}

	wanted := strings.TrimSpace(cfg.Notifications.ImmichAccountID)
	for _, acc := range cfg.Accounts {
		if !strings.EqualFold(strings.TrimSpace(acc.ID), wanted) &&
			!strings.EqualFold(strings.TrimSpace(acc.ImmichUserID), wanted) {
			continue
		}
		key := strings.TrimSpace(acc.ImmichAPIKey)
		if key == "" {
			return nil, "", fmt.Errorf("notification target account has no Immich API key")
		}
		client := immich.NewClient(cfg.ApiURL, key, 1)
		userID := strings.TrimSpace(acc.ImmichUserID)
		if userID == "" {
			id, _, err := client.GetUser(ctx)
			if err != nil {
				return nil, "", fmt.Errorf("read the notification target user: %w", err)
			}
			userID = id
		}
		return client, userID, nil
	}
	return nil, "", fmt.Errorf("notification target account not found")
}
