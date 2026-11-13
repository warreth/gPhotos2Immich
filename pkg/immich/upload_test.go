package immich

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestUploadAssetFromFileStreams(t *testing.T) {
	payload := []byte("file-bytes-from-disk")
	var uploaded []byte
	var uploadedName string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/assets" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if err := r.ParseMultipartForm(10 << 20); err != nil {
			t.Errorf("parse multipart: %v", err)
		}
		file, header, err := r.FormFile("assetData")
		if err != nil {
			t.Errorf("form file: %v", err)
		} else {
			uploaded, _ = io.ReadAll(file)
			file.Close()
			uploadedName = header.Filename
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"asset-1","status":"created"}`))
	}))
	defer server.Close()

	path := filepath.Join(t.TempDir(), "photo.jpg")
	if err := os.WriteFile(path, payload, 0600); err != nil {
		t.Fatal(err)
	}
	client := NewClient(server.URL+"/api", "key", 1)
	id, duplicate, err := client.UploadAssetFromFile(context.Background(), path, "photo.jpg", time.Now(), "description")
	if err != nil {
		t.Fatalf("UploadAssetFromFile failed: %v", err)
	}
	if id != "asset-1" || duplicate {
		t.Fatalf("id = %q duplicate = %v", id, duplicate)
	}
	if string(uploaded) != string(payload) {
		t.Fatalf("uploaded = %q", uploaded)
	}
	if uploadedName != "photo.jpg" {
		t.Fatalf("filename = %q", uploadedName)
	}
}

func TestUploadAssetFromFileMissing(t *testing.T) {
	client := NewClient("http://example.invalid/api", "key", 1)
	if _, _, err := client.UploadAssetFromFile(context.Background(), "/does/not/exist.jpg", "x.jpg", time.Now(), ""); err == nil {
		t.Fatal("expected a missing file error")
	}
}
