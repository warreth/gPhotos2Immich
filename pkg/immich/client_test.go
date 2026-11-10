package immich

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNormalizeAssetName(t *testing.T) {
	tests := map[string]string{
		"gp_abc.jpg":         "gp_abc",
		"gp_abc.cmp.jpg":     "gp_abc",
		"gp_abc.MP.jpg":      "gp_abc",
		"gp_abc.mov":         "gp_abc",
		"gp_abc.cmp.mp4":     "gp_abc",
		"original-photo.jpg": "original-photo",
	}
	for input, want := range tests {
		if got := NormalizeAssetName(input); got != want {
			t.Errorf("NormalizeAssetName(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestAssetIndexSeparatesImagesAndVideos(t *testing.T) {
	index := NewAssetIndex()
	index.Add("img1", "gp_abc.cmp.jpg", "IMAGE", "")
	index.Add("vid1", "gp_abc.cmp.mp4", "VIDEO", "")
	index.Add("vid2", "gp_xyz.MOV", "", "")

	if id, ok := index.FindImage("gp_abc"); !ok || id != "img1" {
		t.Fatalf("expected image img1, got %q (%v)", id, ok)
	}
	if id, ok := index.FindVideo("gp_abc", ""); !ok || id != "vid1" {
		t.Fatalf("expected video vid1, got %q (%v)", id, ok)
	}
	if id, ok := index.FindVideo("gp_xyz", ""); !ok || id != "vid2" {
		t.Fatalf("expected inferred video vid2, got %q (%v)", id, ok)
	}
	if !index.IDs["img1"] || !index.IDs["vid1"] {
		t.Fatal("expected both IDs to be indexed")
	}
	if index.Types["img1"] != "IMAGE" || index.Types["vid1"] != "VIDEO" {
		t.Fatalf("unexpected types: %#v", index.Types)
	}
}

func TestSearchAssetsUsesFilterAndCursor(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		var body map[string]interface{}
		json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		if body["cursor"] == "next" {
			w.Write([]byte(`{"assets":{"items":[{"id":"vid1","originalFileName":"gp_def.mov","type":"VIDEO"}],"nextCursor":null,"nextPage":null}}`))
			return
		}
		w.Write([]byte(`{"assets":{"items":[{"id":"img1","originalFileName":"gp_abc.jpg","type":"IMAGE"}],"nextCursor":"next","nextPage":null}}`))
	}))
	defer server.Close()

	client := NewClient(server.URL, "key", 1)
	index, err := client.SearchAssets(context.Background(), "gphotos2immich-go")
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}
	if requests != 2 {
		t.Fatalf("expected 2 requests, got %d", requests)
	}
	if id, ok := index.FindImage("gp_abc"); !ok || id != "img1" {
		t.Fatalf("expected image img1, got %q (%v)", id, ok)
	}
	if id, ok := index.FindVideo("gp_def", ""); !ok || id != "vid1" {
		t.Fatalf("expected video vid1, got %q (%v)", id, ok)
	}
}

func TestSearchAssetsFallsBackToDeviceID(t *testing.T) {
	filterRequests := 0
	legacyRequests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]interface{}
		json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		if _, hasFilter := body["filter"]; hasFilter {
			filterRequests++
			// Simulate an older server that ignores the filter and returns
			// an unrelated asset.
			w.Write([]byte(`{"assets":{"items":[{"id":"other","originalFileName":"holiday.jpg","type":"IMAGE"}],"nextPage":null}}`))
			return
		}
		legacyRequests++
		w.Write([]byte(`{"assets":{"items":[{"id":"img1","originalFileName":"gp_abc.jpg","type":"IMAGE"}],"nextPage":null}}`))
	}))
	defer server.Close()

	client := NewClient(server.URL, "key", 1)
	index, err := client.SearchAssets(context.Background(), "gphotos2immich-go")
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}
	if filterRequests != 1 || legacyRequests != 1 {
		t.Fatalf("expected one filtered and one legacy request, got %d/%d", filterRequests, legacyRequests)
	}
	if id, ok := index.FindImage("gp_abc"); !ok || id != "img1" {
		t.Fatalf("expected legacy result img1, got %q (%v)", id, ok)
	}
	if index.IDs["other"] {
		t.Fatal("ignored-filter result must not leak into the index")
	}
}

func TestGetUserReturnsHTTPError(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		http.Error(w, `{"message":"Invalid API key"}`, http.StatusUnauthorized)
	}))
	defer server.Close()

	client := NewClient(server.URL, "bad-key", 1)
	_, _, err := client.GetUser(context.Background())
	if err == nil {
		t.Fatal("expected an error")
	}
	if requests != 1 {
		t.Fatalf("expected a 4xx response not to be retried, got %d requests", requests)
	}

	var httpErr *HTTPError
	if !errors.As(err, &httpErr) {
		t.Fatalf("expected *HTTPError, got %T: %v", err, err)
	}
	if httpErr.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected status 401, got %d", httpErr.StatusCode)
	}
}
