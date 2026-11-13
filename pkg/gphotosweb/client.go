// Package gphotosweb talks to the private Google Photos web API with a
// signed-in browser session.
//
// It exists for one reason: the mobile API used by gotohp can upload to the
// library and add items to albums you own, but it refuses albums that are
// shared with you. The web app can add to those, so this package provides the
// narrow set of calls the sync-back feature needs. It deliberately knows
// nothing about Immich, the public album scraper, or the upload pipeline.
package gphotosweb

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultBaseURL = "https://photos.google.com"
	defaultPrefix  = "/_/PhotosUi"

	// ErrNotConfigured means no cookies file has been saved yet.
	ErrNotConfigured = "Google Photos browser session is not configured"
	// ErrSessionExpired means the cookies are missing, stale, or signed out.
	ErrSessionExpired = "Google Photos browser session is expired; export the cookies again"
)

// ErrNoSession is the typed form of ErrSessionExpired.
var ErrNoSession = errors.New(ErrSessionExpired)

// Album is one album in the signed-in account's album list.
type Album struct {
	Key      string `json:"key"`
	Title    string `json:"title"`
	Count    int    `json:"count"`
	Shared   bool   `json:"shared"`
	ShareURL string `json:"shareUrl,omitempty"`
}

// Session is the short-lived state read from the photos.google.com page.
type Session struct {
	At        string
	FSID      string
	BL        string
	RPCPrefix string
}

// Client is safe for concurrent use. It reloads the cookie jar only when
// Reload is called, so a replaced file does not race with an in-flight call.
type Client struct {
	CookiesPath string
	BaseURL     string

	mu        sync.Mutex
	http      *http.Client
	session   *Session
	sessionAt time.Time
}

// NewClient creates a client for a Netscape cookies.txt file. The file may not
// exist yet; calls then return ErrNotConfigured.
func NewClient(cookiesPath string) *Client {
	return &Client{CookiesPath: strings.TrimSpace(cookiesPath)}
}

// Available reports whether a non-empty cookies file exists.
func (c *Client) Available() bool {
	if c == nil || strings.TrimSpace(c.CookiesPath) == "" {
		return false
	}
	info, err := os.Stat(c.CookiesPath)
	return err == nil && info.Size() > 0
}

// Reload drops the cached session and rebuilds the cookie jar from disk.
// Call it after replacing the cookies file.
func (c *Client) Reload() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.session = nil
	c.sessionAt = time.Time{}
	c.http = nil
}

func (c *Client) baseURL() string {
	if c != nil && strings.TrimSpace(c.BaseURL) != "" {
		return strings.TrimRight(strings.TrimSpace(c.BaseURL), "/")
	}
	return defaultBaseURL
}

func (c *Client) httpClient() (*http.Client, error) {
	if c.http != nil {
		return c.http, nil
	}
	if !c.Available() {
		return nil, errors.New(ErrNotConfigured)
	}
	data, err := os.ReadFile(c.CookiesPath)
	if err != nil {
		return nil, fmt.Errorf("read Google Photos cookies: %w", err)
	}
	jar, err := jarFromCookies(data)
	if err != nil {
		return nil, err
	}
	c.http = &http.Client{
		Jar:     jar,
		Timeout: 2 * time.Minute,
	}
	return c.http, nil
}

// Validate fetches the signed-in Photos page and returns a fresh session.
// It bypasses the 30-minute token cache so the UI's "Test session" action
// reports a stale cookie instead of a remembered token.
func (c *Client) Validate(ctx context.Context) (Session, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.session = nil
	c.sessionAt = time.Time{}
	return c.ensureSession(ctx)
}

// ValidateCookies checks a candidate cookies.txt without saving it.
func (c *Client) ValidateCookies(ctx context.Context, cookies []byte) (Session, error) {
	jar, err := jarFromCookies(cookies)
	if err != nil {
		return Session{}, err
	}
	probe := &Client{
		BaseURL: c.baseURL(),
		http: &http.Client{
			Jar:     jar,
			Timeout: 2 * time.Minute,
		},
	}
	return probe.Validate(ctx)
}

func (c *Client) ensureSession(ctx context.Context) (Session, error) {
	if c.session != nil && time.Since(c.sessionAt) < 30*time.Minute {
		return *c.session, nil
	}
	httpClient, err := c.httpClient()
	if err != nil {
		return Session{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL()+"/", nil)
	if err != nil {
		return Session{}, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")

	resp, err := httpClient.Do(req)
	if err != nil {
		return Session{}, fmt.Errorf("fetch Google Photos page: %w", err)
	}
	defer resp.Body.Close()

	if resp.Request != nil && resp.Request.URL != nil && resp.Request.URL.Host == "accounts.google.com" {
		return Session{}, ErrNoSession
	}
	if resp.StatusCode != http.StatusOK {
		return Session{}, fmt.Errorf("Google Photos page returned %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return Session{}, fmt.Errorf("read Google Photos page: %w", err)
	}
	session, err := parseSession(body)
	if err != nil {
		return Session{}, err
	}
	c.session = &session
	c.sessionAt = time.Now()
	return session, nil
}

func parseSession(body []byte) (Session, error) {
	at := firstJSONString(body, "SNlM0e")
	fsid := firstJSONString(body, "FdrFJe")
	bl := firstJSONString(body, "cfb2h")
	prefix := firstJSONString(body, "Im6cmf")
	if at == "" || fsid == "" || bl == "" {
		return Session{}, ErrNoSession
	}
	prefix = strings.ReplaceAll(prefix, `\/`, `/`)
	if prefix == "" {
		prefix = defaultPrefix
	}
	return Session{At: at, FSID: fsid, BL: bl, RPCPrefix: prefix}, nil
}

// firstJSONString finds "key":"value" in a page without parsing the enormous
// WIZ_global_data object.
func firstJSONString(body []byte, key string) string {
	needle := []byte(`"` + key + `":"`)
	idx := bytes.Index(body, needle)
	if idx == -1 {
		return ""
	}
	rest := body[idx+len(needle):]
	end := bytes.IndexByte(rest, '"')
	if end == -1 {
		return ""
	}
	return string(rest[:end])
}

// ListAlbums returns every album, including albums shared with the account.
func (c *Client) ListAlbums(ctx context.Context) ([]Album, error) {
	var albums []Album
	var page any
	for pageNum := 0; pageNum < 50; pageNum++ {
		data, err := c.rpc(ctx, "Z5xsfc", []any{page, nil, nil, nil, 1, nil, nil, 100, []int{2}, 5})
		if err != nil {
			return nil, err
		}
		var response []json.RawMessage
		if err := json.Unmarshal(data, &response); err != nil {
			return nil, fmt.Errorf("parse album list: %w", err)
		}
		if len(response) == 0 {
			break
		}
		var rawAlbums []json.RawMessage
		if err := json.Unmarshal(response[0], &rawAlbums); err != nil {
			return nil, fmt.Errorf("parse album list items: %w", err)
		}
		for _, raw := range rawAlbums {
			if album, ok := parseAlbum(raw); ok {
				albums = append(albums, album)
			}
		}
		if len(response) < 2 || string(response[1]) == "null" {
			break
		}
		var next string
		if err := json.Unmarshal(response[1], &next); err != nil || next == "" {
			break
		}
		page = next
	}
	return albums, nil
}

// parseAlbum reads the parts of the Z5xsfc album record the UI needs. The
// record is a sparse positional array whose trailing metadata lives in an
// extension object keyed "72930366".
func parseAlbum(raw json.RawMessage) (Album, bool) {
	var fields []json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || len(fields) == 0 {
		return Album{}, false
	}
	var key string
	if err := json.Unmarshal(fields[0], &key); err != nil || key == "" {
		return Album{}, false
	}
	album := Album{Key: key}
	for i := len(fields) - 1; i >= 0; i-- {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(fields[i], &obj); err != nil {
			continue
		}
		extRaw, ok := obj["72930366"]
		if !ok {
			continue
		}
		var ext []json.RawMessage
		if err := json.Unmarshal(extRaw, &ext); err != nil {
			continue
		}
		album.Title = rawString(ext, 1)
		album.Count = rawInt(ext, 3)
		album.Shared = rawBool(ext, 4)
		album.ShareURL = rawString(ext, 10)
		break
	}
	if album.Title == "" {
		album.Title = key
	}
	return album, true
}

func rawString(fields []json.RawMessage, idx int) string {
	if idx < 0 || idx >= len(fields) {
		return ""
	}
	var value string
	_ = json.Unmarshal(fields[idx], &value)
	return value
}

func rawInt(fields []json.RawMessage, idx int) int {
	if idx < 0 || idx >= len(fields) {
		return 0
	}
	var value int
	_ = json.Unmarshal(fields[idx], &value)
	return value
}

func rawBool(fields []json.RawMessage, idx int) bool {
	if idx < 0 || idx >= len(fields) {
		return false
	}
	var value bool
	_ = json.Unmarshal(fields[idx], &value)
	return value
}

// AddItems adds library media keys to an album. Shared albums use the web
// app's laUYf call; albums you own use E1Cajb.
func (c *Client) AddItems(ctx context.Context, albumKey string, shared bool, mediaKeys []string) error {
	albumKey = strings.TrimSpace(albumKey)
	if albumKey == "" {
		return fmt.Errorf("album key is required")
	}
	keys := make([]string, 0, len(mediaKeys))
	seen := make(map[string]bool, len(mediaKeys))
	for _, key := range mediaKeys {
		key = strings.TrimSpace(key)
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		keys = append(keys, key)
	}
	if len(keys) == 0 {
		return nil
	}
	const chunkSize = 50
	for start := 0; start < len(keys); start += chunkSize {
		end := start + chunkSize
		if end > len(keys) {
			end = len(keys)
		}
		chunk := keys[start:end]
		if shared {
			nested := make([]any, len(chunk))
			for i, key := range chunk {
				nested[i] = []any{[]string{key}}
			}
			if _, err := c.rpc(ctx, "laUYf", []any{
				albumKey,
				[]any{2, nil, nested, nil, nil, nil, []int{1}},
			}); err != nil {
				return err
			}
			continue
		}
		if _, err := c.rpc(ctx, "E1Cajb", []any{chunk, albumKey}); err != nil {
			return err
		}
	}
	return nil
}

// rpc sends one batchexecute request, refreshing the session once when Google
// reports that it is stale.
func (c *Client) rpc(ctx context.Context, rpcID string, payload any) (json.RawMessage, error) {
	data, err := c.rpcOnce(ctx, rpcID, payload)
	if err == nil {
		return data, nil
	}
	if !errors.Is(err, ErrNoSession) {
		return nil, err
	}
	c.mu.Lock()
	c.session = nil
	c.sessionAt = time.Time{}
	c.mu.Unlock()
	return c.rpcOnce(ctx, rpcID, payload)
}

func (c *Client) rpcOnce(ctx context.Context, rpcID string, payload any) (json.RawMessage, error) {
	c.mu.Lock()
	session, err := c.ensureSession(ctx)
	httpClient := c.http
	c.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if httpClient == nil {
		return nil, errors.New(ErrNotConfigured)
	}

	inner, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encode %s payload: %w", rpcID, err)
	}
	freq, err := json.Marshal([]any{[]any{[]any{rpcID, string(inner), nil, "generic"}}})
	if err != nil {
		return nil, fmt.Errorf("encode %s request: %w", rpcID, err)
	}
	form := url.Values{}
	form.Set("f.req", string(freq))
	form.Set("at", session.At)

	endpoint := fmt.Sprintf("%s%s/data/batchexecute?rpcids=%s&source-path=%%2F&f.sid=%s&bl=%s&rt=c",
		c.baseURL(), session.RPCPrefix, url.QueryEscape(rpcID), url.QueryEscape(session.FSID), url.QueryEscape(session.BL))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded;charset=UTF-8")
	req.Header.Set("X-Same-Domain", "1")
	req.Header.Set("Origin", c.baseURL())
	req.Header.Set("Referer", c.baseURL()+"/")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("Google Photos RPC %s: %w", rpcID, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, ErrNoSession
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Google Photos RPC %s returned %s", rpcID, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, fmt.Errorf("read Google Photos RPC %s: %w", rpcID, err)
	}
	return parseRPCResponse(body, rpcID)
}

func parseRPCResponse(body []byte, rpcID string) (json.RawMessage, error) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("Google Photos RPC %s returned an empty response", rpcID)
	}
	if trimmed[0] == '<' {
		return nil, ErrNoSession
	}
	scanner := bufio.NewScanner(bytes.NewReader(trimmed))
	scanner.Buffer(make([]byte, 64*1024), 32<<20)
	var rpcErr error
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 || line[0] != '[' {
			continue
		}
		var chunk []json.RawMessage
		if err := json.Unmarshal(line, &chunk); err != nil {
			continue
		}
		for _, rawEntry := range chunk {
			var entry []json.RawMessage
			if err := json.Unmarshal(rawEntry, &entry); err != nil || len(entry) < 3 {
				continue
			}
			var kind string
			if err := json.Unmarshal(entry[0], &kind); err != nil {
				continue
			}
			switch kind {
			case "er":
				var code int
				_ = json.Unmarshal(entry[2], &code)
				var message string
				if len(entry) > 3 {
					_ = json.Unmarshal(entry[3], &message)
				}
				if message == "" {
					message = fmt.Sprintf("Google returned error code %d", code)
				}
				if code == 401 || code == 403 {
					rpcErr = ErrNoSession
				} else {
					rpcErr = fmt.Errorf("Google Photos RPC %s: %s", rpcID, message)
				}
			case "wrb.fr":
				var id string
				_ = json.Unmarshal(entry[1], &id)
				if id != rpcID {
					continue
				}
				if string(entry[2]) == "null" {
					return json.RawMessage("null"), nil
				}
				var data string
				if err := json.Unmarshal(entry[2], &data); err != nil {
					return nil, fmt.Errorf("parse Google Photos RPC %s response: %w", rpcID, err)
				}
				return json.RawMessage(data), nil
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read Google Photos RPC %s response: %w", rpcID, err)
	}
	if rpcErr != nil {
		return nil, rpcErr
	}
	return nil, fmt.Errorf("Google Photos RPC %s returned no result", rpcID)
}

// jarFromCookies parses a Netscape cookies.txt and builds a cookie jar. Google
// account cookies live on .google.com and the Photos session cookies on
// photos.google.com, so both host forms are handled.
func jarFromCookies(data []byte) (http.CookieJar, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	byHost := make(map[string][]*http.Cookie)
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 64*1024), 4<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		httpOnly := false
		if strings.HasPrefix(line, "#HttpOnly_") {
			httpOnly = true
			line = strings.TrimPrefix(line, "#HttpOnly_")
		} else if strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) != 7 {
			return nil, fmt.Errorf("invalid cookie line: expected 7 tab-separated fields, got %d", len(fields))
		}
		domain := strings.TrimSpace(fields[0])
		if domain == "" {
			continue
		}
		cookie := &http.Cookie{
			Domain:   domain,
			Path:     fields[2],
			Secure:   strings.EqualFold(fields[3], "TRUE"),
			HttpOnly: httpOnly,
			Name:     fields[5],
			Value:    fields[6],
		}
		if cookie.Path == "" {
			cookie.Path = "/"
		}
		if exp, err := strconv.ParseInt(strings.TrimSpace(fields[4]), 10, 64); err == nil && exp > 0 {
			cookie.Expires = time.Unix(exp, 0)
		}
		host := strings.TrimPrefix(domain, ".")
		byHost[host] = append(byHost[host], cookie)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read cookies: %w", err)
	}
	if len(byHost) == 0 {
		return nil, fmt.Errorf("cookies file contains no cookies")
	}
	for host, cookies := range byHost {
		jar.SetCookies(&url.URL{Scheme: "https", Host: host}, cookies)
	}
	return jar, nil
}

const userAgent = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"
