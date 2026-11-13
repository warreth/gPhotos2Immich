package web

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"warreth.dev/gphotos2immich/pkg/progress"
)

func TestActivityEndpoint(t *testing.T) {
	server := NewServer("dummy.json")
	req := httptest.NewRequest(http.MethodGet, "/api/activity", nil)
	w := httptest.NewRecorder()
	server.handleActivity(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var payload struct {
		Snapshot   progress.Snapshot `json:"snapshot"`
		ImmichUser string            `json:"immichUser"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload.Snapshot.AlbumsTotal < 0 {
		t.Fatalf("snapshot = %#v", payload.Snapshot)
	}
}

func TestLiveEndpointSendsSnapshot(t *testing.T) {
	server := NewServer("dummy.json")
	httpServer := httptest.NewServer(http.HandlerFunc(server.handleLive))
	defer httpServer.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, httpServer.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("content type = %q", resp.Header.Get("Content-Type"))
	}

	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var payload struct {
			Snapshot progress.Snapshot `json:"snapshot"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &payload); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return
	}
	t.Fatal("stream ended before a snapshot arrived")
}
