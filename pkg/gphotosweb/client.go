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
	"path/filepath"
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
	CoverURL string `json:"coverUrl,omitempty"`
	ShareURL string `json:"shareUrl,omitempty"`
}

// MediaItem is one item in an album. The list RPC is intentionally cheap; the
// filename and original download URL are fetched only when an item is imported.
type MediaItem struct {
	Key         string    `json:"key"`
	ThumbURL    string    `json:"thumbUrl,omitempty"`
	TakenAt     time.Time `json:"takenAt"`
	IsVideo     bool      `json:"isVideo"`
	IsLivePhoto bool      `json:"isLivePhoto"`
}

// MediaInfo is the metadata needed to download one item.
type MediaInfo struct {
	Key         string    `json:"key"`
	Filename    string    `json:"filename"`
	Description string    `json:"description,omitempty"`
	TakenAt     time.Time `json:"takenAt"`
	DownloadURL string    `json:"downloadUrl,omitempty"`
	Size        int64     `json:"size,omitempty"`
	Width       int       `json:"width,omitempty"`
	Height      int       `json:"height,omitempty"`
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
	if len(fields) > 1 {
		var cover []json.RawMessage
		if err := json.Unmarshal(fields[1], &cover); err == nil && len(cover) > 0 {
			album.CoverURL = rawString(cover, 0)
		}
	}
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

func rawInt64(fields []json.RawMessage, idx int) int64 {
	if idx < 0 || idx >= len(fields) {
		return 0
	}
	var value int64
	_ = json.Unmarshal(fields[idx], &value)
	return value
}

// AlbumMediaKeys returns every media key currently in an album. It follows
// the album page token so large albums are covered too. The set is used by the
// optional re-upload check to see whether a previously uploaded item is still
// in the album.
func (c *Client) AlbumMediaKeys(ctx context.Context, albumKey string) (map[string]bool, error) {
	albumKey = strings.TrimSpace(albumKey)
	if albumKey == "" {
		return nil, fmt.Errorf("album key is required")
	}
	keys := make(map[string]bool)
	var page any
	for pageNum := 0; pageNum < 200; pageNum++ {
		data, err := c.rpc(ctx, "snAcKc", []any{albumKey, page, nil, nil})
		if err != nil {
			return nil, err
		}
		var response []json.RawMessage
		if err := json.Unmarshal(data, &response); err != nil {
			return nil, fmt.Errorf("parse album items: %w", err)
		}
		if len(response) < 2 {
			break
		}
		var items []json.RawMessage
		if err := json.Unmarshal(response[1], &items); err != nil {
			return nil, fmt.Errorf("parse album item list: %w", err)
		}
		for _, raw := range items {
			var item []json.RawMessage
			if err := json.Unmarshal(raw, &item); err != nil || len(item) == 0 {
				continue
			}
			var key string
			if err := json.Unmarshal(item[0], &key); err == nil && key != "" {
				keys[key] = true
			}
		}
		if len(response) < 3 || string(response[2]) == "null" {
			break
		}
		var next string
		if err := json.Unmarshal(response[2], &next); err != nil || next == "" {
			break
		}
		page = next
	}
	return keys, nil
}

// ListAlbumItems returns the items in one album, including every page. It is
// used by the experimental private-album import.
func (c *Client) ListAlbumItems(ctx context.Context, albumKey string) ([]MediaItem, error) {
	albumKey = strings.TrimSpace(albumKey)
	if albumKey == "" {
		return nil, fmt.Errorf("album key is required")
	}
	items := make([]MediaItem, 0)
	seen := make(map[string]bool)
	var page any
	for pageNum := 0; pageNum < 200; pageNum++ {
		data, err := c.rpc(ctx, "snAcKc", []any{albumKey, page, nil, nil})
		if err != nil {
			return nil, err
		}
		var response []json.RawMessage
		if err := json.Unmarshal(data, &response); err != nil {
			return nil, fmt.Errorf("parse album items: %w", err)
		}
		if len(response) < 2 {
			break
		}
		var rawItems []json.RawMessage
		if err := json.Unmarshal(response[1], &rawItems); err != nil {
			return nil, fmt.Errorf("parse album item list: %w", err)
		}
		for _, raw := range rawItems {
			item, ok := parseMediaItem(raw)
			if !ok || seen[item.Key] {
				continue
			}
			seen[item.Key] = true
			items = append(items, item)
		}
		if len(response) < 3 || string(response[2]) == "null" {
			break
		}
		var next string
		if err := json.Unmarshal(response[2], &next); err != nil || next == "" {
			break
		}
		page = next
	}
	return items, nil
}

func parseMediaItem(raw json.RawMessage) (MediaItem, bool) {
	var fields []json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || len(fields) == 0 {
		return MediaItem{}, false
	}
	key := rawString(fields, 0)
	if key == "" {
		return MediaItem{}, false
	}
	item := MediaItem{Key: key}
	if len(fields) > 1 {
		var thumb []json.RawMessage
		if err := json.Unmarshal(fields[1], &thumb); err == nil && len(thumb) > 0 {
			item.ThumbURL = rawString(thumb, 0)
		}
	}
	if ms := rawInt64(fields, 2); ms > 0 {
		item.TakenAt = time.UnixMilli(ms).UTC()
	}
	for i := len(fields) - 1; i >= 0; i-- {
		var ext map[string]json.RawMessage
		if err := json.Unmarshal(fields[i], &ext); err != nil {
			continue
		}
		if _, ok := ext["76647426"]; ok {
			item.IsVideo = true
		}
		if _, ok := ext["146008172"]; ok {
			item.IsLivePhoto = true
		}
		break
	}
	return item, true
}

// GetMediaInfo returns the filename, date, and original download URL for one
// item. It batches the two web RPCs the Photos app uses itself.
func (c *Client) GetMediaInfo(ctx context.Context, mediaKey string) (MediaInfo, error) {
	mediaKey = strings.TrimSpace(mediaKey)
	if mediaKey == "" {
		return MediaInfo{}, fmt.Errorf("media key is required")
	}
	results, err := c.rpcBatch(ctx, []rpcRequest{
		{ID: "fDcn4b", Payload: []any{mediaKey, 1, nil, nil, 1}},
		{ID: "VrseUb", Payload: []any{mediaKey, nil, nil, nil, nil}},
	})
	if err != nil && len(results) == 0 {
		return MediaInfo{}, err
	}

	info := MediaInfo{Key: mediaKey}
	if raw, ok := results["fDcn4b"]; ok {
		var response []json.RawMessage
		if err := json.Unmarshal(raw, &response); err == nil && len(response) > 0 {
			var record []json.RawMessage
			if err := json.Unmarshal(response[0], &record); err != nil || len(record) == 0 {
				record = response
			}
			info.Filename = rawString(record, 2)
			info.Description = rawString(record, 1)
			if ms := rawInt64(record, 3); ms > 0 {
				info.TakenAt = time.UnixMilli(ms).UTC()
			}
			info.Size = rawInt64(record, 5)
			info.Width = rawInt(record, 6)
			info.Height = rawInt(record, 7)
		}
	}

	if raw, ok := results["VrseUb"]; ok {
		var response []json.RawMessage
		if err := json.Unmarshal(raw, &response); err == nil {
			info.DownloadURL = rawString(response, 7)
			if info.DownloadURL == "" {
				info.DownloadURL = rawString(response, 1)
			}
			if info.DownloadURL == "" && len(response) > 0 {
				var record []json.RawMessage
				if err := json.Unmarshal(response[0], &record); err == nil {
					info.DownloadURL = rawString(record, 7)
					if info.DownloadURL == "" {
						info.DownloadURL = rawString(record, 1)
					}
				}
			}
		}
	}

	if info.Filename == "" {
		info.Filename = mediaKey + ".jpg"
	}
	if info.DownloadURL == "" {
		return info, fmt.Errorf("Google did not return a download URL for this item")
	}
	return info, nil
}

// DownloadMediaToFile streams one original item to destPath. The Photos CDN
// URL is usually signed; if it rejects the request, the account cookies are
// retried once because the URL belongs to a Google host.
func (c *Client) DownloadMediaToFile(ctx context.Context, mediaKey, destPath string) (MediaInfo, error) {
	info, err := c.GetMediaInfo(ctx, mediaKey)
	if err != nil {
		return info, err
	}

	c.mu.Lock()
	httpClient, clientErr := c.httpClient()
	c.mu.Unlock()
	if clientErr != nil {
		return info, clientErr
	}

	download := func(withCookies bool) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, info.DownloadURL, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", userAgent)
		if withCookies && httpClient.Jar != nil {
			photosURL := &url.URL{Scheme: "https", Host: "photos.google.com"}
			for _, cookie := range httpClient.Jar.Cookies(photosURL) {
				req.AddCookie(cookie)
			}
		}
		return httpClient.Do(req)
	}

	resp, err := download(false)
	if err != nil {
		return info, fmt.Errorf("download Google Photos item: %w", err)
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		resp.Body.Close()
		resp, err = download(true)
		if err != nil {
			return info, fmt.Errorf("download Google Photos item with cookies: %w", err)
		}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return info, fmt.Errorf("download Google Photos item returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	contentType := strings.ToLower(resp.Header.Get("Content-Type"))
	if strings.HasPrefix(contentType, "text/html") || strings.HasPrefix(contentType, "application/json") {
		return info, fmt.Errorf("Google returned %s instead of the original file; refresh the browser session and include the googleusercontent.com cookies if Google asks for them", contentType)
	}

	if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
		return info, fmt.Errorf("create download directory: %w", err)
	}
	file, err := os.Create(destPath)
	if err != nil {
		return info, fmt.Errorf("create download file: %w", err)
	}
	_, copyErr := io.Copy(file, resp.Body)
	closeErr := file.Close()
	if copyErr != nil {
		_ = os.Remove(destPath)
		return info, fmt.Errorf("write download: %w", copyErr)
	}
	if closeErr != nil {
		_ = os.Remove(destPath)
		return info, fmt.Errorf("close download: %w", closeErr)
	}
	return info, nil
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
type rpcRequest struct {
	ID      string
	Payload any
}

func (c *Client) rpc(ctx context.Context, rpcID string, payload any) (json.RawMessage, error) {
	results, err := c.rpcBatch(ctx, []rpcRequest{{ID: rpcID, Payload: payload}})
	if data, ok := results[rpcID]; ok {
		return data, nil
	}
	if err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("Google Photos RPC %s returned no result", rpcID)
}

// rpcBatch sends several RPCs in one batchexecute request. The caller gets a
// map keyed by RPC id and the first error Google reported, if any.
func (c *Client) rpcBatch(ctx context.Context, requests []rpcRequest) (map[string]json.RawMessage, error) {
	if len(requests) == 0 {
		return nil, fmt.Errorf("at least one RPC is required")
	}
	results, err := c.rpcBatchOnce(ctx, requests)
	if err == nil {
		return results, nil
	}
	if !errors.Is(err, ErrNoSession) {
		return results, err
	}
	c.mu.Lock()
	c.session = nil
	c.sessionAt = time.Time{}
	c.mu.Unlock()
	return c.rpcBatchOnce(ctx, requests)
}

func (c *Client) rpcBatchOnce(ctx context.Context, requests []rpcRequest) (map[string]json.RawMessage, error) {
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

	outer := make([]any, 0, len(requests))
	ids := make([]string, 0, len(requests))
	for _, request := range requests {
		inner, err := json.Marshal(request.Payload)
		if err != nil {
			return nil, fmt.Errorf("encode %s payload: %w", request.ID, err)
		}
		outer = append(outer, []any{request.ID, string(inner), nil, "generic"})
		ids = append(ids, request.ID)
	}
	freq, err := json.Marshal([]any{outer})
	if err != nil {
		return nil, fmt.Errorf("encode RPC request: %w", err)
	}
	form := url.Values{}
	form.Set("f.req", string(freq))
	form.Set("at", session.At)

	endpoint := fmt.Sprintf("%s%s/data/batchexecute?rpcids=%s&source-path=%%2F&f.sid=%s&bl=%s&rt=c",
		c.baseURL(), session.RPCPrefix, url.QueryEscape(strings.Join(ids, ",")), url.QueryEscape(session.FSID), url.QueryEscape(session.BL))
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
		return nil, fmt.Errorf("Google Photos RPC %s: %w", strings.Join(ids, ","), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, ErrNoSession
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Google Photos RPC %s returned %s", strings.Join(ids, ","), resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, fmt.Errorf("read Google Photos RPC %s: %w", strings.Join(ids, ","), err)
	}
	return parseRPCResponses(body)
}

func parseRPCResponse(body []byte, rpcID string) (json.RawMessage, error) {
	results, err := parseRPCResponses(body)
	if data, ok := results[rpcID]; ok {
		return data, nil
	}
	if err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("Google Photos RPC %s returned no result", rpcID)
}

func parseRPCResponses(body []byte) (map[string]json.RawMessage, error) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("Google Photos returned an empty response")
	}
	if trimmed[0] == '<' {
		return nil, ErrNoSession
	}
	scanner := bufio.NewScanner(bytes.NewReader(trimmed))
	scanner.Buffer(make([]byte, 64*1024), 32<<20)
	results := make(map[string]json.RawMessage)
	var firstErr error
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
				var rpcErr error
				if code == 401 || code == 403 {
					rpcErr = ErrNoSession
				} else {
					var id string
					_ = json.Unmarshal(entry[1], &id)
					rpcErr = fmt.Errorf("Google Photos RPC %s: %s", id, message)
				}
				if firstErr == nil {
					firstErr = rpcErr
				}
			case "wrb.fr":
				var id string
				_ = json.Unmarshal(entry[1], &id)
				if id == "" {
					continue
				}
				if string(entry[2]) == "null" {
					results[id] = json.RawMessage("null")
					continue
				}
				var data string
				if err := json.Unmarshal(entry[2], &data); err != nil {
					if firstErr == nil {
						firstErr = fmt.Errorf("parse Google Photos RPC %s response: %w", id, err)
					}
					continue
				}
				results[id] = json.RawMessage(data)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return results, fmt.Errorf("read Google Photos response: %w", err)
	}
	if len(results) == 0 && firstErr != nil {
		return nil, firstErr
	}
	return results, firstErr
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
