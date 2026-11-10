package googlephotos

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

// parseRawItems decodes a raw scraped item list for fixture tests.
func parseRawItems(t *testing.T, raw string) []interface{} {
	t.Helper()
	var list []interface{}
	if err := json.Unmarshal([]byte(raw), &list); err != nil {
		t.Fatalf("failed to decode fixture: %v", err)
	}
	return list
}

// The album metadata marks videos with a video-info object (real fixture:
// an iPhone MOV shared into an album, trimmed to the relevant structure).
func TestItemIsVideoDetectsVideoMetadata(t *testing.T) {
	video := parseRawItems(t, `[["id1",["url",1080,1920,null,null,null,null,null,[null,null,1],[1645601]],1654019272000,"x",14400000,1784785332472,["a"],[[2]],2,{"101428965":[0,"v"],"15":57048,"76647426":[46929,null,1080,1920,null,4,134,null,[[1,null,1080,1920]],0,null,null,null,["url"]]}]]`)
	if !itemIsVideo(video[0].([]interface{})) {
		t.Fatal("expected video item to be detected as video")
	}
}

// Photos and motion-photo containers carry no video-info object (real
// fixtures from a Pixel motion photo and a plain JPEG).
func TestItemIsVideoFalseForPhotosAndMotionPhotos(t *testing.T) {
	photo := parseRawItems(t, `[["id2",["url",3072,4080,null,null,null,null,null,[null,null,1],[10326927]],1788523856669,"x",19800000,1788528482978,["a"],[[2]],2,{"101428965":[0,"v"],"146008172":[null,2054],"15":57689}]]`)
	if itemIsVideo(photo[0].([]interface{})) {
		t.Fatal("expected motion photo item to not be detected as video")
	}
	plain := parseRawItems(t, `[["id3",["url",400,300,null,null,null,null,null,[null,null,1],[9740727]],1262300400000,"x",3600000,1788502661928,["a"],[[2]],2,{"101428965":[0,"v"],"15":29}]]`)
	if itemIsVideo(plain[0].([]interface{})) {
		t.Fatal("expected plain photo item to not be detected as video")
	}
	// Empty and malformed arrays must not crash or claim video.
	if itemIsVideo(nil) || itemIsVideo([]interface{}{"str"}) {
		t.Fatal("malformed item arrays must not be detected as video")
	}
}

// parsePhotoItems must carry the video flag through to the Photo struct.
func TestParsePhotoItemsSetsIsVideo(t *testing.T) {
	list := parseRawItems(t, `[["id1",["url1",1080,1920,null,null,null,null,null,[null,null,1],[1645601]],1654019272000,"x",14400000,1784785332472,["a"],[[2]],2,{"76647426":[46929,null,1080,1920,null,4,134]}],["id2",["url2",400,300,null,null,null,null,null,[null,null,1],[9740727]],1262300400000,"x",3600000,1788502661928,["a"],[[2]],2,{"15":29}]]`)
	photos := parsePhotoItems(list, nil)
	if len(photos) != 2 {
		t.Fatalf("expected 2 photos, got %d", len(photos))
	}
	if !photos[0].IsVideo {
		t.Fatal("expected first item (video metadata present) to have IsVideo set")
	}
	if photos[1].IsVideo {
		t.Fatal("expected second item (plain photo) to not have IsVideo set")
	}
}

// The iPhone regression: a video item whose =dv stream carries Apple Live
// Photo QuickTime metadata must still classify as a standalone video.
// These iPhone clips keep their live-photo tags after being shared to a
// Google Photos album, so byte sniffing alone would call them live photos.
func TestDownloadItemMediaVideoWithAppleLivePhotoTags(t *testing.T) {
	appleMOV := append(ftypBox("qt  "), bytes.Repeat([]byte{0xBB}, 2048)...)
	appleMOV = append(appleMOV, []byte("com.apple.quicktime.live-photo")...)
	client := newTestClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/item=d":
			w.Header().Set("Content-Type", "image/jpeg")
			w.Write(fakeJPEG())
		case "/item=dv":
			w.Header().Set("Content-Type", "video/quicktime")
			w.Write(appleMOV)
		default:
			http.NotFound(w, r)
		}
	}))
	media, err := DownloadItemMedia(context.Background(), client, Photo{URL: "/item", IsVideo: true})
	if err != nil {
		t.Fatalf("download failed: %v", err)
	}
	if media.Kind != "video" {
		t.Fatalf("expected video, got kind %q", media.Kind)
	}
	if media.Ext != ".mov" {
		t.Fatalf("expected .mov extension, got %q", media.Ext)
	}
	if !bytes.Equal(media.Data, appleMOV) {
		t.Fatal("expected the =dv video bytes as primary data")
	}
	if media.VideoData != nil {
		t.Fatal("a standalone video must not carry a live-photo sidecar")
	}
}

// Issue #21 end-to-end: an iPhone HEIC Live Photo must produce a live photo
// with the HEIC still as primary data, not the .mov clip.
func TestDownloadItemMediaHeicLivePhoto(t *testing.T) {
	heic := append(ftypBox("heic"), bytes.Repeat([]byte{0x11}, 2048)...)
	sidecar := videoPayload("qt  ")
	sidecar = append(sidecar, []byte("com.apple.quicktime.content.identifier")...)
	client := newTestClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/item=d":
			w.Header().Set("Content-Type", "image/heif")
			w.Write(heic)
		case "/item=dv":
			w.Header().Set("Content-Type", "video/quicktime")
			w.Write(sidecar)
		default:
			http.NotFound(w, r)
		}
	}))
	media, err := DownloadItemMedia(context.Background(), client, Photo{URL: "/item"})
	if err != nil {
		t.Fatalf("download failed: %v", err)
	}
	if media.Kind != "live" {
		t.Fatalf("expected live photo, got kind %q", media.Kind)
	}
	if !bytes.Equal(media.Data, heic) {
		t.Fatal("expected the HEIC still as primary data")
	}
	if !bytes.Equal(media.VideoData, sidecar) {
		t.Fatal("expected the QuickTime clip as sidecar data")
	}
}

// A plain HEIC photo must be an image, not a video.
func TestDownloadItemMediaPlainHeic(t *testing.T) {
	heic := append(ftypBox("heic"), bytes.Repeat([]byte{0x11}, 2048)...)
	client := newTestClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/item=d":
			w.Header().Set("Content-Type", "image/heif")
			w.Write(heic)
		default:
			http.NotFound(w, r)
		}
	}))
	media, err := DownloadItemMedia(context.Background(), client, Photo{URL: "/item"})
	if err != nil {
		t.Fatalf("download failed: %v", err)
	}
	if media.Kind != "image" {
		t.Fatalf("expected image, got kind %q", media.Kind)
	}
	if media.Ext != ".heic" {
		t.Fatalf("expected .heic extension, got %q", media.Ext)
	}
}

// Plain videos (no apple tags) take the same path and must also stay videos.
func TestDownloadItemMediaPlainVideo(t *testing.T) {
	mp4 := videoPayload("isom")
	client := newTestClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/item=d":
			w.Header().Set("Content-Type", "image/jpeg")
			w.Write(fakeJPEG())
		case "/item=dv":
			w.Header().Set("Content-Type", "video/mp4")
			w.Write(mp4)
		default:
			http.NotFound(w, r)
		}
	}))
	media, err := DownloadItemMedia(context.Background(), client, Photo{URL: "/item", IsVideo: true})
	if err != nil {
		t.Fatalf("download failed: %v", err)
	}
	if media.Kind != "video" || !bytes.Equal(media.Data, mp4) {
		t.Fatalf("expected video with raw =dv bytes, got kind %q size %d", media.Kind, len(media.Data))
	}
}

// Photo items keep live-photo pairing: a real live photo serves its image
// on =d and the motion video on =dv.
func TestDownloadItemMediaLivePhoto(t *testing.T) {
	jpg := fakeJPEG()
	sidecar := videoPayload("qt  ")
	sidecar = append(sidecar, []byte("com.apple.quicktime.still-image-time")...)
	client := newTestClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/item=d":
			w.Header().Set("Content-Type", "image/jpeg")
			w.Write(jpg)
		case "/item=dv":
			w.Header().Set("Content-Type", "video/quicktime")
			w.Write(sidecar)
		default:
			http.NotFound(w, r)
		}
	}))
	media, err := DownloadItemMedia(context.Background(), client, Photo{URL: "/item"})
	if err != nil {
		t.Fatalf("download failed: %v", err)
	}
	if media.Kind != "live" {
		t.Fatalf("expected live photo, got kind %q", media.Kind)
	}
	if !bytes.Equal(media.Data, jpg) {
		t.Fatal("expected the =d image bytes as primary data")
	}
	if !bytes.Equal(media.VideoData, sidecar) {
		t.Fatal("expected the =dv bytes as the live photo video component")
	}
}

// A plain image stays an image, with no sidecar fetches.
func TestDownloadItemMediaPlainImage(t *testing.T) {
	client := newTestClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/item=d":
			w.Header().Set("Content-Type", "image/jpeg")
			w.Write(fakeJPEG())
		default:
			http.NotFound(w, r)
		}
	}))
	media, err := DownloadItemMedia(context.Background(), client, Photo{URL: "/item"})
	if err != nil {
		t.Fatalf("download failed: %v", err)
	}
	if media.Kind != "image" {
		t.Fatalf("expected image, got kind %q", media.Kind)
	}
}

// If the album metadata says video but =dv serves nothing usable, the item
// must fail loudly instead of silently uploading a poster frame.
func TestDownloadItemMediaVideoItemWithoutStream(t *testing.T) {
	client := newTestClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/item=d":
			w.Header().Set("Content-Type", "image/jpeg")
			w.Write(fakeJPEG())
		case "/item=dv":
			http.NotFound(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	_, err := DownloadItemMedia(context.Background(), client, Photo{URL: "/item", IsVideo: true})
	if err == nil {
		t.Fatal("expected an error when a video item has no usable video stream")
	}
}
