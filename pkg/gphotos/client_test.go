package gphotos

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fakeCLI(t *testing.T, body string) *CLIClient {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "gotohp-cli")
	script := "#!/bin/sh\n" + body + "\n"
	if err := os.WriteFile(path, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	return NewCLIClient(path, filepath.Join(dir, "gotohp.config"))
}

func TestAddAccount(t *testing.T) {
	client := fakeCLI(t, `cat >/dev/null
echo "✓ Account test@example.com connected and selected"`)

	email, err := client.AddAccount(context.Background(), "oauth-token")
	if err != nil {
		t.Fatalf("AddAccount failed: %v", err)
	}
	if email != "test@example.com" {
		t.Fatalf("email = %q", email)
	}
}

func TestListAccounts(t *testing.T) {
	client := fakeCLI(t, `echo "Credentials:"
echo "  * test@example.com"
echo "  second@example.com"
echo ""
echo "* = active"`)

	accounts, err := client.ListAccounts(context.Background())
	if err != nil {
		t.Fatalf("ListAccounts failed: %v", err)
	}
	if len(accounts) != 2 || accounts[0] != "test@example.com" || accounts[1] != "second@example.com" {
		t.Fatalf("accounts = %#v", accounts)
	}
}

func TestUploadBuildsArgsAndParsesSummary(t *testing.T) {
	argsFile := filepath.Join(t.TempDir(), "args.txt")
	client := fakeCLI(t, `printf '%s\n' "$@" > `+argsFile+`
cat <<'JSON'
{
  "total": 1,
  "succeeded": 1,
  "failed": 0,
  "skipped": 0,
  "results": [
    {
      "path": "/staging/a.jpg",
      "success": true,
      "mediaKey": "AF1QipMedia",
      "isLivePhoto": true
    }
  ],
  "album": {
    "name": "AF1QipAlbum",
    "itemsAdded": 1,
    "albumKeys": ["AF1QipAlbum"]
  }
}
JSON`)

	results, err := client.Upload(context.Background(), UploadRequest{
		StagingDir:     "/staging",
		AlbumKey:       "AF1QipAlbum",
		UseQuota:       true,
		PairLivePhotos: true,
		Threads:        2,
	})
	if err != nil {
		t.Fatalf("Upload failed: %v", err)
	}
	if len(results) != 1 || results[0].MediaKey != "AF1QipMedia" || !results[0].IsLivePhoto {
		t.Fatalf("results = %#v", results)
	}

	raw, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	args := string(raw)
	for _, want := range []string{
		"upload", "/staging",
		"--album", "AF1QipAlbum",
		"--use-quota",
		"--pair-live-photos",
		"--no-tui",
		"--threads", "2",
	} {
		if !strings.Contains(args, want) {
			t.Fatalf("args missing %q:\n%s", want, args)
		}
	}
}

func TestUploadQuotaOffByDefault(t *testing.T) {
	argsFile := filepath.Join(t.TempDir(), "args.txt")
	client := fakeCLI(t, `printf '%s\n' "$@" > `+argsFile+`
echo '{"total":0,"succeeded":0,"failed":0,"skipped":0,"results":[]}'`)

	if _, err := client.Upload(context.Background(), UploadRequest{StagingDir: "/staging"}); err != nil {
		t.Fatalf("Upload failed: %v", err)
	}
	raw, _ := os.ReadFile(argsFile)
	if strings.Contains(string(raw), "--use-quota") {
		t.Fatalf("--use-quota must be absent when UseQuota is false:\n%s", raw)
	}
}

func TestUploadReturnsAlbumError(t *testing.T) {
	client := fakeCLI(t, `echo '{"total":1,"succeeded":1,"failed":0,"skipped":0,"results":[{"path":"/a.jpg","success":true,"mediaKey":"m1"}],"album":{"error":"permission denied"}}'`)

	results, err := client.Upload(context.Background(), UploadRequest{StagingDir: "/staging"})
	if err == nil {
		t.Fatal("expected an album error")
	}
	if len(results) != 1 || results[0].MediaKey != "m1" {
		t.Fatalf("results = %#v", results)
	}
	if !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("error = %v", err)
	}
}

func TestUploadRejectsMissingSummary(t *testing.T) {
	client := fakeCLI(t, `echo "boom" >&2
exit 1`)

	if _, err := client.Upload(context.Background(), UploadRequest{StagingDir: "/staging"}); err == nil {
		t.Fatal("expected an error for a missing JSON summary")
	}
}

func TestVersion(t *testing.T) {
	client := fakeCLI(t, `echo "gotohp v0.11.0"`)
	version, err := client.Version(context.Background())
	if err != nil {
		t.Fatalf("Version failed: %v", err)
	}
	if version != "v0.11.0" {
		t.Fatalf("version = %q", version)
	}
}

func TestUploadPassesAccountFlag(t *testing.T) {
	argsFile := filepath.Join(t.TempDir(), "args.txt")
	client := fakeCLI(t, `printf '%s\n' "$@" > `+argsFile+`
echo '{"total":0,"succeeded":0,"failed":0,"skipped":0,"results":[]}'`)

	if _, err := client.Upload(context.Background(), UploadRequest{
		StagingDir: "/staging",
		Account:    " lotte@gmail.com ",
	}); err != nil {
		t.Fatalf("Upload failed: %v", err)
	}

	raw, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	args := string(raw)
	if !strings.Contains(args, "--account\nlotte@gmail.com") {
		t.Fatalf("expected --account lotte@gmail.com in:\n%s", args)
	}
}

func TestUploadOmitsAccountWhenEmpty(t *testing.T) {
	argsFile := filepath.Join(t.TempDir(), "args.txt")
	client := fakeCLI(t, `printf '%s\n' "$@" > `+argsFile+`
echo '{"total":0,"succeeded":0,"failed":0,"skipped":0,"results":[]}'`)

	if _, err := client.Upload(context.Background(), UploadRequest{StagingDir: "/staging"}); err != nil {
		t.Fatalf("Upload failed: %v", err)
	}
	raw, _ := os.ReadFile(argsFile)
	if strings.Contains(string(raw), "--account") {
		t.Fatalf("--account must be absent when Account is empty:\n%s", raw)
	}
}

func TestListAccountsDetailedMarksActive(t *testing.T) {
	client := fakeCLI(t, `echo "Credentials:"
echo "  * warre@gmail.com"
echo "  lotte@gmail.com"`)

	accounts, err := client.ListAccountsDetailed(context.Background())
	if err != nil {
		t.Fatalf("ListAccountsDetailed failed: %v", err)
	}
	if len(accounts) != 2 {
		t.Fatalf("accounts = %#v", accounts)
	}
	if accounts[0].Email != "warre@gmail.com" || !accounts[0].Active {
		t.Fatalf("first account = %#v", accounts[0])
	}
	if accounts[1].Email != "lotte@gmail.com" || accounts[1].Active {
		t.Fatalf("second account = %#v", accounts[1])
	}
}

func TestSetActiveAccount(t *testing.T) {
	argsFile := filepath.Join(t.TempDir(), "args.txt")
	client := fakeCLI(t, `printf '%s\n' "$@" > `+argsFile)

	if err := client.SetActiveAccount(context.Background(), "lotte@gmail.com"); err != nil {
		t.Fatalf("SetActiveAccount failed: %v", err)
	}
	raw, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	args := string(raw)
	for _, want := range []string{"creds", "set", "lotte@gmail.com", "--config"} {
		if !strings.Contains(args, want) {
			t.Fatalf("args missing %q:\n%s", want, args)
		}
	}
}

func TestSetActiveAccountRejectsEmptyEmail(t *testing.T) {
	client := fakeCLI(t, `exit 0`)
	if err := client.SetActiveAccount(context.Background(), "  "); err == nil {
		t.Fatal("expected an error for an empty email")
	}
}
