package upload

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"warreth.dev/gphotos2immich/pkg/gphotos"
	"warreth.dev/gphotos2immich/pkg/immich"
)

type fakeImmich struct {
	album       *immich.Album
	assets      map[string]*immich.Asset
	downloads   map[string][]byte
	downloadErr map[string]error
}

func (f *fakeImmich) GetAlbum(_ context.Context, _ string) (*immich.Album, error) {
	return f.album, nil
}

func (f *fakeImmich) GetAsset(_ context.Context, id string) (*immich.Asset, error) {
	asset, ok := f.assets[id]
	if !ok {
		return nil, errors.New("asset not found")
	}
	return asset, nil
}

func (f *fakeImmich) DownloadAssetToFile(_ context.Context, id, path string) error {
	if err := f.downloadErr[id]; err != nil {
		return err
	}
	data, ok := f.downloads[id]
	if !ok {
		data = []byte("data-" + id)
	}
	return os.WriteFile(path, data, 0644)
}

type fakeGoogle struct {
	req     gphotos.UploadRequest
	results []gphotos.UploadResult
	err     error
	files   []string
	uploads int
}

func (f *fakeGoogle) Upload(_ context.Context, req gphotos.UploadRequest) ([]gphotos.UploadResult, error) {
	f.req = req
	f.uploads++
	entries, _ := os.ReadDir(req.StagingDir)
	for _, entry := range entries {
		f.files = append(f.files, entry.Name())
	}
	return f.results, f.err
}

type writerCall struct {
	albumKey string
	shared   bool
	keys     []string
}

type fakeWriter struct {
	calls       []writerCall
	err         error
	unavailable bool
}

func (f *fakeWriter) Available() bool { return !f.unavailable }
func (f *fakeWriter) AddItems(_ context.Context, albumKey string, shared bool, mediaKeys []string) error {
	f.calls = append(f.calls, writerCall{albumKey: albumKey, shared: shared, keys: append([]string(nil), mediaKeys...)})
	return f.err
}

type fakeImported struct{ ids map[string]bool }

func (f fakeImported) AssetIDs() map[string]bool { return f.ids }

type fakeInspector struct {
	keys  map[string]bool
	err   error
	calls int
}

func (f *fakeInspector) AlbumMediaKeys(context.Context, string) (map[string]bool, error) {
	f.calls++
	return f.keys, f.err
}

func newAlbum(assets ...struct {
	id, name, typ, live string
}) *immich.Album {
	album := &immich.Album{Id: "album-1", AlbumName: "Album"}
	for _, a := range assets {
		album.Assets = append(album.Assets, struct {
			Id               string    `json:"id"`
			OriginalFileName string    `json:"originalFileName"`
			Type             string    `json:"type"`
			LivePhotoVideoID string    `json:"livePhotoVideoId"`
			FileCreatedAt    time.Time `json:"fileCreatedAt"`
			Checksum         string    `json:"checksum"`
		}{
			Id: a.id, OriginalFileName: a.name, Type: a.typ, LivePhotoVideoID: a.live,
			FileCreatedAt: time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC),
		})
	}
	return album
}

func TestSyncAlbumUploadsNewAssets(t *testing.T) {
	album := newAlbum(
		struct{ id, name, typ, live string }{"a1", "one.jpg", "IMAGE", ""},
		struct{ id, name, typ, live string }{"a2", "two.jpg", "IMAGE", ""},
	)
	state := NewState(filepath.Join(t.TempDir(), "upload-state.json"))
	google := &fakeGoogle{
		results: []gphotos.UploadResult{
			{Path: filepath.Join(t.TempDir(), "a1__one.jpg"), MediaKey: "g1"},
			{Path: filepath.Join(t.TempDir(), "a2__two.jpg"), MediaKey: "g2"},
		},
	}
	// The fake returns paths that do not exist; the service maps them by the
	// asset-id prefix, which is what matters.
	google.results[0].Path = "a1__one.jpg"
	google.results[1].Path = "a2__two.jpg"

	service := &Service{
		Immich:      &fakeImmich{album: album, downloads: map[string][]byte{}},
		Google:      google,
		State:       state,
		Imported:    fakeImported{ids: map[string]bool{}},
		StagingRoot: t.TempDir(),
		Threads:     1,
	}
	summary, err := service.SyncAlbum(context.Background(), AlbumMapping{
		ImmichAlbumID: "album-1", GoogleAlbumKey: "AF1QipAlbum", UseQuota: true,
	})
	if err != nil {
		t.Fatalf("SyncAlbum failed: %v", err)
	}
	if summary.Uploaded != 2 || summary.Failed != 0 {
		t.Fatalf("summary = %#v", summary)
	}
	if google.req.AlbumKey != "AF1QipAlbum" || !google.req.UseQuota {
		t.Fatalf("request = %#v", google.req)
	}
	if len(google.files) != 2 {
		t.Fatalf("staged files = %#v", google.files)
	}
	for _, id := range []string{"a1", "a2"} {
		asset, ok := state.Get("album-1", id)
		if !ok || asset.Status != "uploaded" || asset.GoogleMediaKey == "" || !asset.AddedToAlbum {
			t.Fatalf("state for %s = %#v", id, asset)
		}
	}
}

func TestSyncAlbumSkipsImportedAndUploaded(t *testing.T) {
	album := newAlbum(
		struct{ id, name, typ, live string }{"imported", "one.jpg", "IMAGE", ""},
		struct{ id, name, typ, live string }{"done", "two.jpg", "IMAGE", ""},
		struct{ id, name, typ, live string }{"new", "three.jpg", "IMAGE", ""},
	)
	state := NewState(filepath.Join(t.TempDir(), "upload-state.json"))
	state.Set("album-1", "done", AssetState{Status: "uploaded", GoogleMediaKey: "g-done"})

	service := &Service{
		Immich:      &fakeImmich{album: album, downloads: map[string][]byte{}},
		Google:      &fakeGoogle{results: []gphotos.UploadResult{{Path: "new__three.jpg", MediaKey: "g-new"}}},
		State:       state,
		Imported:    fakeImported{ids: map[string]bool{"imported": true}},
		StagingRoot: t.TempDir(),
		Threads:     1,
	}
	summary, err := service.SyncAlbum(context.Background(), AlbumMapping{
		ImmichAlbumID: "album-1", GoogleAlbumKey: "AF1QipAlbum",
	})
	if err != nil {
		t.Fatalf("SyncAlbum failed: %v", err)
	}
	if summary.Skipped != 2 || summary.Uploaded != 1 {
		t.Fatalf("summary = %#v", summary)
	}
}

func TestSyncAlbumLivePhotoStagesPair(t *testing.T) {
	album := newAlbum(struct{ id, name, typ, live string }{"live1", "IMG_1.HEIC", "IMAGE", "video1"})
	state := NewState(filepath.Join(t.TempDir(), "upload-state.json"))
	google := &fakeGoogle{results: []gphotos.UploadResult{{Path: "live1__IMG_1.HEIC", MediaKey: "g-live", IsLivePhoto: true}}}
	service := &Service{
		Immich: &fakeImmich{
			album: album,
			assets: map[string]*immich.Asset{
				"video1": {Id: "video1", OriginalFileName: "IMG_1.MOV", Type: "VIDEO"},
			},
			downloads: map[string][]byte{},
		},
		Google:      google,
		State:       state,
		Imported:    fakeImported{ids: map[string]bool{}},
		StagingRoot: t.TempDir(),
		Threads:     1,
	}
	summary, err := service.SyncAlbum(context.Background(), AlbumMapping{
		ImmichAlbumID: "album-1", GoogleAlbumKey: "AF1QipAlbum",
	})
	if err != nil {
		t.Fatalf("SyncAlbum failed: %v", err)
	}
	if summary.Uploaded != 1 {
		t.Fatalf("summary = %#v", summary)
	}
	if !google.req.PairLivePhotos {
		t.Fatal("expected PairLivePhotos to be true")
	}
	if len(google.files) != 2 {
		t.Fatalf("expected still and video, got %#v", google.files)
	}
	asset, _ := state.Get("album-1", "live1")
	if !asset.IsLivePhoto {
		t.Fatalf("expected live photo state, got %#v", asset)
	}
}

func TestSyncAlbumDownloadFailure(t *testing.T) {
	album := newAlbum(struct{ id, name, typ, live string }{"bad", "bad.jpg", "IMAGE", ""})
	state := NewState(filepath.Join(t.TempDir(), "upload-state.json"))
	service := &Service{
		Immich: &fakeImmich{
			album:       album,
			downloads:   map[string][]byte{},
			downloadErr: map[string]error{"bad": errors.New("network down")},
		},
		Google:      &fakeGoogle{},
		State:       state,
		Imported:    fakeImported{ids: map[string]bool{}},
		StagingRoot: t.TempDir(),
		Threads:     1,
	}
	summary, err := service.SyncAlbum(context.Background(), AlbumMapping{
		ImmichAlbumID: "album-1", GoogleAlbumKey: "AF1QipAlbum",
	})
	if err != nil {
		t.Fatalf("SyncAlbum failed: %v", err)
	}
	if summary.Failed != 1 {
		t.Fatalf("summary = %#v", summary)
	}
	asset, _ := state.Get("album-1", "bad")
	if asset.Status != "failed" {
		t.Fatalf("state = %#v", asset)
	}
}

func TestStateRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "upload-state.json")
	state := NewState(path)
	state.Set("album-1", "a1", AssetState{Status: "uploaded", GoogleMediaKey: "g1"})
	state.SetAlbumMeta("album-1", "AF1QipAlbum", "https://photos.app.goo.gl/x", time.Now())
	if err := state.Save(); err != nil {
		t.Fatal(err)
	}

	loaded := NewState(path)
	if err := loaded.Load(); err != nil {
		t.Fatal(err)
	}
	asset, ok := loaded.Get("album-1", "a1")
	if !ok || asset.GoogleMediaKey != "g1" {
		t.Fatalf("loaded asset = %#v", asset)
	}
	uploaded, _, _ := loaded.AlbumSummary("album-1")
	if uploaded != 1 {
		t.Fatalf("uploaded = %d", uploaded)
	}
}

func TestSyncAlbumSharedUsesWebAdd(t *testing.T) {
	album := newAlbum(struct{ id, name, typ, live string }{"a1", "one.jpg", "IMAGE", ""})
	state := NewState(filepath.Join(t.TempDir(), "upload-state.json"))
	google := &fakeGoogle{results: []gphotos.UploadResult{{Path: "a1__one.jpg", MediaKey: "g1"}}}
	writer := &fakeWriter{}
	service := &Service{
		Immich:       &fakeImmich{album: album, downloads: map[string][]byte{}},
		Google:       google,
		State:        state,
		Imported:     fakeImported{ids: map[string]bool{}},
		SharedWriter: writer,
		StagingRoot:  t.TempDir(),
		Threads:      1,
	}
	summary, err := service.SyncAlbum(context.Background(), AlbumMapping{
		ImmichAlbumID: "album-1", GoogleAlbumKey: "AF1QipShared", Shared: true,
	})
	if err != nil {
		t.Fatalf("SyncAlbum failed: %v", err)
	}
	if summary.Uploaded != 1 || summary.Failed != 0 {
		t.Fatalf("summary = %#v", summary)
	}
	if google.req.AlbumKey != "" {
		t.Fatalf("shared albums must upload to the library first, request = %#v", google.req)
	}
	if len(writer.calls) != 1 || !writer.calls[0].shared || writer.calls[0].albumKey != "AF1QipShared" {
		t.Fatalf("writer calls = %#v", writer.calls)
	}
	if len(writer.calls[0].keys) != 1 || writer.calls[0].keys[0] != "g1" {
		t.Fatalf("writer keys = %#v", writer.calls[0].keys)
	}
	asset, _ := state.Get("album-1", "a1")
	if !asset.AddedToAlbum || asset.Status != "uploaded" {
		t.Fatalf("state = %#v", asset)
	}
}

func TestSyncAlbumSharedAddFailureRetriesWithoutUpload(t *testing.T) {
	album := newAlbum(struct{ id, name, typ, live string }{"a1", "one.jpg", "IMAGE", ""})
	state := NewState(filepath.Join(t.TempDir(), "upload-state.json"))
	google := &fakeGoogle{results: []gphotos.UploadResult{{Path: "a1__one.jpg", MediaKey: "g1"}}}
	writer := &fakeWriter{err: errors.New("session expired")}
	service := &Service{
		Immich:       &fakeImmich{album: album, downloads: map[string][]byte{}},
		Google:       google,
		State:        state,
		Imported:     fakeImported{ids: map[string]bool{}},
		SharedWriter: writer,
		StagingRoot:  t.TempDir(),
		Threads:      1,
	}
	mapping := AlbumMapping{ImmichAlbumID: "album-1", GoogleAlbumKey: "AF1QipShared", Shared: true}
	if _, err := service.SyncAlbum(context.Background(), mapping); err == nil {
		t.Fatal("expected the first add to fail")
	}
	asset, _ := state.Get("album-1", "a1")
	if asset.Status != "uploaded" || asset.AddedToAlbum || asset.GoogleMediaKey != "g1" {
		t.Fatalf("pending state = %#v", asset)
	}
	pending := state.PendingAlbumAdds("album-1")
	if len(pending) != 1 || pending[0].AssetID != "a1" {
		t.Fatalf("pending = %#v", pending)
	}

	writer.err = nil
	google.uploads = 0
	summary, err := service.SyncAlbum(context.Background(), mapping)
	if err != nil {
		t.Fatalf("retry failed: %v", err)
	}
	if google.uploads != 0 {
		t.Fatalf("retry must not upload again, uploads = %d", google.uploads)
	}
	if summary.Uploaded != 1 {
		t.Fatalf("summary = %#v", summary)
	}
	asset, _ = state.Get("album-1", "a1")
	if !asset.AddedToAlbum {
		t.Fatalf("state after retry = %#v", asset)
	}
}

func TestSyncAlbumAlbumErrorFallsBackToWeb(t *testing.T) {
	album := newAlbum(struct{ id, name, typ, live string }{"a1", "one.jpg", "IMAGE", ""})
	state := NewState(filepath.Join(t.TempDir(), "upload-state.json"))
	google := &fakeGoogle{
		results: []gphotos.UploadResult{{Path: "a1__one.jpg", MediaKey: "g1"}},
		err:     &gphotos.AlbumError{Message: "Requested entity was not found"},
	}
	writer := &fakeWriter{}
	service := &Service{
		Immich:       &fakeImmich{album: album, downloads: map[string][]byte{}},
		Google:       google,
		State:        state,
		Imported:     fakeImported{ids: map[string]bool{}},
		SharedWriter: writer,
		StagingRoot:  t.TempDir(),
		Threads:      1,
	}
	summary, err := service.SyncAlbum(context.Background(), AlbumMapping{
		ImmichAlbumID: "album-1", GoogleAlbumKey: "AF1QipShared",
	})
	if err != nil {
		t.Fatalf("expected the web fallback to recover, got %v", err)
	}
	if summary.Uploaded != 1 {
		t.Fatalf("summary = %#v", summary)
	}
	if len(writer.calls) != 1 || !writer.calls[0].shared {
		t.Fatalf("writer calls = %#v", writer.calls)
	}
	asset, _ := state.Get("album-1", "a1")
	if !asset.AddedToAlbum {
		t.Fatalf("state = %#v", asset)
	}
}

func TestSyncAlbumSharedSkippedFileStillAddsToAlbum(t *testing.T) {
	album := newAlbum(struct{ id, name, typ, live string }{"a1", "one.jpg", "IMAGE", ""})
	state := NewState(filepath.Join(t.TempDir(), "upload-state.json"))
	google := &fakeGoogle{results: []gphotos.UploadResult{{
		Path: "a1__one.jpg", MediaKey: "g1", Skipped: true, SkipReason: "already uploaded",
	}}}
	writer := &fakeWriter{}
	service := &Service{
		Immich:       &fakeImmich{album: album, downloads: map[string][]byte{}},
		Google:       google,
		State:        state,
		Imported:     fakeImported{ids: map[string]bool{}},
		SharedWriter: writer,
		StagingRoot:  t.TempDir(),
		Threads:      1,
	}
	summary, err := service.SyncAlbum(context.Background(), AlbumMapping{
		ImmichAlbumID: "album-1", GoogleAlbumKey: "AF1QipShared", Shared: true,
	})
	if err != nil {
		t.Fatalf("SyncAlbum failed: %v", err)
	}
	if summary.Uploaded != 1 || summary.Skipped != 0 {
		t.Fatalf("summary = %#v", summary)
	}
	if len(writer.calls) != 1 || len(writer.calls[0].keys) != 1 || writer.calls[0].keys[0] != "g1" {
		t.Fatalf("writer calls = %#v", writer.calls)
	}
	asset, _ := state.Get("album-1", "a1")
	if !asset.AddedToAlbum || asset.Status != "uploaded" {
		t.Fatalf("state = %#v", asset)
	}
}

func TestSyncAlbumOwnedUsesWebAdd(t *testing.T) {
	album := newAlbum(struct{ id, name, typ, live string }{"a1", "one.jpg", "IMAGE", ""})
	state := NewState(filepath.Join(t.TempDir(), "upload-state.json"))
	google := &fakeGoogle{results: []gphotos.UploadResult{{Path: "a1__one.jpg", MediaKey: "g1"}}}
	writer := &fakeWriter{}
	service := &Service{
		Immich:       &fakeImmich{album: album, downloads: map[string][]byte{}},
		Google:       google,
		State:        state,
		Imported:     fakeImported{ids: map[string]bool{}},
		SharedWriter: writer,
		StagingRoot:  t.TempDir(),
		Threads:      1,
	}
	summary, err := service.SyncAlbum(context.Background(), AlbumMapping{
		ImmichAlbumID: "album-1", GoogleAlbumKey: "AF1QipOwned", UseWebAdd: true,
	})
	if err != nil {
		t.Fatalf("SyncAlbum failed: %v", err)
	}
	if summary.Uploaded != 1 {
		t.Fatalf("summary = %#v", summary)
	}
	if google.req.AlbumKey != "" {
		t.Fatalf("expected a library-only upload, request = %#v", google.req)
	}
	if len(writer.calls) != 1 || writer.calls[0].shared {
		t.Fatalf("owned album must use the E1Cajb payload, calls = %#v", writer.calls)
	}
}

func TestSyncAlbumReuploadMissing(t *testing.T) {
	album := newAlbum(struct{ id, name, typ, live string }{"a1", "one.jpg", "IMAGE", ""})
	state := NewState(filepath.Join(t.TempDir(), "upload-state.json"))
	state.Set("album-1", "a1", AssetState{
		GoogleMediaKey: "g1", Filename: "one.jpg", Status: "uploaded", AddedToAlbum: true,
	})
	google := &fakeGoogle{results: []gphotos.UploadResult{{
		Path: "a1__one.jpg", MediaKey: "g1", Skipped: true, SkipReason: "already uploaded",
	}}}
	writer := &fakeWriter{}
	inspector := &fakeInspector{keys: map[string]bool{}}
	service := &Service{
		Immich:       &fakeImmich{album: album, downloads: map[string][]byte{}},
		Google:       google,
		State:        state,
		Imported:     fakeImported{ids: map[string]bool{}},
		SharedWriter: writer,
		Inspector:    inspector,
		StagingRoot:  t.TempDir(),
		Threads:      1,
	}
	summary, err := service.SyncAlbum(context.Background(), AlbumMapping{
		ImmichAlbumID: "album-1", GoogleAlbumKey: "AF1QipShared",
		Shared: true, UseWebAdd: true, ReuploadDeleted: true,
	})
	if err != nil {
		t.Fatalf("SyncAlbum failed: %v", err)
	}
	if inspector.calls != 1 {
		t.Fatalf("inspector calls = %d", inspector.calls)
	}
	if google.uploads != 1 {
		t.Fatalf("expected the missing item to be staged, uploads = %d", google.uploads)
	}
	if summary.Uploaded != 1 {
		t.Fatalf("summary = %#v", summary)
	}
	if len(writer.calls) != 1 || len(writer.calls[0].keys) != 1 || writer.calls[0].keys[0] != "g1" {
		t.Fatalf("writer calls = %#v", writer.calls)
	}
	asset, _ := state.Get("album-1", "a1")
	if !asset.AddedToAlbum || asset.Status != "uploaded" {
		t.Fatalf("state = %#v", asset)
	}
}

func TestSyncAlbumReuploadPresent(t *testing.T) {
	album := newAlbum(struct{ id, name, typ, live string }{"a1", "one.jpg", "IMAGE", ""})
	state := NewState(filepath.Join(t.TempDir(), "upload-state.json"))
	state.Set("album-1", "a1", AssetState{
		GoogleMediaKey: "g1", Filename: "one.jpg", Status: "uploaded", AddedToAlbum: true,
	})
	google := &fakeGoogle{}
	writer := &fakeWriter{}
	inspector := &fakeInspector{keys: map[string]bool{"g1": true}}
	service := &Service{
		Immich:       &fakeImmich{album: album, downloads: map[string][]byte{}},
		Google:       google,
		State:        state,
		Imported:     fakeImported{ids: map[string]bool{}},
		SharedWriter: writer,
		Inspector:    inspector,
		StagingRoot:  t.TempDir(),
		Threads:      1,
	}
	summary, err := service.SyncAlbum(context.Background(), AlbumMapping{
		ImmichAlbumID: "album-1", GoogleAlbumKey: "AF1QipShared",
		Shared: true, UseWebAdd: true, ReuploadDeleted: true,
	})
	if err != nil {
		t.Fatalf("SyncAlbum failed: %v", err)
	}
	if google.uploads != 0 {
		t.Fatalf("present item was staged, uploads = %d", google.uploads)
	}
	if summary.Skipped != 1 || summary.Uploaded != 0 {
		t.Fatalf("summary = %#v", summary)
	}
	if len(writer.calls) != 0 {
		t.Fatalf("writer calls = %#v", writer.calls)
	}
}

func TestSyncAlbumReuploadInspectorError(t *testing.T) {
	album := newAlbum(struct{ id, name, typ, live string }{"a1", "one.jpg", "IMAGE", ""})
	state := NewState(filepath.Join(t.TempDir(), "upload-state.json"))
	state.Set("album-1", "a1", AssetState{
		GoogleMediaKey: "g1", Filename: "one.jpg", Status: "uploaded", AddedToAlbum: true,
	})
	google := &fakeGoogle{}
	writer := &fakeWriter{}
	inspector := &fakeInspector{err: errors.New("session expired")}
	service := &Service{
		Immich:       &fakeImmich{album: album, downloads: map[string][]byte{}},
		Google:       google,
		State:        state,
		Imported:     fakeImported{ids: map[string]bool{}},
		SharedWriter: writer,
		Inspector:    inspector,
		StagingRoot:  t.TempDir(),
		Threads:      1,
	}
	summary, err := service.SyncAlbum(context.Background(), AlbumMapping{
		ImmichAlbumID: "album-1", GoogleAlbumKey: "AF1QipShared",
		Shared: true, UseWebAdd: true, ReuploadDeleted: true,
	})
	if err != nil {
		t.Fatalf("SyncAlbum failed: %v", err)
	}
	if google.uploads != 0 {
		t.Fatalf("an inspection error must keep the safe default, uploads = %d", google.uploads)
	}
	if summary.Skipped != 1 {
		t.Fatalf("summary = %#v", summary)
	}
}

func TestSyncAlbumAlbumErrorWithoutWriter(t *testing.T) {
	album := newAlbum(struct{ id, name, typ, live string }{"a1", "one.jpg", "IMAGE", ""})
	state := NewState(filepath.Join(t.TempDir(), "upload-state.json"))
	google := &fakeGoogle{
		results: []gphotos.UploadResult{{Path: "a1__one.jpg", MediaKey: "g1"}},
		err:     &gphotos.AlbumError{Message: "Requested entity was not found"},
	}
	service := &Service{
		Immich:      &fakeImmich{album: album, downloads: map[string][]byte{}},
		Google:      google,
		State:       state,
		Imported:    fakeImported{ids: map[string]bool{}},
		StagingRoot: t.TempDir(),
		Threads:     1,
	}
	summary, err := service.SyncAlbum(context.Background(), AlbumMapping{
		ImmichAlbumID: "album-1", GoogleAlbumKey: "AF1QipShared",
	})
	if err == nil || !strings.Contains(err.Error(), "browser session") {
		t.Fatalf("error = %v", err)
	}
	if summary.Failed != 1 {
		t.Fatalf("summary = %#v", summary)
	}
	asset, _ := state.Get("album-1", "a1")
	if asset.Status != "failed" || asset.AddedToAlbum {
		t.Fatalf("state = %#v", asset)
	}
}

func TestSyncAlbumSharedWithoutWriter(t *testing.T) {
	service := &Service{}
	_, err := service.SyncAlbum(context.Background(), AlbumMapping{
		ImmichAlbumID: "album-1", GoogleAlbumKey: "AF1QipShared", Shared: true,
	})
	if err == nil || !strings.Contains(err.Error(), "browser session") {
		t.Fatalf("error = %v", err)
	}
}

func TestSyncAlbumSharedWithUnavailableWriter(t *testing.T) {
	service := &Service{SharedWriter: &fakeWriter{unavailable: true}}
	_, err := service.SyncAlbum(context.Background(), AlbumMapping{
		ImmichAlbumID: "album-1", GoogleAlbumKey: "AF1QipShared", Shared: true,
	})
	if err == nil || !strings.Contains(err.Error(), "browser session") {
		t.Fatalf("error = %v", err)
	}
}

func TestStateMigratesVersion1AsAdded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "upload-state.json")
	legacy := `{
		"version": 1,
		"albums": {
			"album-1": {
				"assets": {
					"a1": {"googleMediaKey": "g1", "status": "uploaded"}
				}
			}
		}
	}`
	if err := os.WriteFile(path, []byte(legacy), 0600); err != nil {
		t.Fatal(err)
	}
	state := NewState(path)
	if err := state.Load(); err != nil {
		t.Fatal(err)
	}
	asset, _ := state.Get("album-1", "a1")
	if !asset.AddedToAlbum {
		t.Fatalf("legacy uploaded asset must be treated as already added: %#v", asset)
	}
	if pending := state.PendingAlbumAdds("album-1"); len(pending) != 0 {
		t.Fatalf("pending = %#v", pending)
	}
}
