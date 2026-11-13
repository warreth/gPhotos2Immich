package immich

import (
	"context"
	"encoding/json"
	"fmt"
)

// Notification is one custom notification for an Immich user.
type Notification struct {
	UserID      string
	Level       string // info, success, warning, error
	Title       string
	Description string
	Data        map[string]any
}

// CreateNotification posts a custom notification through Immich's admin
// notification API. The API key must have the notification.create permission,
// which Immich currently grants only to admin keys with all permissions.
func (c *Client) CreateNotification(ctx context.Context, notification Notification) error {
	if notification.UserID == "" {
		return fmt.Errorf("notification userId is required")
	}
	switch notification.Level {
	case "", "info", "success", "warning", "error":
	default:
		return fmt.Errorf("invalid notification level %q", notification.Level)
	}
	if notification.Title == "" {
		return fmt.Errorf("notification title is required")
	}
	level := notification.Level
	if level == "" {
		level = "info"
	}
	payload := map[string]any{
		"userId":      notification.UserID,
		"type":        "Custom",
		"level":       level,
		"title":       notification.Title,
		"description": notification.Description,
	}
	if notification.Data != nil {
		payload["data"] = notification.Data
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode notification: %w", err)
	}
	if _, err := c.request(ctx, "POST", "admin/notifications", body, ""); err != nil {
		return fmt.Errorf("create Immich notification: %w", err)
	}
	return nil
}
