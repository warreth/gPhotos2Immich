package web

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"warreth.dev/gphotos2immich/pkg/gphotosweb"
	"warreth.dev/gphotos2immich/pkg/privateimport"
)

type stubPrivateManager struct {
	albums    []gphotosweb.Album
	albumsErr error
	status    privateimport.Status
	startErr  error
	started   privateimport.Request
	cancelled bool
}

func (s *stubPrivateManager) Start(_ context.Context, request privateimport.Request) (privateimport.Status, error) {
	s.started = request
	return s.status, s.startErr
}

func (s *stubPrivateManager) Status() privateimport.Status { return s.status }
func (s *stubPrivateManager) Cancel()                      { s.cancelled = true }
func (s *stubPrivateManager) Albums(context.Context) ([]gphotosweb.Album, error) {
	return s.albums, s.albumsErr
}

func TestPrivateAlbumsEndpoint(t *testing.T) {
	server := NewServer("dummy.json")
	server.SetPrivateImport(&stubPrivateManager{albums: []gphotosweb.Album{{Key: "a1", Title: "Private", Count: 4}}})
	req := httptest.NewRequest(http.MethodGet, "/api/google/private/albums", nil)
	w := httptest.NewRecorder()
	server.handlePrivateAlbums(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"title":"Private"`) {
		t.Fatalf("body = %s", w.Body.String())
	}
}

func TestPrivateAlbumsEndpointError(t *testing.T) {
	server := NewServer("dummy.json")
	server.SetPrivateImport(&stubPrivateManager{albumsErr: errors.New("session expired")})
	req := httptest.NewRequest(http.MethodGet, "/api/google/private/albums", nil)
	w := httptest.NewRecorder()
	server.handlePrivateAlbums(w, req)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d", w.Code)
	}
}

func TestPrivateImportEndpoint(t *testing.T) {
	manager := &stubPrivateManager{status: privateimport.Status{ID: "job-1", Running: true, Album: "Private"}}
	server := NewServer("dummy.json")
	server.SetPrivateImport(manager)
	body := bytes.NewBufferString(`{"albumKey":"a1","albumTitle":"Private","immichAlbumId":"target","immichAlbumName":""}`)
	req := httptest.NewRequest(http.MethodPost, "/api/google/private/import", body)
	w := httptest.NewRecorder()
	server.handlePrivateImport(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if manager.started.AlbumKey != "a1" || manager.started.ImmichAlbumID != "target" {
		t.Fatalf("request = %#v", manager.started)
	}
}

func TestPrivateImportConflict(t *testing.T) {
	server := NewServer("dummy.json")
	server.SetPrivateImport(&stubPrivateManager{startErr: errors.New("a private import is already running")})
	body := bytes.NewBufferString(`{"albumKey":"a1"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/google/private/import", body)
	w := httptest.NewRecorder()
	server.handlePrivateImport(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d", w.Code)
	}
}

func TestPrivateStatusEndpoint(t *testing.T) {
	server := NewServer("dummy.json")
	server.SetPrivateImport(&stubPrivateManager{status: privateimport.Status{ID: "job-1", Running: true, Total: 10, Imported: 3}})
	req := httptest.NewRequest(http.MethodGet, "/api/google/private/status", nil)
	w := httptest.NewRecorder()
	server.handlePrivateStatus(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"imported":3`) {
		t.Fatalf("body = %s", w.Body.String())
	}
}

func TestPrivateCancelEndpoint(t *testing.T) {
	manager := &stubPrivateManager{}
	server := NewServer("dummy.json")
	server.SetPrivateImport(manager)
	req := httptest.NewRequest(http.MethodPost, "/api/google/private/cancel", nil)
	w := httptest.NewRecorder()
	server.handlePrivateCancel(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if !manager.cancelled {
		t.Fatal("manager was not cancelled")
	}
}

func TestPrivateEndpointsWithoutManager(t *testing.T) {
	server := NewServer("dummy.json")
	for _, target := range []string{"/api/google/private/albums", "/api/google/private/import", "/api/google/private/status", "/api/google/private/cancel"} {
		method := http.MethodGet
		if target == "/api/google/private/import" || target == "/api/google/private/cancel" {
			method = http.MethodPost
		}
		req := httptest.NewRequest(method, target, nil)
		w := httptest.NewRecorder()
		switch target {
		case "/api/google/private/albums":
			server.handlePrivateAlbums(w, req)
		case "/api/google/private/import":
			server.handlePrivateImport(w, req)
		case "/api/google/private/status":
			server.handlePrivateStatus(w, req)
		case "/api/google/private/cancel":
			server.handlePrivateCancel(w, req)
		}
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s status = %d", target, w.Code)
		}
	}
}
