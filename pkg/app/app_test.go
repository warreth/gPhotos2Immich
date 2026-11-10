package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"warreth.dev/gphotos2immich/pkg/config"
	"warreth.dev/gphotos2immich/pkg/googlephotos"
	"warreth.dev/gphotos2immich/pkg/immich"
)

func TestTriggerSync(t *testing.T) {
	cfg := &config.Config{
		ApiKey:  "test-key",
		ApiURL:  "http://localhost:2283/api",
		DataDir: t.TempDir(),
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

func TestIsPermanentImmichError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"unauthorized", &immich.HTTPError{StatusCode: http.StatusUnauthorized}, true},
		{"not found", &immich.HTTPError{StatusCode: http.StatusNotFound}, true},
		{"rate limited", &immich.HTTPError{StatusCode: http.StatusTooManyRequests}, false},
		{"server error", &immich.HTTPError{StatusCode: http.StatusInternalServerError}, false},
		{"connection refused", errors.New("dial tcp: connection refused"), false},
		{"wrapped unauthorized", fmt.Errorf("request failed: %w", &immich.HTTPError{StatusCode: http.StatusUnauthorized}), true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isPermanentImmichError(tt.err); got != tt.want {
				t.Fatalf("isPermanentImmichError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestWaitForImmichSucceeds(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/users/me" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"user-1","name":"Test User"}`))
	}))
	defer server.Close()

	app := newTestApp(t, server.URL)
	id, name, err := app.waitForImmich(context.Background())
	if err != nil {
		t.Fatalf("connect failed: %v", err)
	}
	if id != "user-1" || name != "Test User" {
		t.Fatalf("got user %q (%q), want user-1 (Test User)", name, id)
	}
}

func TestWaitForImmichReturnsPermanentErrorWithoutRetrying(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		http.Error(w, `{"message":"Invalid API key"}`, http.StatusUnauthorized)
	}))
	defer server.Close()

	app := newTestApp(t, server.URL)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	start := time.Now()
	_, _, err := app.waitForImmich(ctx)
	if err == nil {
		t.Fatal("expected an error")
	}
	if requests != 1 {
		t.Fatalf("expected a permanent error not to be retried, got %d requests", requests)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("permanent error took %s, expected an immediate return", elapsed)
	}
}

func TestWaitForImmichRetriesTransientErrorsUntilCancelled(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		http.Error(w, "not ready", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	app := newTestApp(t, server.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_, _, err := app.waitForImmich(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected context deadline exceeded, got %v", err)
	}
	if requests == 0 {
		t.Fatal("expected at least one request")
	}
}

func TestBuildDescription(t *testing.T) {
	p := googlephotos.Photo{Photographer: "Alice", Description: "Holiday"}
	got := buildDescription(p, "Album", "https://photos.app.goo.gl/x")
	want := "Holiday\n\nPhotographer: Alice\n\nSource Album: Album (https://photos.app.goo.gl/x)"
	if got != want {
		t.Fatalf("buildDescription() = %q, want %q", got, want)
	}

	got = buildDescription(googlephotos.Photo{}, "Album", "url")
	if got != "Source Album: Album (url)" {
		t.Fatalf("empty description = %q", got)
	}
}

func TestDescriptionAlreadySet(t *testing.T) {
	if descriptionAlreadySet("", "Alice") {
		t.Fatal("empty description must not count as set")
	}
	if !descriptionAlreadySet("Photographer: Alice\n\nSource Album: x", "Alice") {
		t.Fatal("expected photographer match")
	}
	if !descriptionAlreadySet("My own caption", "Alice") {
		t.Fatal("user captions are left untouched and must not be queued")
	}
}

func TestResolveStateAsset(t *testing.T) {
	newApp := func(t *testing.T, mode string) *App {
		t.Helper()
		app := newTestApp(t, "http://localhost:2283/api")
		app.Cfg.DedupMode = mode
		return app
	}
	global := func(entries ...struct {
		id, name, typ string
	}) *immich.AssetIndex {
		index := immich.NewAssetIndex()
		for _, e := range entries {
			index.Add(e.id, e.name, e.typ, "")
		}
		return index
	}

	t.Run("no state entry", func(t *testing.T) {
		app := newApp(t, "safe")
		if _, action := app.resolveStateAsset("gp_x", false, global()); action != stateNone {
			t.Fatalf("expected stateNone, got %v", action)
		}
	})

	t.Run("valid state entry", func(t *testing.T) {
		app := newApp(t, "safe")
		app.State.Set("gp_x", "img1")
		idx := global(struct{ id, name, typ string }{"img1", "gp_x.jpg", "IMAGE"})
		id, action := app.resolveStateAsset("gp_x", false, idx)
		if action != stateUse || id != "img1" {
			t.Fatalf("expected stateUse img1, got %v %q", action, id)
		}
	})

	t.Run("deleted asset is skipped in safe mode", func(t *testing.T) {
		app := newApp(t, "safe")
		app.State.Set("gp_x", "gone")
		if _, action := app.resolveStateAsset("gp_x", false, global()); action != stateSkip {
			t.Fatalf("expected stateSkip, got %v", action)
		}
	})

	t.Run("compressed replacement is adopted", func(t *testing.T) {
		app := newApp(t, "safe")
		app.State.Set("gp_x", "gone")
		idx := global(struct{ id, name, typ string }{"cmp1", "gp_x.cmp.jpg", "IMAGE"})
		id, action := app.resolveStateAsset("gp_x", false, idx)
		if action != stateUse || id != "cmp1" {
			t.Fatalf("expected adopted cmp1, got %v %q", action, id)
		}
		if stored, _ := app.State.Get("gp_x"); stored != "cmp1" {
			t.Fatalf("state was not updated to replacement: %q", stored)
		}
	})

	t.Run("repair mode reuploads missing asset", func(t *testing.T) {
		app := newApp(t, "repair")
		app.State.Set("gp_x", "gone")
		if _, action := app.resolveStateAsset("gp_x", false, global()); action != stateReupload {
			t.Fatalf("expected stateReupload, got %v", action)
		}
	})

	t.Run("fast mode trusts state", func(t *testing.T) {
		app := newApp(t, "fast")
		app.State.Set("gp_x", "gone")
		id, action := app.resolveStateAsset("gp_x", false, global())
		if action != stateUse || id != "gone" {
			t.Fatalf("expected trusted state, got %v %q", action, id)
		}
	})

	t.Run("type mismatch is reprocessed", func(t *testing.T) {
		app := newApp(t, "safe")
		app.State.Set("gp_x", "vid1")
		idx := global(struct{ id, name, typ string }{id: "vid1", name: "gp_x.mov", typ: "VIDEO"})
		id, action := app.resolveStateAsset("gp_x", false, idx)
		if action != stateReupload || id != "vid1" {
			t.Fatalf("expected stateReupload with vid1, got %v %q", action, id)
		}
	})

	t.Run("missing global index trusts state", func(t *testing.T) {
		app := newApp(t, "safe")
		app.State.Set("gp_x", "gone")
		id, action := app.resolveStateAsset("gp_x", false, nil)
		if action != stateUse || id != "gone" {
			t.Fatalf("expected trusted state, got %v %q", action, id)
		}
	})
}

func TestApplyDescriptionUpdateWaitsForMetadata(t *testing.T) {
	var mu sync.Mutex
	gets := 0
	var updated string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			mu.Lock()
			gets++
			n := gets
			mu.Unlock()
			if n < 2 {
				w.Write([]byte(`{"id":"a1","hasMetadata":false}`))
				return
			}
			w.Write([]byte(`{"id":"a1","hasMetadata":true,"exifInfo":{"description":""}}`))
		case http.MethodPut:
			var body map[string]string
			json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			updated = body["description"]
			mu.Unlock()
			w.Write([]byte(`{}`))
		}
	}))
	defer server.Close()

	app := newTestApp(t, server.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	app.applyDescriptionUpdate(ctx, descriptionUpdate{
		assetID:         "a1",
		description:     "Photographer: Alice",
		waitForMetadata: true,
		force:           true,
	})

	mu.Lock()
	defer mu.Unlock()
	if updated != "Photographer: Alice" {
		t.Fatalf("expected description update, got %q", updated)
	}
	if gets < 2 {
		t.Fatalf("expected metadata polling, got %d GETs", gets)
	}
}

func TestApplyDescriptionUpdateLeavesUserCaption(t *testing.T) {
	var mu sync.Mutex
	puts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			w.Write([]byte(`{"id":"a1","hasMetadata":true,"exifInfo":{"description":"My own caption"}}`))
		case http.MethodPut:
			mu.Lock()
			puts++
			mu.Unlock()
			w.Write([]byte(`{}`))
		}
	}))
	defer server.Close()

	app := newTestApp(t, server.URL)
	app.applyDescriptionUpdate(context.Background(), descriptionUpdate{
		assetID:     "a1",
		description: "Photographer: Alice",
	})

	mu.Lock()
	defer mu.Unlock()
	if puts != 0 {
		t.Fatalf("user-edited description must not be overwritten, got %d PUTs", puts)
	}
}

func newTestApp(t *testing.T, apiURL string) *App {
	t.Helper()
	app, err := New(&config.Config{
		ApiKey:  "test-key",
		ApiURL:  apiURL,
		DataDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("failed to create app: %v", err)
	}
	return app
}
