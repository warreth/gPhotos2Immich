package googlephotos

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Issue #16: the old 120s overall client timeout aborted large video
// downloads mid-body. The client must not impose an overall deadline; the
// transport still bounds connection setup.
func TestNewClientHasNoOverallTimeout(t *testing.T) {
	c := NewClient(slog.New(slog.NewTextHandler(io.Discard, nil)), 10)
	if c.client.Timeout != 0 {
		t.Fatalf("expected no overall client timeout, got %s", c.client.Timeout)
	}
	tr, ok := c.client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("expected *http.Transport, got %T", c.client.Transport)
	}
	if tr.ResponseHeaderTimeout <= 0 {
		t.Fatal("expected a response header timeout to bound connection setup")
	}
}

// A body that trickles in over several reads must still be downloaded in full.
func TestDownloadMediaSlowBody(t *testing.T) {
	jpg := fakeJPEG()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/item=d":
			w.Header().Set("Content-Type", "image/jpeg")
			flusher, _ := w.(http.Flusher)
			for _, chunk := range [][]byte{jpg[:4], jpg[4:8], jpg[8:]} {
				w.Write(chunk)
				if flusher != nil {
					flusher.Flush()
				}
				time.Sleep(50 * time.Millisecond)
			}
		case "/item=dv":
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := NewClient(slog.New(slog.NewTextHandler(io.Discard, nil)), 10)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	data, _, isVideo, isLive, err := DownloadMedia(ctx, client, server.URL+"/item")
	if err != nil {
		t.Fatalf("slow download failed: %v", err)
	}
	if isVideo || isLive {
		t.Fatalf("plain image misclassified (isVideo=%v isLive=%v)", isVideo, isLive)
	}
	if string(data) != string(jpg) {
		t.Fatalf("expected full image body, got %d bytes", len(data))
	}
}
