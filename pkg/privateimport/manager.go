package privateimport

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"warreth.dev/gphotos2immich/pkg/gphotosweb"
)

// Status is the live state of the one private import job.
type Status struct {
	ID        string    `json:"id,omitempty"`
	Running   bool      `json:"running"`
	Album     string    `json:"album,omitempty"`
	Total     int       `json:"total"`
	Processed int       `json:"processed"`
	Imported  int       `json:"imported"`
	Skipped   int       `json:"skipped"`
	Failed    int       `json:"failed"`
	StartedAt time.Time `json:"startedAt,omitempty"`
	EndedAt   time.Time `json:"endedAt,omitempty"`
	Error     string    `json:"error,omitempty"`
}

// Manager runs one private import at a time and exposes its progress.
type Manager struct {
	mu      sync.Mutex
	service *Service
	status  Status
	cancel  context.CancelFunc
}

// NewManager creates a manager. A nil service makes Start return an error.
func NewManager(service *Service) *Manager {
	return &Manager{service: service}
}

// Start begins one import in the background. The caller’s context is not used
// for the job itself because HTTP handlers return before an import finishes.
func (m *Manager) Start(_ context.Context, request Request) (Status, error) {
	if m == nil || m.service == nil {
		return Status{}, fmt.Errorf("private import is not configured")
	}
	m.mu.Lock()
	if m.status.Running {
		status := m.status
		m.mu.Unlock()
		return status, fmt.Errorf("a private import is already running")
	}
	jobCtx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	m.status = Status{
		ID:        fmt.Sprintf("%d", time.Now().UnixNano()),
		Running:   true,
		Album:     request.AlbumTitle,
		StartedAt: time.Now().UTC(),
	}
	status := m.status
	m.mu.Unlock()

	go func() {
		summary, err := m.service.Import(jobCtx, request, func(update Summary) {
			m.mu.Lock()
			if m.status.Running {
				m.status.Album = update.Album
				m.status.Total = update.Total
				m.status.Imported = update.Imported
				m.status.Skipped = update.Skipped
				m.status.Failed = update.Failed
				m.status.Processed = update.Imported + update.Skipped + update.Failed
			}
			m.mu.Unlock()
		})
		m.mu.Lock()
		m.status.Running = false
		m.status.EndedAt = time.Now().UTC()
		m.status.Album = summary.Album
		m.status.Total = summary.Total
		m.status.Imported = summary.Imported
		m.status.Skipped = summary.Skipped
		m.status.Failed = summary.Failed
		m.status.Processed = summary.Imported + summary.Skipped + summary.Failed
		if err != nil {
			if errors.Is(err, context.Canceled) {
				m.status.Error = "Import canceled."
			} else {
				m.status.Error = err.Error()
			}
		}
		m.cancel = nil
		m.mu.Unlock()
	}()

	return status, nil
}

// Status returns the current or last import state.
func (m *Manager) Status() Status {
	if m == nil {
		return Status{}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.status
}

// Cancel stops the running import, if any.
func (m *Manager) Cancel() {
	if m == nil {
		return
	}
	m.mu.Lock()
	cancel := m.cancel
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// Service exposes the underlying service for the album list. It may be nil.
func (m *Manager) Service() *Service {
	if m == nil {
		return nil
	}
	return m.service
}

// Albums returns the private albums available to the current browser session.
func (m *Manager) Albums(ctx context.Context) ([]gphotosweb.Album, error) {
	if m == nil || m.service == nil {
		return nil, fmt.Errorf("private import is not configured")
	}
	return m.service.ListAlbums(ctx)
}
