package web

import (
	"testing"

	"warreth.dev/gphotos2immich/pkg/config"
)

func TestMergeGoogleUploadPreservesNonUIFields(t *testing.T) {
	existing := map[string]interface{}{
		"webCookiesFile": "/data/cookies.txt",
		"gotohpBinary":   "/app/gotohp-cli",
		"maxRetries":     float64(5),
		"autoUpdate":     false,
	}
	merged := mergeGoogleUpload(existing, config.GoogleUploadConfig{
		StorageMode:           config.StorageModeQuota,
		Quality:               config.QualityOriginal,
		SkipLivePhotos:        true,
		ReuploadDeleted:       true,
		Threads:               2,
		StrictAccountMatching: true,
	})

	if merged["webCookiesFile"] != "/data/cookies.txt" {
		t.Fatalf("webCookiesFile = %#v", merged["webCookiesFile"])
	}
	if merged["gotohpBinary"] != "/app/gotohp-cli" {
		t.Fatalf("gotohpBinary = %#v", merged["gotohpBinary"])
	}
	if merged["maxRetries"] != float64(5) {
		t.Fatalf("maxRetries = %#v", merged["maxRetries"])
	}
	if merged["reuploadDeleted"] != true {
		t.Fatalf("reuploadDeleted = %#v", merged["reuploadDeleted"])
	}
	if merged["enabled"] != true {
		t.Fatalf("enabled = %#v", merged["enabled"])
	}
	if merged["strictAccountMatching"] != true {
		t.Fatalf("strictAccountMatching = %#v", merged["strictAccountMatching"])
	}
}

func TestMergeNotificationsPreservesNestedSettings(t *testing.T) {
	existing := map[string]interface{}{
		"immich":          true,
		"immichAccountId": "acc-1",
		"onSyncFailed":    true,
		"ntfy": map[string]interface{}{
			"enabled": true,
			"url":     "https://ntfy.sh",
			"topic":   "family",
		},
	}
	incoming := config.NotificationsConfig{
		Immich:       false,
		OnSyncFailed: false,
	}
	raw := []byte(`{"immich":false,"onSyncFailed":false}`)
	merged := mergeNotifications(existing, incoming, raw)

	if merged["immich"] != false || merged["onSyncFailed"] != false {
		t.Fatalf("form fields were not applied: %#v", merged)
	}
	if merged["immichAccountId"] != "acc-1" {
		t.Fatalf("immichAccountId = %#v", merged["immichAccountId"])
	}
	if _, ok := merged["ntfy"]; !ok {
		t.Fatal("an omitted ntfy block must be preserved")
	}

	// A submitted ntfy block replaces the stored one.
	incoming.Ntfy = config.NtfyConfig{Enabled: true, URL: "https://ntfy.example", Topic: "new"}
	merged = mergeNotifications(existing, incoming, []byte(`{"ntfy":{"enabled":true}}`))
	ntfy, ok := merged["ntfy"].(config.NtfyConfig)
	if !ok || ntfy.URL != "https://ntfy.example" || ntfy.Topic != "new" {
		t.Fatalf("ntfy = %#v", merged["ntfy"])
	}
}
