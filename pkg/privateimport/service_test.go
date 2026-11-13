package privateimport

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"warreth.dev/gphotos2immich/pkg/gphotosweb"
	"warreth.dev/gphotos2immich/pkg/immich"
)

type fakeWeb struct {
	albums      []gphotosweb.Album
	items       []gphotosweb.MediaItem
	infos       map[string]gphotosweb.MediaInfo
	downloads   map[string][]byte
	downloadErr map[string]error
	block       chan struct{}
}

func (f *fakeWeb) ListAlbums(context.Context) ([]gphotosweb.Album, error) {
	return f.albums, nil
}

func (f *fakeWeb) ListAlbumItems(context.Context, string) ([]gphotosweb.MediaItem, error) {
	return f.items, nil
}

func (f *fakeWeb) DownloadMediaToFile(ctx context.Context, mediaKey, destPath string) (gphotosweb.MediaInfo, error) {
	if f.block != nil {
		select {
		case <-ctx.Done():
			return gphotosweb.MediaInfo{}, ctx.Err()
		case <-f.block:
		}
	}
	if err := f.downloadErr[mediaKey]; err != nil {
		return gphotosweb.MediaInfo{}, err
	}
	if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
		return gphotosweb.MediaInfo{}, err
	}
	if err := os.WriteFile(destPath, f.downloads[mediaKey], 0600); err != nil {
		return gphotosweb.MediaInfo{}, err
	}
	return f.infos[mediaKey], nil
}

type fakeImmich struct {
	createdName string
	albumID     string
	uploads     map[string][]byte
	uploadErr   error
	addErr      error
	added       []string
}

func (f *fakeImmich) CreateAlbum(_ context.Context, name string) (*immich.Album, error) {
	f.createdName = name
	return &immich.Album{Id: "new-album", AlbumName: name}, nil
}

func (f *fakeImmich) AddAssetsToAlbum(_ context.Context, albumID string, assetIDs []string) error {
	if f.addErr != nil {
		return f.addErr
	}
	f.added = append(f.added, assetIDs...)
	return nil
}

func (f *fakeImmich) UploadAssetFromFile(_ context.Context, path, filename string, _ time.Time, _ string) (string, bool, error) {
	if f.uploadErr != nil {
		return "", false, f.uploadErr
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false, err
	}
	if f.uploads == nil {
		f.uploads = make(map[string][]byte)
	}
	f.uploads[filename] = data
	return "asset-" + filename, false, nil
}

type fakeState struct {
	values map[string]string
	saved  int
}

func (f *fakeState) Get(key string) (string, bool) {
	value, ok := f.values[key]
	return value, ok
}

func (f *fakeState) Set(key, assetID string) {
	if f.values == nil {
		f.values = make(map[string]string)
	}
	f.values[key] = assetID
}

func (f *fakeState) Save() error {
	f.saved++
	return nil
}

func testItems() ([]gphotosweb.MediaItem, map[string]gphotosweb.MediaInfo, map[string][]byte) {
	items := []gphotosweb.MediaItem{
		{Key: "m1", TakenAt: time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)},
		{Key: "m2", TakenAt: time.Date(2024, 2, 2, 12, 0, 0, 0, time.UTC)},
	}
	infos := map[string]gphotosweb.MediaInfo{
		"m1": {Key: "m1", Filename: "one.jpg", TakenAt: items[0].TakenAt},
		"m2": {Key: "m2", Filename: "two.jpg", TakenAt: items[1].TakenAt},
	}
	downloads := map[string][]byte{"m1": []byte("one"), "m2": []byte("two")}
	return items, infos, downloads
}

func TestListAlbumsFiltersShared(t *testing.T) {
	service := &Service{Web: &fakeWeb{albums: []gphotosweb.Album{
		{Key: "a", Title: "Private", Shared: false},
		{Key: "b", Title: "Shared", Shared: true},
	}}}
	albums, err := service.ListAlbums(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(albums) != 1 || albums[0].Key != "a" {
		t.Fatalf("albums = %#v", albums)
	}
}

func TestImportSkipsKnownItems(t *testing.T) {
	items, infos, downloads := testItems()
	state := &fakeState{values: map[string]string{"gp_private_m1": "existing"}}
	immich := &fakeImmich{}
	service := &Service{
		Web:         &fakeWeb{items: items, infos: infos, downloads: downloads},
		Immich:      immich,
		State:       state,
		StagingRoot: t.TempDir(),
	}
	summary, err := service.Import(context.Background(), Request{AlbumKey: "album", AlbumTitle: "Private", ImmichAlbumID: "target"}, nil)
	if err != nil {
		t.Fatalf("Import failed: %v", err)
	}
	if summary.Total != 2 || summary.Imported != 1 || summary.Skipped != 1 || summary.Failed != 0 {
		t.Fatalf("summary = %#v", summary)
	}
	if len(immich.uploads) != 1 || len(immich.added) != 1 || immich.added[0] != "asset-two.jpg" {
		t.Fatalf("uploads = %#v added = %#v", immich.uploads, immich.added)
	}
	if state.values["gp_private_m2"] != "asset-two.jpg" {
		t.Fatalf("state = %#v", state.values)
	}
	if state.saved != 1 {
		t.Fatalf("state saves = %d", state.saved)
	}
}

func TestImportCreatesAlbum(t *testing.T) {
	items, infos, downloads := testItems()
	immich := &fakeImmich{}
	service := &Service{
		Web:         &fakeWeb{items: items[:1], infos: infos, downloads: downloads},
		Immich:      immich,
		State:       &fakeState{},
		StagingRoot: t.TempDir(),
	}
	summary, err := service.Import(context.Background(), Request{
		AlbumKey: "album", AlbumTitle: "Holiday", ImmichAlbumName: "My Holiday",
	}, nil)
	if err != nil {
		t.Fatalf("Import failed: %v", err)
	}
	if immich.createdName != "My Holiday" {
		t.Fatalf("created album = %q", immich.createdName)
	}
	if summary.Imported != 1 {
		t.Fatalf("summary = %#v", summary)
	}
}

func TestImportDownloadFailure(t *testing.T) {
	items, infos, downloads := testItems()
	immich := &fakeImmich{}
	service := &Service{
		Web: &fakeWeb{
			items: items[:1], infos: infos, downloads: downloads,
			downloadErr: map[string]error{"m1": errors.New("network down")},
		},
		Immich: immich, State: &fakeState{}, StagingRoot: t.TempDir(),
	}
	summary, err := service.Import(context.Background(), Request{AlbumKey: "a", AlbumTitle: "A", ImmichAlbumID: "t"}, nil)
	if err != nil {
		t.Fatalf("Import failed: %v", err)
	}
	if summary.Failed != 1 || summary.Imported != 0 {
		t.Fatalf("summary = %#v", summary)
	}
	if len(immich.added) != 0 {
		t.Fatalf("added = %#v", immich.added)
	}
}

func TestImportAlbumAddFailureKeepsStateEmpty(t *testing.T) {
	items, infos, downloads := testItems()
	state := &fakeState{}
	service := &Service{
		Web:         &fakeWeb{items: items[:1], infos: infos, downloads: downloads},
		Immich:      &fakeImmich{addErr: errors.New("album add failed")},
		State:       state,
		StagingRoot: t.TempDir(),
	}
	summary, err := service.Import(context.Background(), Request{AlbumKey: "a", AlbumTitle: "A", ImmichAlbumID: "t"}, nil)
	if err != nil {
		t.Fatalf("Import failed: %v", err)
	}
	if summary.Failed != 1 || summary.Imported != 0 {
		t.Fatalf("summary = %#v", summary)
	}
	if len(state.values) != 0 {
		t.Fatalf("state must stay empty so the add is retried: %#v", state.values)
	}
}

func TestImportCanceled(t *testing.T) {
	items, infos, downloads := testItems()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	service := &Service{
		Web:         &fakeWeb{items: items, infos: infos, downloads: downloads},
		Immich:      &fakeImmich{},
		State:       &fakeState{},
		StagingRoot: t.TempDir(),
	}
	if _, err := service.Import(ctx, Request{AlbumKey: "a", AlbumTitle: "A", ImmichAlbumID: "t"}, nil); err == nil {
		t.Fatal("expected a cancellation error")
	}
}

func TestManagerRunsAndReports(t *testing.T) {
	items, infos, downloads := testItems()
	manager := NewManager(&Service{
		Web:         &fakeWeb{items: items[:1], infos: infos, downloads: downloads},
		Immich:      &fakeImmich{},
		State:       &fakeState{},
		StagingRoot: t.TempDir(),
	})
	status, err := manager.Start(context.Background(), Request{AlbumKey: "a", AlbumTitle: "A", ImmichAlbumID: "t"})
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	if !status.Running {
		t.Fatalf("status = %#v", status)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		status = manager.Status()
		if !status.Running {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if status.Running || status.Imported != 1 || status.Total != 1 || status.Error != "" {
		t.Fatalf("final status = %#v", status)
	}
}

func TestManagerCancel(t *testing.T) {
	items, infos, downloads := testItems()
	manager := NewManager(&Service{
		Web:         &fakeWeb{items: items, infos: infos, downloads: downloads, block: make(chan struct{})},
		Immich:      &fakeImmich{},
		State:       &fakeState{},
		StagingRoot: t.TempDir(),
	})
	if _, err := manager.Start(context.Background(), Request{AlbumKey: "a", AlbumTitle: "A", ImmichAlbumID: "t"}); err != nil {
		t.Fatal(err)
	}
	manager.Cancel()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		status := manager.Status()
		if !status.Running {
			if status.Error != "Import canceled." {
				t.Fatalf("status = %#v", status)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("import did not stop after cancel")
}
