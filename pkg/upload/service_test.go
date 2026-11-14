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
	req            gphotos.UploadRequest
	requests       []gphotos.UploadRequest
	handler        func(req gphotos.UploadRequest) ([]gphotos.UploadResult, error)
	results        []gphotos.UploadResult
	err            error
	files          []string
	filesByAccount map[string][]string
	uploads        int
}

func (f *fakeGoogle) Upload(_ context.Context, req gphotos.UploadRequest) ([]gphotos.UploadResult, error) {
	f.req = req
	f.requests = append(f.requests, req)
	f.uploads++
	entries, _ := os.ReadDir(req.StagingDir)
	for _, entry := range entries {
		f.files = append(f.files, entry.Name())
		if f.filesByAccount == nil {
			f.filesByAccount = make(map[string][]string)
		}
		f.filesByAccount[req.Account] = append(f.filesByAccount[req.Account], entry.Name())
	}
	if f.handler != nil {
		return f.handler(req)
	}
	return f.results, f.err
}

// uploadEveryFile returns one successful result per staged file, keyed by the
// asset id prefix the uploader put in the file name.
func uploadEveryFile(req gphotos.UploadRequest) ([]gphotos.UploadResult, error) {
	entries, err := os.ReadDir(req.StagingDir)
	if err != nil {
		return nil, err
	}
	results := make([]gphotos.UploadResult, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		assetID := name
		if idx := strings.Index(name, "__"); idx > 0 {
			assetID = name[:idx]
		}
		results = append(results, gphotos.UploadResult{
			Path:     filepath.Join(req.StagingDir, name),
			MediaKey: "g-" + assetID,
		})
	}
	return results, nil
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
	id, name, typ, live, owner string
}) *immich.Album {
	album := &immich.Album{Id: "album-1", AlbumName: "Album"}
	for _, a := range assets {
		album.Assets = append(album.Assets, immich.AlbumAsset{
			Id: a.id, OriginalFileName: a.name, Type: a.typ, LivePhotoVideoID: a.live,
			OwnerID:       a.owner,
			FileCreatedAt: time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC),
		})
	}
	return album
}

func TestSyncAlbumUploadsNewAssets(t *testing.T) {
	album := newAlbum(
		struct{ id, name, typ, live, owner string }{"a1", "one.jpg", "IMAGE", "", ""},
		struct{ id, name, typ, live, owner string }{"a2", "two.jpg", "IMAGE", "", ""},
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
		struct{ id, name, typ, live, owner string }{"imported", "one.jpg", "IMAGE", "", ""},
		struct{ id, name, typ, live, owner string }{"done", "two.jpg", "IMAGE", "", ""},
		struct{ id, name, typ, live, owner string }{"new", "three.jpg", "IMAGE", "", ""},
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
	album := newAlbum(struct{ id, name, typ, live, owner string }{"live1", "IMG_1.HEIC", "IMAGE", "video1", ""})
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
	album := newAlbum(struct{ id, name, typ, live, owner string }{"bad", "bad.jpg", "IMAGE", "", ""})
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
	album := newAlbum(struct{ id, name, typ, live, owner string }{"a1", "one.jpg", "IMAGE", "", ""})
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
	album := newAlbum(struct{ id, name, typ, live, owner string }{"a1", "one.jpg", "IMAGE", "", ""})
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
	album := newAlbum(struct{ id, name, typ, live, owner string }{"a1", "one.jpg", "IMAGE", "", ""})
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
	album := newAlbum(struct{ id, name, typ, live, owner string }{"a1", "one.jpg", "IMAGE", "", ""})
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
	album := newAlbum(struct{ id, name, typ, live, owner string }{"a1", "one.jpg", "IMAGE", "", ""})
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
	album := newAlbum(struct{ id, name, typ, live, owner string }{"a1", "one.jpg", "IMAGE", "", ""})
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
	album := newAlbum(struct{ id, name, typ, live, owner string }{"a1", "one.jpg", "IMAGE", "", ""})
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
	album := newAlbum(struct{ id, name, typ, live, owner string }{"a1", "one.jpg", "IMAGE", "", ""})
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
	album := newAlbum(struct{ id, name, typ, live, owner string }{"a1", "one.jpg", "IMAGE", "", ""})
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

type fakeAccountResolver struct {
	routes      map[string]AccountRoute
	fallback    AccountRoute
	hasFallback bool
	owners      []string
	strict      []bool
}

func (f *fakeAccountResolver) ResolveOwner(ownerID string, strict bool) (AccountRoute, bool, bool) {
	f.owners = append(f.owners, ownerID)
	f.strict = append(f.strict, strict)
	if route, ok := f.routes[ownerID]; ok {
		return route, true, false
	}
	if strict || !f.hasFallback {
		return AccountRoute{}, false, false
	}
	return f.fallback, true, true
}

func (f *fakeAccountResolver) RouteForEmail(email string) (AccountRoute, bool) {
	if email == "" {
		return AccountRoute{}, false
	}
	for _, route := range f.routes {
		if route.GoogleEmail == email {
			return route, true
		}
	}
	if f.hasFallback && f.fallback.GoogleEmail == email {
		return f.fallback, true
	}
	return AccountRoute{}, false
}

// accountsByEmail indexes the fake upload requests by the account they used.
func accountsByEmail(requests []gphotos.UploadRequest) map[string][]gphotos.UploadRequest {
	out := make(map[string][]gphotos.UploadRequest)
	for _, req := range requests {
		out[req.Account] = append(out[req.Account], req)
	}
	return out
}

func TestSyncAlbumRoutesEachOwnerToItsOwnAccount(t *testing.T) {
	album := newAlbum(
		struct{ id, name, typ, live, owner string }{"a-warre", "warre.jpg", "IMAGE", "", "owner-warre"},
		struct{ id, name, typ, live, owner string }{"a-lotte", "lotte.jpg", "IMAGE", "", "owner-lotte"},
	)
	state := NewState(filepath.Join(t.TempDir(), "upload-state.json"))
	warreWriter := &fakeWriter{}
	lotteWriter := &fakeWriter{}
	google := &fakeGoogle{handler: uploadEveryFile}
	resolver := &fakeAccountResolver{routes: map[string]AccountRoute{
		"owner-warre": {GoogleEmail: "warre@gmail.com", SharedWriter: warreWriter},
		"owner-lotte": {GoogleEmail: "lotte@gmail.com", SharedWriter: lotteWriter},
	}}
	stagingRoot := t.TempDir()

	service := &Service{
		Immich:      &fakeImmich{album: album, downloads: map[string][]byte{}},
		Google:      google,
		State:       state,
		Imported:    fakeImported{ids: map[string]bool{}},
		Resolver:    resolver,
		StagingRoot: stagingRoot,
		Threads:     1,
	}
	summary, err := service.SyncAlbum(context.Background(), AlbumMapping{
		ImmichAlbumID: "album-1", GoogleAlbumKey: "AF1QipShared", Shared: true,
	})
	if err != nil {
		t.Fatalf("SyncAlbum failed: %v", err)
	}
	if summary.Uploaded != 2 || summary.Failed != 0 || summary.Skipped != 0 {
		t.Fatalf("summary = %#v", summary)
	}

	byAccount := accountsByEmail(google.requests)
	if len(byAccount) != 2 {
		t.Fatalf("expected one upload run per account, got %#v", google.requests)
	}
	warreReqs, ok := byAccount["warre@gmail.com"]
	if !ok || len(warreReqs) != 1 {
		t.Fatalf("warre uploads = %#v", byAccount)
	}
	lotteReqs, ok := byAccount["lotte@gmail.com"]
	if !ok || len(lotteReqs) != 1 {
		t.Fatalf("lotte uploads = %#v", byAccount)
	}

	// Each account uploads only its own owner's file, from its own staging
	// sub-directory, and adds to the album through its own web session.
	warreFiles := google.filesByAccount["warre@gmail.com"]
	if len(warreFiles) != 1 || !strings.HasPrefix(warreFiles[0], "a-warre__") {
		t.Fatalf("warre staged files = %#v", warreFiles)
	}
	if !strings.Contains(warreReqs[0].StagingDir, filepath.Join(sanitizeID("album-1"), sanitizeID("warre@gmail.com"))) {
		t.Fatalf("warre staging dir = %q", warreReqs[0].StagingDir)
	}
	lotteFiles := google.filesByAccount["lotte@gmail.com"]
	if len(lotteFiles) != 1 || !strings.HasPrefix(lotteFiles[0], "a-lotte__") {
		t.Fatalf("lotte staged files = %#v", lotteFiles)
	}
	if len(warreWriter.calls) != 1 || warreWriter.calls[0].keys[0] != "g-a-warre" {
		t.Fatalf("warre writer calls = %#v", warreWriter.calls)
	}
	if len(lotteWriter.calls) != 1 || lotteWriter.calls[0].keys[0] != "g-a-lotte" {
		t.Fatalf("lotte writer calls = %#v", lotteWriter.calls)
	}

	warreState, _ := state.Get("album-1", "a-warre")
	if warreState.UploadedBy != "warre@gmail.com" || !warreState.AddedToAlbum {
		t.Fatalf("warre state = %#v", warreState)
	}
	lotteState, _ := state.Get("album-1", "a-lotte")
	if lotteState.UploadedBy != "lotte@gmail.com" || !lotteState.AddedToAlbum {
		t.Fatalf("lotte state = %#v", lotteState)
	}
}

func TestSyncAlbumRoutesOwnedAlbumPerAccount(t *testing.T) {
	album := newAlbum(
		struct{ id, name, typ, live, owner string }{"a-warre", "warre.jpg", "IMAGE", "", "owner-warre"},
		struct{ id, name, typ, live, owner string }{"a-lotte", "lotte.jpg", "IMAGE", "", "owner-lotte"},
	)
	state := NewState(filepath.Join(t.TempDir(), "upload-state.json"))
	google := &fakeGoogle{handler: uploadEveryFile}
	resolver := &fakeAccountResolver{routes: map[string]AccountRoute{
		"owner-warre": {GoogleEmail: "warre@gmail.com"},
		"owner-lotte": {GoogleEmail: "lotte@gmail.com"},
	}}
	service := &Service{
		Immich:      &fakeImmich{album: album, downloads: map[string][]byte{}},
		Google:      google,
		State:       state,
		Imported:    fakeImported{ids: map[string]bool{}},
		Resolver:    resolver,
		StagingRoot: t.TempDir(),
		Threads:     1,
	}
	summary, err := service.SyncAlbum(context.Background(), AlbumMapping{
		ImmichAlbumID: "album-1", GoogleAlbumKey: "AF1QipOwned",
	})
	if err != nil {
		t.Fatalf("SyncAlbum failed: %v", err)
	}
	if summary.Uploaded != 2 {
		t.Fatalf("summary = %#v", summary)
	}
	for _, req := range google.requests {
		if req.AlbumKey != "AF1QipOwned" {
			t.Fatalf("owned upload must target the album directly: %#v", req)
		}
	}
	if len(accountsByEmail(google.requests)) != 2 {
		t.Fatalf("expected two account runs: %#v", google.requests)
	}
}

func TestSyncAlbumUnmappedOwnerFallsBackWhenNotStrict(t *testing.T) {
	album := newAlbum(
		struct{ id, name, typ, live, owner string }{"a-warre", "warre.jpg", "IMAGE", "", "owner-warre"},
		struct{ id, name, typ, live, owner string }{"a-papa", "papa.jpg", "IMAGE", "", "owner-papa"},
	)
	state := NewState(filepath.Join(t.TempDir(), "upload-state.json"))
	google := &fakeGoogle{handler: uploadEveryFile}
	primaryWriter := &fakeWriter{}
	resolver := &fakeAccountResolver{
		routes: map[string]AccountRoute{
			"owner-warre": {GoogleEmail: "warre@gmail.com", SharedWriter: primaryWriter},
		},
		fallback:    AccountRoute{GoogleEmail: "warre@gmail.com", SharedWriter: primaryWriter},
		hasFallback: true,
	}
	service := &Service{
		Immich:       &fakeImmich{album: album, downloads: map[string][]byte{}},
		Google:       google,
		State:        state,
		Imported:     fakeImported{ids: map[string]bool{}},
		Resolver:     resolver,
		SharedWriter: &fakeWriter{},
		StagingRoot:  t.TempDir(),
		Threads:      1,
	}
	summary, err := service.SyncAlbum(context.Background(), AlbumMapping{
		ImmichAlbumID: "album-1", GoogleAlbumKey: "AF1QipShared", Shared: true,
	})
	if err != nil {
		t.Fatalf("SyncAlbum failed: %v", err)
	}
	if summary.Uploaded != 2 || summary.Skipped != 0 || summary.Failed != 0 {
		t.Fatalf("summary = %#v", summary)
	}
	// The unmapped owner must be uploaded with the fallback account, not
	// dropped and not uploaded with some third account.
	byAccount := accountsByEmail(google.requests)
	if len(byAccount) != 1 || len(byAccount["warre@gmail.com"]) != 1 {
		t.Fatalf("expected a single fallback upload run: %#v", google.requests)
	}
	files := google.filesByAccount["warre@gmail.com"]
	if len(files) != 2 {
		t.Fatalf("fallback run must include both owners' files: %#v", files)
	}
	papaState, ok := state.Get("album-1", "a-papa")
	if !ok || papaState.UploadedBy != "warre@gmail.com" || !papaState.AddedToAlbum {
		t.Fatalf("papa state = %#v", papaState)
	}
	if len(primaryWriter.calls) != 1 || len(primaryWriter.calls[0].keys) != 2 {
		t.Fatalf("primary writer calls = %#v", primaryWriter.calls)
	}
}

func TestSyncAlbumUnmappedOwnerSkippedWhenStrict(t *testing.T) {
	album := newAlbum(
		struct{ id, name, typ, live, owner string }{"a-warre", "warre.jpg", "IMAGE", "", "owner-warre"},
		struct{ id, name, typ, live, owner string }{"a-papa", "papa.jpg", "IMAGE", "", "owner-papa"},
	)
	state := NewState(filepath.Join(t.TempDir(), "upload-state.json"))
	google := &fakeGoogle{handler: uploadEveryFile}
	resolver := &fakeAccountResolver{
		routes: map[string]AccountRoute{
			"owner-warre": {GoogleEmail: "warre@gmail.com"},
		},
		fallback:    AccountRoute{GoogleEmail: "warre@gmail.com"},
		hasFallback: true,
	}
	service := &Service{
		Immich:      &fakeImmich{album: album, downloads: map[string][]byte{}},
		Google:      google,
		State:       state,
		Imported:    fakeImported{ids: map[string]bool{}},
		Resolver:    resolver,
		StagingRoot: t.TempDir(),
		Threads:     1,
	}
	summary, err := service.SyncAlbum(context.Background(), AlbumMapping{
		ImmichAlbumID: "album-1", GoogleAlbumKey: "AF1QipOwned",
		StrictAccountMatching: true,
	})
	if err != nil {
		t.Fatalf("SyncAlbum failed: %v", err)
	}
	if summary.Uploaded != 1 || summary.Skipped != 1 || summary.Failed != 0 {
		t.Fatalf("summary = %#v", summary)
	}
	// The unmapped asset must not be uploaded under the wrong name.
	if len(google.requests) != 1 || google.requests[0].Account != "warre@gmail.com" {
		t.Fatalf("requests = %#v", google.requests)
	}
	files := google.filesByAccount["warre@gmail.com"]
	if len(files) != 1 || !strings.HasPrefix(files[0], "a-warre__") {
		t.Fatalf("unmapped asset was staged: %#v", files)
	}
	// No failed state either, so the asset is picked up automatically once the
	// owner is mapped.
	if _, ok := state.Get("album-1", "a-papa"); ok {
		t.Fatal("a skipped unmapped asset must not be recorded as failed")
	}
	if !resolver.strict[0] {
		t.Fatal("the resolver must receive the strict flag")
	}
}

func TestSyncAlbumFillsEmptyOwnerFromAsset(t *testing.T) {
	album := newAlbum(struct{ id, name, typ, live, owner string }{"a1", "one.jpg", "IMAGE", "", ""})
	state := NewState(filepath.Join(t.TempDir(), "upload-state.json"))
	google := &fakeGoogle{handler: uploadEveryFile}
	resolver := &fakeAccountResolver{routes: map[string]AccountRoute{
		"owner-lotte": {GoogleEmail: "lotte@gmail.com"},
	}}
	service := &Service{
		Immich: &fakeImmich{
			album:     album,
			assets:    map[string]*immich.Asset{"a1": {Id: "a1", OwnerID: "owner-lotte"}},
			downloads: map[string][]byte{},
		},
		Google:      google,
		State:       state,
		Imported:    fakeImported{ids: map[string]bool{}},
		Resolver:    resolver,
		StagingRoot: t.TempDir(),
		Threads:     1,
	}
	if _, err := service.SyncAlbum(context.Background(), AlbumMapping{
		ImmichAlbumID: "album-1", GoogleAlbumKey: "AF1QipOwned",
	}); err != nil {
		t.Fatalf("SyncAlbum failed: %v", err)
	}
	if len(resolver.owners) != 1 || resolver.owners[0] != "owner-lotte" {
		t.Fatalf("resolved owners = %#v", resolver.owners)
	}
	if len(google.requests) != 1 || google.requests[0].Account != "lotte@gmail.com" {
		t.Fatalf("requests = %#v", google.requests)
	}
}

func TestSyncAlbumWithoutResolverKeepsPrimaryAccount(t *testing.T) {
	album := newAlbum(struct{ id, name, typ, live, owner string }{"a1", "one.jpg", "IMAGE", "", "owner-warre"})
	state := NewState(filepath.Join(t.TempDir(), "upload-state.json"))
	google := &fakeGoogle{handler: uploadEveryFile}
	stagingRoot := t.TempDir()
	service := &Service{
		Immich:      &fakeImmich{album: album, downloads: map[string][]byte{}},
		Google:      google,
		State:       state,
		Imported:    fakeImported{ids: map[string]bool{}},
		StagingRoot: stagingRoot,
		Threads:     1,
	}
	if _, err := service.SyncAlbum(context.Background(), AlbumMapping{
		ImmichAlbumID: "album-1", GoogleAlbumKey: "AF1QipOwned",
	}); err != nil {
		t.Fatalf("SyncAlbum failed: %v", err)
	}
	if len(google.requests) != 1 {
		t.Fatalf("requests = %#v", google.requests)
	}
	if google.requests[0].Account != "" {
		t.Fatalf("a single-account setup must not pass --account: %#v", google.requests[0])
	}
	if google.requests[0].StagingDir != filepath.Join(stagingRoot, sanitizeID("album-1")) {
		t.Fatalf("staging dir = %q", google.requests[0].StagingDir)
	}
}

func TestSyncAlbumPendingAddUsesUploadingAccount(t *testing.T) {
	album := newAlbum(struct{ id, name, typ, live, owner string }{"a-lotte", "lotte.jpg", "IMAGE", "", "owner-lotte"})
	state := NewState(filepath.Join(t.TempDir(), "upload-state.json"))
	google := &fakeGoogle{handler: uploadEveryFile}
	warreWriter := &fakeWriter{}
	lotteWriter := &fakeWriter{err: errors.New("lotte session expired")}
	resolver := &fakeAccountResolver{routes: map[string]AccountRoute{
		"owner-warre": {GoogleEmail: "warre@gmail.com", SharedWriter: warreWriter},
		"owner-lotte": {GoogleEmail: "lotte@gmail.com", SharedWriter: lotteWriter},
	}}
	service := &Service{
		Immich:       &fakeImmich{album: album, downloads: map[string][]byte{}},
		Google:       google,
		State:        state,
		Imported:     fakeImported{ids: map[string]bool{}},
		Resolver:     resolver,
		SharedWriter: warreWriter,
		StagingRoot:  t.TempDir(),
		Threads:      1,
	}
	mapping := AlbumMapping{ImmichAlbumID: "album-1", GoogleAlbumKey: "AF1QipShared", Shared: true}
	if _, err := service.SyncAlbum(context.Background(), mapping); err == nil {
		t.Fatal("expected the shared add to fail")
	}
	pending := state.PendingAlbumAdds("album-1")
	if len(pending) != 1 || pending[0].UploadedBy != "lotte@gmail.com" {
		t.Fatalf("pending = %#v", pending)
	}

	lotteWriter.err = nil
	google.uploads = 0
	google.requests = nil
	summary, err := service.SyncAlbum(context.Background(), mapping)
	if err != nil {
		t.Fatalf("retry failed: %v", err)
	}
	if google.uploads != 0 {
		t.Fatalf("the retry must not upload again, uploads = %d", google.uploads)
	}
	if summary.Uploaded != 1 {
		t.Fatalf("summary = %#v", summary)
	}
	if len(lotteWriter.calls) != 2 {
		t.Fatalf("lotte's session must be retried, calls = %#v", lotteWriter.calls)
	}
	if len(warreWriter.calls) != 0 {
		t.Fatalf("another account's session must not be used: %#v", warreWriter.calls)
	}
}

func TestStateKeepsUploadedByAcrossReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "upload-state.json")
	state := NewState(path)
	state.Set("album-1", "a1", AssetState{
		GoogleMediaKey: "g1", Status: "uploaded", UploadedBy: "lotte@gmail.com",
	})
	if err := state.Save(); err != nil {
		t.Fatal(err)
	}
	loaded := NewState(path)
	if err := loaded.Load(); err != nil {
		t.Fatal(err)
	}
	asset, _ := loaded.Get("album-1", "a1")
	if asset.UploadedBy != "lotte@gmail.com" {
		t.Fatalf("uploadedBy was not persisted: %#v", asset)
	}
	pending := loaded.PendingAlbumAdds("album-1")
	if len(pending) != 1 || pending[0].UploadedBy != "lotte@gmail.com" {
		t.Fatalf("pending = %#v", pending)
	}
}
