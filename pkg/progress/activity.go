package progress

import (
	"sync"
	"time"
)

// Event is one structured activity entry shown in the Web UI. It is separate
// from the raw text log, which stays available for debugging.
type Event struct {
	ID       int64          `json:"id"`
	Time     time.Time      `json:"time"`
	Level    string         `json:"level"` // info, success, warning, error
	Category string         `json:"category"`
	Message  string         `json:"message"`
	Fields   map[string]any `json:"fields,omitempty"`
}

// AlbumProgress is the live progress for one album being imported.
type AlbumProgress struct {
	Name      string `json:"name"`
	Processed int    `json:"processed"`
	Total     int    `json:"total"`
	Added     int    `json:"added"`
	Skipped   int    `json:"skipped"`
	Failed    int    `json:"failed"`
}

// Snapshot is the complete state the Web UI renders. It is safe to serialize
// and small enough to send over one SSE connection.
type Snapshot struct {
	Running        bool            `json:"running"`
	Phase          string          `json:"phase"` // idle, importing, done
	CycleStartedAt time.Time       `json:"cycleStartedAt,omitempty"`
	CycleEndedAt   time.Time       `json:"cycleEndedAt,omitempty"`
	AlbumsTotal    int             `json:"albumsTotal"`
	AlbumsDone     int             `json:"albumsDone"`
	ActiveAlbums   []AlbumProgress `json:"activeAlbums"`
	Added          int             `json:"added"`
	Skipped        int             `json:"skipped"`
	Failed         int             `json:"failed"`

	UploadRunning   bool      `json:"uploadRunning"`
	UploadAlbum     string    `json:"uploadAlbum,omitempty"`
	UploadProcessed int       `json:"uploadProcessed"`
	UploadTotal     int       `json:"uploadTotal"`
	Uploaded        int       `json:"uploaded"`
	UploadFailed    int       `json:"uploadFailed"`
	UploadStartedAt time.Time `json:"uploadStartedAt,omitempty"`

	LastRun   time.Time `json:"lastRun,omitempty"`
	NextRun   time.Time `json:"nextRun,omitempty"`
	UpdatedAt time.Time `json:"updatedAt"`
	Events    []Event   `json:"events"`
}

type activityStore struct {
	mu     sync.Mutex
	snap   Snapshot
	events []Event
	nextID int64
	subs   map[chan struct{}]struct{}
}

var activity = &activityStore{subs: make(map[chan struct{}]struct{})}

const maxEvents = 200

func nowUTC() time.Time { return time.Now().UTC() }

// BeginCycle starts a new import cycle.
func BeginCycle(albums int) {
	activity.mu.Lock()
	defer activity.mu.Unlock()
	activity.snap.Running = true
	activity.snap.Phase = "importing"
	activity.snap.CycleStartedAt = nowUTC()
	activity.snap.CycleEndedAt = time.Time{}
	activity.snap.AlbumsTotal = albums
	activity.snap.AlbumsDone = 0
	activity.snap.ActiveAlbums = nil
	activity.snap.Added = 0
	activity.snap.Skipped = 0
	activity.snap.Failed = 0
	activity.addEventLocked(Event{
		Level: "info", Category: "import", Message: "Sync cycle started",
		Fields: map[string]any{"albums": albums},
	})
	activity.notifyLocked()
}

// StartAlbum registers one album as active.
func StartAlbum(name string, total int) {
	activity.mu.Lock()
	defer activity.mu.Unlock()
	for i := range activity.snap.ActiveAlbums {
		if activity.snap.ActiveAlbums[i].Name == name {
			activity.snap.ActiveAlbums[i].Total = total
			activity.snap.UpdatedAt = nowUTC()
			activity.notifyLocked()
			return
		}
	}
	activity.snap.ActiveAlbums = append(activity.snap.ActiveAlbums, AlbumProgress{Name: name, Total: total})
	activity.snap.UpdatedAt = nowUTC()
	activity.addEventLocked(Event{
		Level: "info", Category: "import", Message: "Album started",
		Fields: map[string]any{"album": name, "items": total},
	})
	activity.notifyLocked()
}

// RecordImport records one processed import item.
func RecordImport(album string, added, skipped, failed bool) {
	activity.mu.Lock()
	defer activity.mu.Unlock()
	for i := range activity.snap.ActiveAlbums {
		a := &activity.snap.ActiveAlbums[i]
		if a.Name != album {
			continue
		}
		a.Processed++
		if added {
			a.Added++
			activity.snap.Added++
		}
		if skipped {
			a.Skipped++
			activity.snap.Skipped++
		}
		if failed {
			a.Failed++
			activity.snap.Failed++
		}
		activity.snap.UpdatedAt = nowUTC()
		activity.notifyLocked()
		return
	}
}

// FinishAlbum removes an album from the active list and records its result.
func FinishAlbum(name string, added, skipped, failed int) {
	activity.mu.Lock()
	defer activity.mu.Unlock()
	for i := range activity.snap.ActiveAlbums {
		if activity.snap.ActiveAlbums[i].Name != name {
			continue
		}
		activity.snap.ActiveAlbums = append(activity.snap.ActiveAlbums[:i], activity.snap.ActiveAlbums[i+1:]...)
		break
	}
	activity.snap.AlbumsDone++
	level := "success"
	if failed > 0 {
		level = "error"
	}
	activity.addEventLocked(Event{
		Level: level, Category: "import", Message: "Album finished",
		Fields: map[string]any{
			"album": name, "added": added, "skipped": skipped, "failed": failed,
		},
	})
	activity.snap.UpdatedAt = nowUTC()
	activity.notifyLocked()
}

// FinishCycle closes the import cycle.
func FinishCycle(added, skipped, failed int) {
	activity.mu.Lock()
	defer activity.mu.Unlock()
	activity.snap.Running = false
	activity.snap.Phase = "idle"
	activity.snap.CycleEndedAt = nowUTC()
	activity.snap.LastRun = nowUTC()
	activity.snap.ActiveAlbums = nil
	level := "success"
	if failed > 0 {
		level = "error"
	}
	activity.addEventLocked(Event{
		Level: level, Category: "import", Message: "Sync cycle finished",
		Fields: map[string]any{"added": added, "skipped": skipped, "failed": failed},
	})
	activity.snap.UpdatedAt = nowUTC()
	activity.notifyLocked()
}

// FailAlbum records an album that could not be read or processed.
func FailAlbum(name string, err error) {
	activity.mu.Lock()
	defer activity.mu.Unlock()
	for i := range activity.snap.ActiveAlbums {
		if activity.snap.ActiveAlbums[i].Name == name {
			activity.snap.ActiveAlbums = append(activity.snap.ActiveAlbums[:i], activity.snap.ActiveAlbums[i+1:]...)
			break
		}
	}
	activity.snap.AlbumsDone++
	activity.addEventLocked(Event{
		Level: "error", Category: "import", Message: "Album failed",
		Fields: map[string]any{"album": name, "error": err.Error()},
	})
	activity.snap.UpdatedAt = nowUTC()
	activity.notifyLocked()
}

// BeginUpload starts one Google Photos upload run.
func BeginUpload(album string, total int) {
	activity.mu.Lock()
	defer activity.mu.Unlock()
	activity.snap.UploadRunning = true
	activity.snap.UploadAlbum = album
	activity.snap.UploadProcessed = 0
	activity.snap.UploadTotal = total
	activity.snap.Uploaded = 0
	activity.snap.UploadFailed = 0
	activity.snap.UploadStartedAt = nowUTC()
	activity.addEventLocked(Event{
		Level: "info", Category: "upload", Message: "Google Photos upload started",
		Fields: map[string]any{"album": album, "items": total},
	})
	activity.snap.UpdatedAt = nowUTC()
	activity.notifyLocked()
}

// SetUploadTotal updates the upload total once the Immich album is known.
func SetUploadTotal(total int) {
	activity.mu.Lock()
	defer activity.mu.Unlock()
	activity.snap.UploadTotal = total
	activity.snap.UpdatedAt = nowUTC()
	activity.notifyLocked()
}

// RecordUpload records one processed upload item.
func RecordUpload(uploaded, failed bool) {
	activity.mu.Lock()
	defer activity.mu.Unlock()
	activity.snap.UploadProcessed++
	if uploaded {
		activity.snap.Uploaded++
	}
	if failed {
		activity.snap.UploadFailed++
	}
	activity.snap.UpdatedAt = nowUTC()
	activity.notifyLocked()
}

// FinishUpload closes one upload run.
func FinishUpload(album string, uploaded, failed int) {
	activity.mu.Lock()
	defer activity.mu.Unlock()
	activity.snap.UploadRunning = false
	activity.snap.UploadAlbum = album
	activity.snap.Uploaded = uploaded
	activity.snap.UploadFailed = failed
	if activity.snap.UploadTotal > 0 {
		activity.snap.UploadProcessed = activity.snap.UploadTotal
	}
	level := "success"
	if failed > 0 {
		level = "error"
	}
	activity.addEventLocked(Event{
		Level: level, Category: "upload", Message: "Google Photos upload finished",
		Fields: map[string]any{"album": album, "uploaded": uploaded, "failed": failed},
	})
	activity.snap.UpdatedAt = nowUTC()
	activity.notifyLocked()
}

// AddEvent records one structured event.
func AddEvent(level, category, message string, fields map[string]any) {
	activity.mu.Lock()
	defer activity.mu.Unlock()
	activity.addEventLocked(Event{Level: level, Category: category, Message: message, Fields: fields})
	activity.snap.UpdatedAt = nowUTC()
	activity.notifyLocked()
}

func (s *activityStore) addEventLocked(event Event) {
	s.nextID++
	event.ID = s.nextID
	event.Time = nowUTC()
	s.events = append(s.events, event)
	if len(s.events) > maxEvents {
		s.events = s.events[len(s.events)-maxEvents:]
	}
}

func (s *activityStore) notifyLocked() {
	for ch := range s.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// CurrentSnapshot returns a copy of the current activity state.
func CurrentSnapshot() Snapshot {
	activity.mu.Lock()
	defer activity.mu.Unlock()
	snap := activity.snap
	snap.ActiveAlbums = append([]AlbumProgress(nil), activity.snap.ActiveAlbums...)
	snap.Events = append([]Event(nil), activity.events...)
	return snap
}

// Subscribe returns a signal channel and a cancel function for live updates.
func Subscribe() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	activity.mu.Lock()
	activity.subs[ch] = struct{}{}
	activity.mu.Unlock()
	cancel := func() {
		activity.mu.Lock()
		delete(activity.subs, ch)
		activity.mu.Unlock()
	}
	return ch, cancel
}
