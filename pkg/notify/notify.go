// Package notify sends user-facing notifications through Immich.
//
// It is deliberately a thin policy layer: the caller decides what happened,
// this package decides whether the user asked for that event and forwards it
// through the Immich admin notification API. A failed notification never
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

// Service sends configured notifications for one Immich user.
type Service struct {
	sender Sender
	cfg    config.NotificationsConfig
	logger *slog.Logger

	mu     sync.RWMutex
	userID string
}

// New creates a notification service. The user ID can be filled in after the
// Immich connection is validated.
func New(sender Sender, cfg config.NotificationsConfig, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{sender: sender, cfg: cfg, logger: logger}
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

// Enabled reports whether the user wants this event.
func (s *Service) Enabled(event Event) bool {
	if s == nil || s.sender == nil || !s.cfg.Immich || s.UserID() == "" {
		return false
	}
	switch event {
	case EventSyncFailed:
		return s.cfg.OnSyncFailed
	case EventImportComplete:
		return s.cfg.OnImportComplete
	case EventUploadComplete:
		return s.cfg.OnUploadComplete
	default:
		return false
	}
}

// Send delivers one notification if the event is enabled. It never returns an
// error: a notification is best-effort and must not fail the sync.
func (s *Service) Send(ctx context.Context, event Event, level, title, description string, data map[string]any) {
	if !s.Enabled(event) {
		return
	}
	userID := s.UserID()
	// The sync context is often canceled right after a run finishes. Keep the
	// notification working in that case while still bounding the call.
	ctx = context.WithoutCancel(ctx)
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	if err := s.sender.CreateNotification(ctx, immich.Notification{
		UserID:      userID,
		Level:       level,
		Title:       title,
		Description: description,
		Data:        data,
	}); err != nil {
		s.logger.Warn("Could not send Immich notification", "event", event, "error", err)
	}
}
