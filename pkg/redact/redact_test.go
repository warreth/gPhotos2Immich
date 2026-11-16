package redact

import (
	"bytes"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

func useSecrets(t *testing.T, values ...string) {
	t.Helper()
	t.Setenv(EnvVar, "")
	SetSecrets(values)
	t.Cleanup(func() { SetSecrets(nil) })
}

func TestStringRedactsConfiguredSecrets(t *testing.T) {
	useSecrets(t, "immich-api-key-123", "ntfy-token-456")

	got := String("request failed with immich-api-key-123 and ntfy-token-456")
	if strings.Contains(got, "immich-api-key-123") || strings.Contains(got, "ntfy-token-456") {
		t.Fatalf("secret survived redaction: %q", got)
	}
	if count := strings.Count(got, Placeholder); count != 2 {
		t.Fatalf("placeholder count = %d, want 2: %q", count, got)
	}
}

func TestStringMasksLongestSecretFirst(t *testing.T) {
	useSecrets(t, "secret-key", "secret-key-longer")

	got := String("value secret-key-longer and secret-key")
	if strings.Contains(got, "secret-key") {
		t.Fatalf("secret survived redaction: %q", got)
	}
	if count := strings.Count(got, Placeholder); count != 2 {
		t.Fatalf("placeholder count = %d, want 2: %q", count, got)
	}
}

func TestStringMasksEscapedSecret(t *testing.T) {
	useSecrets(t, "a b/c")

	for _, value := range []string{"a b/c", "a+b%2Fc", "a%20b%2Fc"} {
		if got := String("value=" + value); strings.Contains(got, value) {
			t.Fatalf("escaped secret %q survived redaction: %q", value, got)
		}
	}
}

func TestStringIgnoresShortValues(t *testing.T) {
	useSecrets(t, "ab")

	if got := String("ab is too short to mask"); got != "ab is too short to mask" {
		t.Fatalf("short value was masked: %q", got)
	}
}

func TestStringRedactsURLSecrets(t *testing.T) {
	useSecrets(t)

	tests := []struct {
		name      string
		input     string
		forbidden []string
		want      []string
	}{
		{
			name:      "google share link",
			input:     `https://photos.google.com/share/AF1QipSecretAlbumKey?key=ShareKey123&x=1`,
			forbidden: []string{"AF1QipSecretAlbumKey", "ShareKey123"},
			want:      []string{"https://photos.google.com/share/" + Placeholder},
		},
		{
			name:      "shared path on any host",
			input:     `Get "http://127.0.0.1:1/share/TokenOnAnyHost?key=ShareKey": EOF`,
			forbidden: []string{"TokenOnAnyHost", "ShareKey"},
			want:      []string{"/share/" + Placeholder},
		},
		{
			name:      "private google album",
			input:     `https://photos.google.com/album/APrivateAlbumKey`,
			forbidden: []string{"APrivateAlbumKey"},
			want:      []string{"https://photos.google.com/album/" + Placeholder},
		},
		{
			name:      "short share link",
			input:     `Get "https://photos.app.goo.gl/ShortToken": EOF`,
			forbidden: []string{"ShortToken"},
			want:      []string{"https://photos.app.goo.gl/" + Placeholder},
		},
		{
			name:      "google CDN link",
			input:     `Get "https://lh3.googleusercontent.com/pw/AP1GczToken=w100-h100": EOF`,
			forbidden: []string{"AP1GczToken"},
			want:      []string{"https://lh3.googleusercontent.com/" + Placeholder},
		},
		{
			name:      "authkey parameter",
			input:     `https://example.com/api?authkey=AuthKey123&page=2`,
			forbidden: []string{"AuthKey123"},
			want:      []string{"authkey=" + Placeholder, "page=2"},
		},
		{
			name:      "token parameter",
			input:     `https://example.com/api?token=NtfyToken&topic=family`,
			forbidden: []string{"NtfyToken"},
			want:      []string{"token=" + Placeholder, "topic=family"},
		},
		{
			name:      "url userinfo",
			input:     `https://user:password@example.com/path`,
			forbidden: []string{"user:password"},
			want:      []string{Placeholder + "@example.com"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := String(tt.input)
			for _, value := range tt.forbidden {
				if strings.Contains(got, value) {
					t.Fatalf("sensitive value %q survived redaction: %q", value, got)
				}
			}
			for _, value := range tt.want {
				if !strings.Contains(got, value) {
					t.Fatalf("redacted output missing %q: %q", value, got)
				}
			}
		})
	}
}

func TestStringLeavesBenignURLAlone(t *testing.T) {
	useSecrets(t)

	const input = "https://example.com/api?page=2&sort=name"
	if got := String(input); got != input {
		t.Fatalf("benign URL changed: %q", got)
	}
}

func TestShowEnvDisablesRedaction(t *testing.T) {
	t.Setenv(EnvVar, "show")
	SetSecrets([]string{"super-secret-key"})
	t.Cleanup(func() { SetSecrets(nil) })

	input := "value super-secret-key and https://photos.google.com/share/AlbumSecret?key=ShareSecret"
	if got := String(input); got != input {
		t.Fatalf("LOG_SECRETS=show changed output: %q", got)
	}
}

func TestNonShowEnvKeepsRedaction(t *testing.T) {
	t.Setenv(EnvVar, "false")
	SetSecrets([]string{"super-secret-key"})
	t.Cleanup(func() { SetSecrets(nil) })

	if got := String("super-secret-key"); strings.Contains(got, "super-secret-key") {
		t.Fatalf("secret survived redaction with LOG_SECRETS=false: %q", got)
	}
}

func TestReplaceAttrRedactsStringsAndErrors(t *testing.T) {
	useSecrets(t, "super-secret-key")

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{ReplaceAttr: ReplaceAttr}))
	logger.Error(
		"failed with super-secret-key",
		"error", fmt.Errorf(`request to https://photos.google.com/share/AlbumSecret?key=super-secret-key`),
		"note", "super-secret-key",
	)

	out := buf.String()
	for _, value := range []string{"super-secret-key", "AlbumSecret"} {
		if strings.Contains(out, value) {
			t.Fatalf("sensitive value %q survived redaction: %s", value, out)
		}
	}
	if !strings.Contains(out, Placeholder) {
		t.Fatalf("redacted output missing placeholder: %s", out)
	}
}

func TestRef(t *testing.T) {
	first := Ref("https://photos.google.com/share/one?key=secret")
	second := Ref("https://photos.google.com/share/two?key=secret")
	if len(first) != 8 {
		t.Fatalf("Ref() length = %d, want 8", len(first))
	}
	if first != Ref("https://photos.google.com/share/one?key=secret") {
		t.Fatal("Ref() is not stable")
	}
	if first == second {
		t.Fatal("Ref() returned the same reference for different URLs")
	}
	if Ref("") != "" {
		t.Fatalf("Ref(\"\") = %q, want empty", Ref(""))
	}
}

func TestAlbumLabel(t *testing.T) {
	if got := AlbumLabel("Holiday", "https://photos.google.com/share/secret"); got != "Holiday" {
		t.Fatalf("AlbumLabel() with a name = %q, want Holiday", got)
	}
	got := AlbumLabel("", "https://photos.google.com/share/secret")
	if !strings.HasPrefix(got, "album ") || len(got) != len("album ")+8 {
		t.Fatalf("AlbumLabel() fallback = %q, want album <8 hex chars>", got)
	}
	if strings.Contains(got, "secret") {
		t.Fatalf("AlbumLabel() leaked the URL: %q", got)
	}
	if got := AlbumLabel("", ""); got != "album" {
		t.Fatalf("AlbumLabel() empty = %q, want album", got)
	}
}
