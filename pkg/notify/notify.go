// Package notify sends user-facing notifications through Immich and ntfy.
//
// It is deliberately a thin policy layer: the caller decides what happened,
// this package decides whether the user asked for that event and forwards it
// through the enabled channels independently. A failed notification never
// interrupts a sync.
package notify

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"warreth.dev/gphotos2immich/pkg/config"
	"warreth.dev/gphotos2immich/pkg/immich"
)

// Event identifies a notification the user can enable or disable.
type Event string

const (
	EventSyncFailed     Event = "syncFailed"
	EventImportComplete Event = "importComplete"
	EventUploadComplete Event = "uploadComplete"
)

// Sender is the Immich notification call the service needs.
type Sender interface {
	CreateNotification(ctx context.Context, notification immich.Notification) error
}

// Service sends configured notifications for one Immich user and/or ntfy.
type Service struct {
	logger *slog.Logger

	mu     sync.RWMutex
	sender Sender
	userID string
	cfg    config.NotificationsConfig
	ntfy   *NtfyClient
}

// New creates a notification service. The user ID and target can be filled in
// after the Immich connection is validated.
func New(sender Sender, cfg config.NotificationsConfig, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{
		sender: sender,
		cfg:    cfg,
		logger: logger,
		ntfy:   NewNtfyClient(),
	}
}

// SetConfig replaces the notification policy, for example after the settings
// form was saved while the app was running.
func (s *Service) SetConfig(cfg config.NotificationsConfig) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cfg = cfg
}

// SetImmichTarget selects the Immich client and user that receive
// notifications. Either value may be empty when no target is available.
func (s *Service) SetImmichTarget(sender Sender, userID string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sender = sender
	s.userID = userID
}

// SetUserID sets the notification target once Immich has been reached.
func (s *Service) SetUserID(userID string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.userID = userID
}

// UserID returns the current notification target.
func (s *Service) UserID() string {
	if s == nil {
		return ""
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.userID
}

// SetNtfyClient replaces the ntfy HTTP client, mainly for tests.
func (s *Service) SetNtfyClient(client *NtfyClient) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if client == nil {
		client = NewNtfyClient()
	}
	s.ntfy = client
}

// Enabled reports whether either channel wants this event.
func (s *Service) Enabled(event Event) bool {
	if s == nil {
		return false
	}
	s.mu.RLock()
	cfg := s.cfg
	s.mu.RUnlock()
	return immichEventEnabled(cfg, event) || ntfyEventEnabled(cfg.Ntfy, event)
}

func immichEventEnabled(cfg config.NotificationsConfig, event Event) bool {
	if !cfg.Immich {
		return false
	}
	switch event {
	case EventSyncFailed:
		return cfg.OnSyncFailed
	case EventImportComplete:
		return cfg.OnImportComplete
	case EventUploadComplete:
		return cfg.OnUploadComplete
	default:
		return false
	}
}

func ntfyEventEnabled(cfg config.NtfyConfig, event Event) bool {
	if !cfg.Enabled {
		return false
	}
	switch event {
	case EventSyncFailed:
		return cfg.OnSyncFailed
	case EventImportComplete:
		return cfg.OnImportComplete
	case EventUploadComplete:
		return cfg.OnUploadComplete
	default:
		return false
	}
}

// Send delivers one notification through every enabled channel. It never
// returns an error: a notification is best-effort and must not fail the sync.
func (s *Service) Send(ctx context.Context, event Event, level, title, description string, data map[string]any) {
	if !s.Enabled(event) {
		return
	}

	s.mu.RLock()
	sender, userID, cfg, ntfy := s.sender, s.userID, s.cfg, s.ntfy
	s.mu.RUnlock()

	// The sync context is often canceled right after a run finishes. Keep the
	// notification working in that case while still bounding the call.
	ctx = context.WithoutCancel(ctx)
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	// Each channel is independent: a missing Immich key must not stop ntfy and
	// an ntfy outage must not stop the Immich notification.
	if immichEventEnabled(cfg, event) && sender != nil && userID != "" {
		if err := sender.CreateNotification(ctx, immich.Notification{
			UserID:      userID,
			Level:       level,
			Title:       title,
			Description: description,
			Data:        data,
		}); err != nil {
			s.logger.Warn("Could not send Immich notification", "event", event, "error", err)
		}
	}

	if ntfyEventEnabled(cfg.Ntfy, event) {
		if ntfy == nil {
			ntfy = NewNtfyClient()
		}
		if err := ntfy.Send(ctx, cfg.Ntfy, title, description, cfg.Ntfy.Priority); err != nil {
			s.logger.Warn("Could not send ntfy notification", "event", event, "error", err)
		}
	}
}
