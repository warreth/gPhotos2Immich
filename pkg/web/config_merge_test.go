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
		StorageMode:     config.StorageModeQuota,
		Quality:         config.QualityOriginal,
		SkipLivePhotos:  true,
		ReuploadDeleted: true,
		Threads:         2,
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
}
