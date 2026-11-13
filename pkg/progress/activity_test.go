package progress

import (
	"errors"
	"testing"
	"time"
)

func resetActivity(t *testing.T) {
	t.Helper()
	activity.mu.Lock()
	activity.snap = Snapshot{}
	activity.events = nil
	activity.nextID = 0
	activity.subs = make(map[chan struct{}]struct{})
	activity.mu.Unlock()
}

func TestActivityImportCycle(t *testing.T) {
	resetActivity(t)
	BeginCycle(2)
	StartAlbum("Album A", 3)
	RecordImport("Album A", true, false, false)
	RecordImport("Album A", false, true, false)
	RecordImport("Album A", false, false, true)
	FinishAlbum("Album A", 1, 1, 1)
	FinishCycle(1, 1, 1)

	snap := CurrentSnapshot()
	if snap.Running {
		t.Fatal("cycle must be finished")
	}
	if snap.AlbumsTotal != 2 || snap.AlbumsDone != 1 {
		t.Fatalf("album counts = %#v", snap)
	}
	if snap.Added != 1 || snap.Skipped != 1 || snap.Failed != 1 {
		t.Fatalf("item counts = %#v", snap)
	}
	if len(snap.ActiveAlbums) != 0 {
		t.Fatalf("active albums = %#v", snap.ActiveAlbums)
	}
	if len(snap.Events) < 4 {
		t.Fatalf("events = %#v", snap.Events)
	}
}

func TestActivityUploadProgress(t *testing.T) {
	resetActivity(t)
	BeginUpload("https://photos.app.goo.gl/x", 4)
	SetUploadTotal(4)
	RecordUpload(true, false)
	RecordUpload(true, false)
	RecordUpload(false, true)
	FinishUpload("https://photos.app.goo.gl/x", 2, 1)

	snap := CurrentSnapshot()
	if snap.UploadRunning {
		t.Fatal("upload must be finished")
	}
	if snap.UploadTotal != 4 || snap.UploadProcessed != 4 {
		t.Fatalf("upload progress = %#v", snap)
	}
	if snap.Uploaded != 2 || snap.UploadFailed != 1 {
		t.Fatalf("upload result = %#v", snap)
	}
}

func TestActivityFailAlbum(t *testing.T) {
	resetActivity(t)
	BeginCycle(1)
	FailAlbum("Broken", errors.New("scrape failed"))
	snap := CurrentSnapshot()
	if snap.AlbumsDone != 1 {
		t.Fatalf("albums done = %d", snap.AlbumsDone)
	}
	if len(snap.Events) == 0 || snap.Events[len(snap.Events)-1].Level != "error" {
		t.Fatalf("events = %#v", snap.Events)
	}
}

func TestActivitySubscribe(t *testing.T) {
	resetActivity(t)
	ch, cancel := Subscribe()
	defer cancel()
	AddEvent("info", "system", "hello", nil)
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("subscriber was not notified")
	}
}

func TestActivitySnapshotIsCopy(t *testing.T) {
	resetActivity(t)
	BeginCycle(1)
	StartAlbum("A", 2)
	snap := CurrentSnapshot()
	snap.ActiveAlbums[0].Name = "mutated"
	snap.Events = append(snap.Events, Event{Message: "injected"})
	again := CurrentSnapshot()
	if again.ActiveAlbums[0].Name != "A" {
		t.Fatal("snapshot mutated the active album")
	}
	if len(again.Events) != len(snap.Events)-1 {
		t.Fatal("snapshot mutated the event store")
	}
}
