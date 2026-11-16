// Package redact removes secrets from log output. Secrets are registered by
// the application from the live configuration, and strings are also scanned
// for access-bearing URL parameters so a share key cannot leak through a
// wrapped net/http error.
package redact

import (
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
)

const (
	// EnvVar controls whether redaction is disabled. Set it to "show" to keep
	// full values in the logs while debugging.
	EnvVar = "LOG_SECRETS"
	// Placeholder replaces a detected secret.
	Placeholder = "[REDACTED]"
)

var (
	secretsMu sync.RWMutex
	secrets   []string

	// queryParamRe masks the values of query parameters that commonly carry
	// credentials: Google share keys, API keys, tokens and signatures.
	queryParamRe = regexp.MustCompile(`(?i)([?&](?:key|authkey|auth_key|apikey|api_key|access_token|token|password|passwd|secret|signature|sig)=)[^&\s"'<>]+`)
	// sharePathRe masks the album key in any /share/ URL path, regardless of
	// the host that served it.
	sharePathRe = regexp.MustCompile(`(?i)(/share/)[^\s"'<>/?#]+`)
	// albumPathRe masks a Google Photos private album URL.
	albumPathRe = regexp.MustCompile(`(?i)(https?://photos\.google\.com/album/)[^\s"'<>]+`)
	// shortShareRe masks the token in a photos.app.goo.gl short link.
	shortShareRe = regexp.MustCompile(`(?i)(https?://photos\.app\.goo\.gl/)[^\s"'<>]+`)
	// userContentRe masks signed Google CDN URLs, whose path is the credential.
	userContentRe = regexp.MustCompile(`(?i)(https?://[a-z0-9.-]*googleusercontent\.com/)[^\s"'<>]+`)
	// userinfoRe masks credentials embedded in a URL authority.
	userinfoRe = regexp.MustCompile(`(?i)([a-z][a-z0-9+.-]*://)[^/@\s"'<>]+@`)
)

// Show reports whether LOG_SECRETS explicitly asked for unredacted output.
func Show() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(EnvVar))) {
	case "show", "true", "1", "full", "unredacted":
		return true
	default:
		return false
	}
}

// SetSecrets replaces the values that are masked by exact match. Values shorter
// than three characters are ignored because replacing them would garble the
// logs without protecting a realistic credential. Longest values are matched
// first so a secret that contains another secret is masked as a whole.
func SetSecrets(values []string) {
	cleaned := make([]string, 0, len(values)*2)
	seen := make(map[string]bool, len(values)*2)
	add := func(value string) {
		value = strings.TrimSpace(value)
		if len(value) < 3 || seen[value] {
			return
		}
		seen[value] = true
		cleaned = append(cleaned, value)
	}
	for _, value := range values {
		add(value)
		// Net/http renders request URLs with escaped query values, so also
		// mask the escaped form of each secret.
		if escaped := url.QueryEscape(strings.TrimSpace(value)); escaped != value {
			add(escaped)
		}
		if escaped := url.PathEscape(strings.TrimSpace(value)); escaped != value {
			add(escaped)
		}
	}
	sort.SliceStable(cleaned, func(i, j int) bool {
		return len(cleaned[i]) > len(cleaned[j])
	})
	secretsMu.Lock()
	secrets = cleaned
	secretsMu.Unlock()
}

// String returns value with registered secrets and access-bearing URL
// parameters replaced. When LOG_SECRETS=show is set it returns value as-is.
func String(value string) string {
	if value == "" || Show() {
		return value
	}

	secretsMu.RLock()
	masked := value
	for _, secret := range secrets {
		if strings.Contains(masked, secret) {
			masked = strings.ReplaceAll(masked, secret, Placeholder)
		}
	}
	secretsMu.RUnlock()

	masked = queryParamRe.ReplaceAllString(masked, "${1}"+Placeholder)
	masked = sharePathRe.ReplaceAllString(masked, "${1}"+Placeholder)
	masked = albumPathRe.ReplaceAllString(masked, "${1}"+Placeholder)
	masked = shortShareRe.ReplaceAllString(masked, "${1}"+Placeholder)
	masked = userContentRe.ReplaceAllString(masked, "${1}"+Placeholder)
	return userinfoRe.ReplaceAllString(masked, "${1}"+Placeholder+"@")
}

// ReplaceAttr is a slog.HandlerOptions.ReplaceAttr function that redacts
// string attributes, the log message and error values.
func ReplaceAttr(groups []string, a slog.Attr) slog.Attr {
	if a.Value.Kind() == slog.KindString {
		return slog.String(a.Key, String(a.Value.String()))
	}
	if a.Value.Kind() == slog.KindAny {
		if err, ok := a.Value.Any().(error); ok && err != nil {
			return slog.String(a.Key, String(err.Error()))
		}
	}
	return a
}

// Ref derives a short, stable reference from a value. It is used in place of
// an album URL so log lines can still be correlated without exposing the URL.
func Ref(value string) string {
	if strings.TrimSpace(value) == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:4])
}

// AlbumLabel returns a log-safe album label: the configured title when one is
// available, otherwise an opaque reference derived from the URL.
func AlbumLabel(name, albumURL string) string {
	if name = strings.TrimSpace(name); name != "" {
		return name
	}
	if ref := Ref(albumURL); ref != "" {
		return "album " + ref
	}
	return "album"
}
