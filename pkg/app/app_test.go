package app

import (
	"testing"
	"time"

	"warreth.dev/gphotos2immich/pkg/config"
)

func TestTriggerSync(t *testing.T) {
	cfg := &config.Config{
		ApiKey: "test-key",
		ApiURL: "http://localhost:2283/api",
	}

	app, err := New(cfg)
	if err != nil {
		t.Fatalf("failed to create app: %v", err)
	}

	// TriggerSync on idle app should send to channel without blocking
	app.TriggerSync()

	select {
	case <-app.syncCh:
		// success
	case <-time.After(50 * time.Millisecond):
		t.Fatal("expected syncCh to receive event")
	}

	// Multiple TriggerSync calls should not block even if channel full (buffer size 1)
	app.TriggerSync()
	app.TriggerSync()

	select {
	case <-app.syncCh:
		// drained one
	default:
		t.Fatal("expected channel to have event")
	}
}
