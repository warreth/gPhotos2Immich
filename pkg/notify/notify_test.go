package notify

import (
	"context"
	"errors"
	"testing"

	"warreth.dev/gphotos2immich/pkg/config"
	"warreth.dev/gphotos2immich/pkg/immich"
)

type fakeSender struct {
	calls []immich.Notification
	err   error
}

func (f *fakeSender) CreateNotification(_ context.Context, notification immich.Notification) error {
	f.calls = append(f.calls, notification)
	return f.err
}

func enabledConfig() config.NotificationsConfig {
	return config.NotificationsConfig{
		Immich:           true,
		OnSyncFailed:     true,
		OnImportComplete: false,
		OnUploadComplete: true,
	}
}

func TestSendEnabledEvent(t *testing.T) {
	sender := &fakeSender{}
	service := New(sender, enabledConfig(), nil)
	service.SetUserID("user-1")

	service.Send(context.Background(), EventUploadComplete, "success", "Upload complete", "3 photos added", map[string]any{"album": "Leiding"})
	if len(sender.calls) != 1 {
		t.Fatalf("calls = %#v", sender.calls)
	}
	call := sender.calls[0]
	if call.UserID != "user-1" || call.Level != "success" || call.Title != "Upload complete" {
		t.Fatalf("call = %#v", call)
	}
	if call.Data["album"] != "Leiding" {
		t.Fatalf("data = %#v", call.Data)
	}
}

func TestSendSkipsDisabledEvents(t *testing.T) {
	sender := &fakeSender{}
	service := New(sender, enabledConfig(), nil)
	service.SetUserID("user-1")

	service.Send(context.Background(), EventImportComplete, "success", "Import complete", "", nil)
	if len(sender.calls) != 0 {
		t.Fatalf("disabled event was sent: %#v", sender.calls)
	}
}

func TestSendRequiresUserID(t *testing.T) {
	sender := &fakeSender{}
	service := New(sender, enabledConfig(), nil)

	service.Send(context.Background(), EventSyncFailed, "error", "Sync failed", "", nil)
	if len(sender.calls) != 0 {
		t.Fatalf("notification without a user id was sent: %#v", sender.calls)
	}
}

func TestFailedSendDoesNotPanic(t *testing.T) {
	sender := &fakeSender{err: errors.New("403 missing permission")}
	service := New(sender, enabledConfig(), nil)
	service.SetUserID("user-1")

	service.Send(context.Background(), EventSyncFailed, "error", "Sync failed", "album failed", nil)
	if len(sender.calls) != 1 {
		t.Fatalf("calls = %#v", sender.calls)
	}
}

func TestCancelledContextStillSends(t *testing.T) {
	sender := &fakeSender{}
	service := New(sender, enabledConfig(), nil)
	service.SetUserID("user-1")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	service.Send(ctx, EventSyncFailed, "error", "Sync failed", "", nil)
	if len(sender.calls) != 1 {
		t.Fatalf("calls = %#v", sender.calls)
	}
}
