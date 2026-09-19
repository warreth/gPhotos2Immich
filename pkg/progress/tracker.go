package progress

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	barWidth             = 30
	ttyUpdateInterval    = 3 * time.Second
	nonTTYUpdateInterval = 30 * time.Second
	nonTTYPercentStep    = 10 // only print at every 10% milestone in Docker/non-TTY
	filledBlock          = "█"
	emptyBlock           = "░"
)

// Tracker tracks download/upload progress for an album
type Tracker struct {
	albumName       string
	totalItems      int
	processedItems  atomic.Int64
	addedItems      atomic.Int64
	skippedItems    atomic.Int64
	failedItems     atomic.Int64
	bytesDownloaded atomic.Int64
	bytesUploaded   atomic.Int64
	startTime       time.Time
	debug           bool
	isTTY           bool
	lastLogPercent  int // last milestone printed in non-TTY mode
	done            chan struct{}
	once            sync.Once
}

var (
	GlobalTrackerMu sync.RWMutex
	GlobalTracker   *Tracker
	GlobalLastRun   time.Time
	GlobalNextRun   time.Time
)

// SetScheduleTimes updates global last run and next run timestamps
func SetScheduleTimes(lastRun, nextRun time.Time) {
	GlobalTrackerMu.Lock()
	defer GlobalTrackerMu.Unlock()
	if !lastRun.IsZero() {
		GlobalLastRun = lastRun
	}
	if !nextRun.IsZero() {
		GlobalNextRun = nextRun
	}
}

// ScheduleTimes returns the recorded last run and next run timestamps
func ScheduleTimes() (lastRun, nextRun time.Time) {
	GlobalTrackerMu.RLock()
	defer GlobalTrackerMu.RUnlock()
	return GlobalLastRun, GlobalNextRun
}

// ActiveStatus returns the current sync status
func ActiveStatus() (album string, processed, total int) {
	GlobalTrackerMu.RLock()
	defer GlobalTrackerMu.RUnlock()
	if GlobalTracker == nil {
		return "", 0, 0
	}
	return GlobalTracker.albumName, int(GlobalTracker.processedItems.Load()), GlobalTracker.totalItems
}

// detectTTY checks if stdout is a terminal (false in Docker logs)
func detectTTY() bool {
	stat, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return (stat.Mode() & os.ModeCharDevice) != 0
}

// New creates a new progress tracker for an album
func New(albumName string, totalItems int, debug bool) *Tracker {
	t := &Tracker{
		albumName:      albumName,
		totalItems:     totalItems,
		startTime:      time.Now(),
		debug:          debug,
		isTTY:          detectTTY(),
		lastLogPercent: -1,
		done:           make(chan struct{}),
	}
	GlobalTrackerMu.Lock()
	GlobalTracker = t
	GlobalTrackerMu.Unlock()
	return t
}

// RecordItem records a processed item with its transfer sizes
func (t *Tracker) RecordItem(downloaded, uploaded int64, wasAdded bool, wasSkipped bool, wasFailed bool) {
	t.processedItems.Add(1)
	t.bytesDownloaded.Add(downloaded)
	t.bytesUploaded.Add(uploaded)
	if wasAdded {
		t.addedItems.Add(1)
	}
	if wasSkipped {
		t.skippedItems.Add(1)
	}
	if wasFailed {
		t.failedItems.Add(1)
	}
}

// Start begins periodic progress printing (only in non-debug mode)
func (t *Tracker) Start() {
	if t.debug {
		return
	}
	if !t.isTTY {
		fmt.Printf("[%s] Processing %d items (progress updates every %d%%)\n",
			truncateAlbumName(t.albumName, 20), t.totalItems, nonTTYPercentStep)
	}
	go func() {
		interval := ttyUpdateInterval
		if !t.isTTY {
			interval = nonTTYUpdateInterval
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				t.printProgress()
			case <-t.done:
				return
			}
		}
	}()
}

// Stop ends periodic progress printing and prints the final summary
func (t *Tracker) Stop() {
	t.once.Do(func() {
		close(t.done)
		if !t.debug {
			t.printFinal()
		}
		GlobalTrackerMu.Lock()
		if GlobalTracker == t {
			GlobalTracker = nil
		}
		GlobalTrackerMu.Unlock()
	})
}

// printProgress prints a formatted progress line, rate-limited for Docker logs
func (t *Tracker) printProgress() {
	processed := int(t.processedItems.Load())
	total := t.totalItems
	if total == 0 {
		return
	}

	percent := int(float64(processed) / float64(total) * 100)

	// In non-TTY mode (Docker), only print at percentage milestones to avoid log spam
	if !t.isTTY {
		milestone := (percent / nonTTYPercentStep) * nonTTYPercentStep
		if milestone <= t.lastLogPercent {
			return
		}
		t.lastLogPercent = milestone
	}

	elapsed := time.Since(t.startTime)
	bar := renderBar(processed, total)
	speed := t.formatSpeeds(elapsed)
	eta := t.formatETA(processed, total, elapsed)

	fmt.Printf("[%s] %s %3d%% │ %d/%d │ %s │ ETA: %s\n",
		truncateAlbumName(t.albumName, 20),
		bar,
		percent,
		processed,
		total,
		speed,
		eta,
	)
}

// printFinal prints the completion summary line
func (t *Tracker) printFinal() {
	processed := int(t.processedItems.Load())
	added := int(t.addedItems.Load())
	skipped := int(t.skippedItems.Load())
	failed := int(t.failedItems.Load())
	elapsed := time.Since(t.startTime)
	totalDown := t.bytesDownloaded.Load()
	totalUp := t.bytesUploaded.Load()

	bar := renderBar(processed, t.totalItems)

	fmt.Printf("[%s] %s 100%% │ %d/%d │ +%d =%d ✗%d │ ↓ %s ↑ %s │ %s\n",
		truncateAlbumName(t.albumName, 20),
		bar,
		processed,
		t.totalItems,
		added,
		skipped,
		failed,
		formatBytes(totalDown),
		formatBytes(totalUp),
		formatDuration(elapsed),
	)
}

// formatSpeeds returns formatted download/upload speed string
func (t *Tracker) formatSpeeds(elapsed time.Duration) string {
	if elapsed.Seconds() < 0.5 {
		return "↓ --- ↑ ---"
	}
	secs := elapsed.Seconds()
	downSpeed := float64(t.bytesDownloaded.Load()) / secs
	upSpeed := float64(t.bytesUploaded.Load()) / secs
	return fmt.Sprintf("↓ %s/s ↑ %s/s", formatBytes(int64(downSpeed)), formatBytes(int64(upSpeed)))
}

// formatETA calculates and formats estimated time remaining
func (t *Tracker) formatETA(processed, total int, elapsed time.Duration) string {
	if processed == 0 {
		return "calculating..."
	}
	remaining := total - processed
	timePerItem := elapsed / time.Duration(processed)
	eta := timePerItem * time.Duration(remaining)
	return formatDuration(eta)
}

// renderBar creates a text-based progress bar
func renderBar(current, total int) string {
	if total == 0 {
		return strings.Repeat(emptyBlock, barWidth)
	}
	filled := (current * barWidth) / total
	if filled > barWidth {
		filled = barWidth
	}
	empty := barWidth - filled
	return strings.Repeat(filledBlock, filled) + strings.Repeat(emptyBlock, empty)
}

// formatBytes formats a byte count into a human-readable string
func formatBytes(b int64) string {
	const (
		kb = 1024
		mb = kb * 1024
		gb = mb * 1024
	)
	switch {
	case b >= gb:
		return fmt.Sprintf("%.1f GB", float64(b)/float64(gb))
	case b >= mb:
		return fmt.Sprintf("%.1f MB", float64(b)/float64(mb))
	case b >= kb:
		return fmt.Sprintf("%.1f KB", float64(b)/float64(kb))
	default:
		return fmt.Sprintf("%d B", b)
	}
}

// formatDuration formats a duration into a short human-readable string
func formatDuration(d time.Duration) string {
	d = d.Round(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		m := int(d.Minutes())
		s := int(d.Seconds()) % 60
		return fmt.Sprintf("%dm %ds", m, s)
	}
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	return fmt.Sprintf("%dh %dm", h, m)
}

// truncateAlbumName shortens album names for display
func truncateAlbumName(name string, maxLen int) string {
	if len(name) <= maxLen {
		// Pad to fixed width for alignment
		return name + strings.Repeat(" ", maxLen-len(name))
	}
	return name[:maxLen-1] + "…"
}
