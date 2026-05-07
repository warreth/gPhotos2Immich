package googlephotos

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"log/slog"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Pre-compiled regexps for scraping performance
var (
	titleRe      = regexp.MustCompile(`<meta property="og:title" content="([^"]+)">`)
	dateSuffixRe = regexp.MustCompile(`\s*·.*$`)
	startRe      = regexp.MustCompile(`key:\s*'ds:1'.*?data:`)
	wizSNlM0eRe  = regexp.MustCompile(`"SNlM0e":"([^"]+)"`)
	wizFdrFJeRe  = regexp.MustCompile(`"FdrFJe":"([^"]+)"`)
	wizCfb2hRe   = regexp.MustCompile(`"cfb2h":"([^"]+)"`)
	wizEptZeRe   = regexp.MustCompile(`"eptZe":"([^"]+)"`)
)

type Album struct {
	ID     string
	Title  string
	Photos []Photo
}

type Photo struct {
	ID          string
	URL         string
	Width       int
	Height      int
	TakenAt     time.Time
	Description string
}

// ScrapeAlbum parses a Google Photos shared album URL and returns the Album structure.
// Handles pagination automatically for albums with more than ~300 items.
func ScrapeAlbum(ctx context.Context, client *Client, albumURL string) (*Album, error) {
	resp, err := client.Get(ctx, albumURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("failed to fetch album: %d", resp.StatusCode)
	}

	// Capture final URL after redirects (short URLs like photos.app.goo.gl redirect to photos.google.com)
	finalURL := albumURL
	if resp.Request != nil && resp.Request.URL != nil {
		finalURL = resp.Request.URL.String()
	}

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	htmlContent := string(bodyBytes)

	// Extract Title from OG:TITLE
	title := "Google Photos Album"
	titleMatch := titleRe.FindStringSubmatch(htmlContent)
	if len(titleMatch) > 1 {
		title = titleMatch[1]
	}

	// Clean Title
	title = html.UnescapeString(title)
	// Remove Date Range Suffix (e.g. " · Feb 6–7") and emojis
	title = dateSuffixRe.ReplaceAllString(title, "")
	title = strings.TrimSpace(title)
	title = strings.TrimSuffix(title, " 📸")

	// Find the start of the data
	// Look for key: 'ds:1' followed by data:
	loc := startRe.FindStringIndex(htmlContent)
	if loc == nil {
		return nil, fmt.Errorf("could not find album data (ds:1) in page")
	}

	startPos := loc[1]
	// Scan forward for first '['
	jsonStart := -1
	for i := startPos; i < len(htmlContent); i++ {
		if htmlContent[i] == '[' {
			jsonStart = i
			break
		}
	}
	if jsonStart == -1 {
		return nil, fmt.Errorf("could not find start of JSON array")
	}

	// Balance brackets to find the end of the JSON array
	balance := 0
	inString := false
	escape := false
	jsonEnd := -1

	for i := jsonStart; i < len(htmlContent); i++ {
		char := htmlContent[i]

		if escape {
			escape = false
			continue
		}

		if char == '\\' {
			escape = true
			continue
		}

		if char == '"' {
			inString = !inString
			continue
		}

		if !inString {
			if char == '[' {
				balance++
			} else if char == ']' {
				balance--
				if balance == 0 {
					jsonEnd = i + 1
					break
				}
			}
		}
	}

	if jsonEnd == -1 {
		return nil, fmt.Errorf("could not find end of JSON array")
	}

	jsonStr := htmlContent[jsonStart:jsonEnd]
	
	// Pre-cleanup of JSON string if needed (sometimes unescaping)
	// Usually it's valid JSON directly in the script tag
	
	var data []interface{}
	err = json.Unmarshal([]byte(jsonStr), &data)
	if err != nil {
		return nil, fmt.Errorf("failed to parse album JSON: %v", err)
	}

	// Structure: [metadata, [item1, item2, ...], token, ...]
	// Index 1 is usually the item list.
	var list []interface{}
	if len(data) > 1 {
		if l, ok := data[1].([]interface{}); ok {
			list = l
		}
	}
	// Fallback check
	if list == nil && len(data) > 0 {
		if l, ok := data[0].([]interface{}); ok {
			list = l
		}
	}

	// Parse initial batch of items from embedded page data
	photos := parsePhotoItems(list)

	// Extract pagination tokens for fetching remaining album items
	wiz := extractWizTokens(htmlContent)
	var continueToken string
	// Primary: continuation token is at data[2]
	if len(data) > 2 {
		if tok, ok := data[2].(string); ok && tok != "" {
			continueToken = tok
		}
	}
	// Fallback: scan all top-level string elements after the item list for a long token-like string
	if continueToken == "" {
		for i := 2; i < len(data); i++ {
			if tok, ok := data[i].(string); ok && len(tok) > 10 {
				continueToken = tok
				break
			}
		}
	}

	// Paginate through remaining pages via batchexecute API
	// Note: wiz.AT (SNlM0e CSRF token) is NOT present on public shared album pages
	// batchexecute works without it for public albums
	if continueToken != "" {
		sourcePath, mediaKey := extractAlbumPath(finalURL)
		authKey := extractAuthKeyFromURL(finalURL)

		// Fallback: extract mediaKey from embedded album metadata at data[3][0]
		if mediaKey == "" && len(data) > 3 {
			if meta, ok := data[3].([]interface{}); ok && len(meta) > 0 {
				if key, ok := meta[0].(string); ok && key != "" {
					mediaKey = key
				}
			}
		}

		// Fallback: extract authKey from embedded album metadata at data[3][19]
		if authKey == "" && len(data) > 3 {
			if meta, ok := data[3].([]interface{}); ok && len(meta) > 19 {
				if key, ok := meta[19].(string); ok {
					authKey = key
				}
			}
		}

		if mediaKey != "" {
			client.logger.Info("Album has continuation token, fetching remaining items", "count", len(photos))
			const maxPages = 500
			for page := 0; page < maxPages && continueToken != ""; page++ {
				client.logger.Debug("Fetching album page", "page", page+2, "total_items", len(photos))
				nextPhotos, nextToken, fetchErr := fetchNextPage(ctx, client, mediaKey, authKey, continueToken, sourcePath, wiz)
				if fetchErr != nil {
					client.logger.Warn("Pagination stopped", "page", page+2, "error", fetchErr)
					break
				}
				if len(nextPhotos) == 0 {
					break
				}
				photos = append(photos, nextPhotos...)
				continueToken = nextToken
			}
		} else {
			client.logger.Warn("Could not determine album mediaKey, pagination skipped")
		}
	}

	// Remove duplicate photos from overlapping pages
	photos = deduplicatePhotos(photos)

	return &Album{
		ID:     finalURL,
		Title:  title,
		Photos: photos,
	}, nil
}

// extractInt converts interface{} values to int64 (handles JSON string and float64)
func extractInt(v interface{}) (int64, bool) {
	switch val := v.(type) {
	case string:
		if i, err := strconv.ParseInt(val, 10, 64); err == nil {
			return i, true
		}
	case float64:
		return int64(val), true
	}
	return 0, false
}

// normalizeTimestamp converts timestamps in various epoch units to milliseconds
func normalizeTimestamp(t int64) int64 {
	if t > 1e15 {
		return t / 1000 // microseconds to milliseconds
	}
	if t > 0 && t < 1e10 {
		return t * 1000 // seconds to milliseconds
	}
	return t
}

// parsePhotoItems extracts Photo structs from a list of raw scraped item arrays
func parsePhotoItems(list []interface{}) []Photo {
	var photos []Photo
	for _, item := range list {
		itemArr, ok := item.([]interface{})
		if !ok || len(itemArr) < 2 {
			continue
		}

		id, _ := itemArr[0].(string)

		mediaArr, ok := itemArr[1].([]interface{})
		if !ok || len(mediaArr) < 1 {
			continue
		}

		photoURL, _ := mediaArr[0].(string)
		w := 0
		h := 0
		if len(mediaArr) >= 3 {
			if fw, ok := mediaArr[1].(float64); ok {
				w = int(fw)
			}
			if fh, ok := mediaArr[2].(float64); ok {
				h = int(fh)
			}
		}

		timestamp := extractTimestamp(itemArr)

		var description string
		for i := 3; i < len(itemArr); i++ {
			if d, ok := itemArr[i].(string); ok && d != "" {
				description = d
				break
			}
		}

		if photoURL != "" {
			photos = append(photos, Photo{
				ID:          id,
				URL:         photoURL,
				Width:       w,
				Height:      h,
				TakenAt:     timestamp,
				Description: description,
			})
		}
	}

	return photos
}

// wizTokens holds Google session tokens needed for pagination requests
type wizTokens struct {
	AT   string // CSRF token (SNlM0e)
	SID  string // Session ID (FdrFJe)
	BL   string // Build label (cfb2h)
	Path string // URL path prefix (eptZe), typically "/_/PhotosUi/"
}

// extractWizTokens parses WIZ_global_data tokens from page HTML for batchexecute requests
func extractWizTokens(htmlContent string) wizTokens {
	var tokens wizTokens
	if m := wizSNlM0eRe.FindStringSubmatch(htmlContent); len(m) > 1 {
		tokens.AT = m[1]
	}
	if m := wizFdrFJeRe.FindStringSubmatch(htmlContent); len(m) > 1 {
		tokens.SID = m[1]
	}
	if m := wizCfb2hRe.FindStringSubmatch(htmlContent); len(m) > 1 {
		tokens.BL = m[1]
	}
	if m := wizEptZeRe.FindStringSubmatch(htmlContent); len(m) > 1 {
		tokens.Path = m[1]
	}
	if tokens.Path == "" {
		tokens.Path = "/_/PhotosUi/"
	}
	return tokens
}

// extractAlbumPath returns the source-path and album media key from a shared album URL
func extractAlbumPath(rawURL string) (string, string) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", ""
	}

	sourcePath := u.Path
	if u.RawQuery != "" {
		sourcePath += "?" + u.RawQuery
	}

	// Extract media key from path: /share/<mediaKey>
	parts := strings.Split(strings.TrimRight(u.Path, "/"), "/")
	mediaKey := ""
	for i, p := range parts {
		if p == "share" && i+1 < len(parts) {
			mediaKey = parts[i+1]
			break
		}
	}

	return sourcePath, mediaKey
}

// extractAuthKeyFromURL gets the shared album auth key from URL query parameter
func extractAuthKeyFromURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return u.Query().Get("key")
}

// fetchNextPage calls Google's internal batchexecute API to get the next page of album items
func fetchNextPage(ctx context.Context, client *Client, mediaKey, authKey, pageToken, sourcePath string, wiz wizTokens) ([]Photo, string, error) {
	// Build the inner request payload
	innerData := []interface{}{mediaKey, pageToken, nil, authKey}
	innerJSON, err := json.Marshal(innerData)
	if err != nil {
		return nil, "", fmt.Errorf("failed to marshal inner request: %w", err)
	}

	// Wrap in batchexecute envelope
	outerData := []interface{}{
		[]interface{}{
			[]interface{}{"snAcKc", string(innerJSON), nil, "generic"},
		},
	}
	outerJSON, err := json.Marshal(outerData)
	if err != nil {
		return nil, "", fmt.Errorf("failed to marshal outer request: %w", err)
	}

	formBody := url.Values{}
	formBody.Set("f.req", string(outerJSON))
	// AT (CSRF token) is optional — not present on public shared album pages
	if wiz.AT != "" {
		formBody.Set("at", wiz.AT)
	}

	batchURL := fmt.Sprintf(
		"https://photos.google.com%sdata/batchexecute?rpcids=snAcKc&source-path=%s&f.sid=%s&bl=%s&pageId=none&rt=c",
		wiz.Path,
		url.QueryEscape(sourcePath),
		url.QueryEscape(wiz.SID),
		url.QueryEscape(wiz.BL),
	)

	resp, err := client.Post(ctx, batchURL, "application/x-www-form-urlencoded;charset=UTF-8", formBody.Encode())
	if err != nil {
		return nil, "", fmt.Errorf("batchexecute request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, "", fmt.Errorf("batchexecute returned status %d", resp.StatusCode)
	}

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", fmt.Errorf("failed to read batchexecute response: %w", err)
	}

	return parseBatchResponse(string(respBody))
}

// parseBatchResponse parses Google's batchexecute multi-line RPC response format
func parseBatchResponse(body string) ([]Photo, string, error) {
	lines := strings.Split(body, "\n")

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if !strings.Contains(line, "wrb.fr") {
			continue
		}

		// Parse the envelope JSON
		var envelope []interface{}
		if err := json.Unmarshal([]byte(line), &envelope); err != nil {
			continue
		}

		if len(envelope) == 0 {
			continue
		}

		respArr, ok := envelope[0].([]interface{})
		if !ok || len(respArr) < 3 {
			continue
		}

		// Verify RPC ID matches our request
		rpcId, _ := respArr[1].(string)
		if rpcId != "snAcKc" {
			continue
		}

		payloadStr, ok := respArr[2].(string)
		if !ok || payloadStr == "" {
			continue
		}

		// Parse the actual data payload
		var payload []interface{}
		if err := json.Unmarshal([]byte(payloadStr), &payload); err != nil {
			continue
		}

		// Extract items from payload[1]
		var photos []Photo
		if len(payload) > 1 {
			if items, ok := payload[1].([]interface{}); ok {
				photos = parsePhotoItems(items)
			}
		}

		// Extract continuation token from payload[2]
		var nextToken string
		if len(payload) > 2 {
			if tok, ok := payload[2].(string); ok {
				nextToken = tok
			}
		}

		return photos, nextToken, nil
	}

	return nil, "", fmt.Errorf("no valid response envelope found in batchexecute response")
}

// deduplicatePhotos removes duplicate photos based on their ID
func deduplicatePhotos(photos []Photo) []Photo {
	seen := make(map[string]bool, len(photos))
	result := make([]Photo, 0, len(photos))
	for _, p := range photos {
		if p.ID != "" && !seen[p.ID] {
			seen[p.ID] = true
			result = append(result, p)
		}
	}
	return result
}

// extractTimestamp extracts the best available timestamp from a scraped item
func extractTimestamp(itemArr []interface{}) time.Time {
	now := time.Now()
	var candidates []int64

	// Collect all plausible timestamps from the item
	for i := 2; i < len(itemArr); i++ {
		if metaArr, ok := itemArr[i].([]interface{}); ok && len(metaArr) > 0 {
			if t, ok := extractInt(metaArr[0]); ok {
				t = normalizeTimestamp(t)
				if t > 946684800000 && time.UnixMilli(t).Before(now.Add(24*time.Hour)) {
					candidates = append(candidates, t)
				}
			}
		}
		if t, ok := extractInt(itemArr[i]); ok {
			t = normalizeTimestamp(t)
			if t > 946684800000 && time.UnixMilli(t).Before(now.Add(24*time.Hour)) {
				candidates = append(candidates, t)
			}
		}
	}

	if len(candidates) == 0 {
		return time.Time{}
	}

	// Prefer the oldest valid timestamp (most likely the "taken" date)
	best := candidates[0]
	for _, c := range candidates[1:] {
		if c < best {
			best = c
		}
	}

	return time.UnixMilli(best)
}

// hasMotionPhotoXMP checks if data contains any motion photo XMP markers
func hasMotionPhotoXMP(data []byte) bool {
	markers := [][]byte{
		[]byte(`MotionPhoto="1"`),
		[]byte(`MicroVideo="1"`),
		[]byte("MotionPhoto>1<"),
		[]byte("MicroVideo>1<"),
	}
	for _, m := range markers {
		if bytes.Contains(data, m) {
			return true
		}
	}
	return false
}

// ExtractMotionPhoto checks if a JPEG contains an embedded MP4 video.
// Returns hadMotionXMP=true when motion markers are present even if no video payload
// is embedded in the image bytes.
func ExtractMotionPhoto(data []byte, logger *slog.Logger) ([]byte, []byte, bool, bool) {
	if !hasMotionPhotoXMP(data) {
		return data, nil, false, false
	}

	// Check if actual video data is embedded
	ftypMagic := []byte("ftyp")
	lastFtyp := bytes.LastIndex(data, ftypMagic)

	// ftyp must be after image header (>4KB) with 4 bytes before for box size
	if lastFtyp >= 8 && lastFtyp > 4096 {
		videoStart := lastFtyp - 4
		videoSize := len(data) - videoStart

		// Validate: box size should be reasonable (8 bytes to 1MB) and video should be > 1KB
		boxSize := int(data[videoStart])<<24 | int(data[videoStart+1])<<16 | int(data[videoStart+2])<<8 | int(data[videoStart+3])
		if boxSize >= 8 && boxSize <= 1024*1024 && videoSize > 1024 {
			videoData := data[videoStart:]
			imageData := make([]byte, videoStart)
			copy(imageData, data[:videoStart])
			StripMotionPhotoXMP(imageData)

			logger.Debug("Motion photo extracted",
				"image_size", videoStart,
				"video_size", videoSize,
				"ftyp_offset", lastFtyp,
			)
			return imageData, videoData, true, true
		}
	}

	// Has motion photo XMP but no valid embedded video in the container bytes.
	// Caller may attempt sidecar download (=dv) before deciding to strip XMP.
	logger.Debug("Motion photo XMP found but no embedded video",
		"file_size", len(data),
	)
	return data, nil, false, true
}

// StripMotionPhotoXMP disables motion photo flags in XMP metadata (same-length replacements)
func StripMotionPhotoXMP(data []byte) {
	replacements := []struct{ old, new []byte }{
		{[]byte(`MotionPhoto="1"`), []byte(`MotionPhoto="0"`)},
		{[]byte(`MicroVideo="1"`), []byte(`MicroVideo="0"`)},
		{[]byte("MotionPhoto>1<"), []byte("MotionPhoto>0<")},
		{[]byte("MicroVideo>1<"), []byte("MicroVideo>0<")},
	}
	for _, r := range replacements {
		if idx := bytes.Index(data, r.old); idx != -1 {
			copy(data[idx:], r.new)
		}
	}
}

// extensionFromContentType maps Content-Type to file extension
func extensionFromContentType(contentType string) string {
	ct := strings.ToLower(strings.TrimSpace(strings.SplitN(contentType, ";", 2)[0]))
	switch ct {
	case "image/jpeg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	case "image/heic", "image/heif":
		return ".heic"
	case "image/avif":
		return ".avif"
	case "video/mp4":
		return ".mp4"
	case "video/webm":
		return ".webm"
	case "video/quicktime":
		return ".mov"
	case "video/x-matroska":
		return ".mkv"
	default:
		if strings.HasPrefix(ct, "video/") {
			return ".mp4"
		}
		return ".jpg"
	}
}

// isVideoMagicBytes checks if data starts with known video container signatures
func isVideoMagicBytes(data []byte) bool {
	if len(data) < 12 {
		return false
	}
	// MP4/MOV: ftyp box at offset 4
	if string(data[4:8]) == "ftyp" {
		return true
	}
	// WebM/Matroska: EBML header
	if data[0] == 0x1A && data[1] == 0x45 && data[2] == 0xDF && data[3] == 0xA3 {
		return true
	}
	// AVI: RIFF....AVI
	if string(data[0:4]) == "RIFF" && len(data) >= 12 && string(data[8:12]) == "AVI " {
		return true
	}
	return false
}

// DownloadMedia downloads original media from Google Photos.
// It always tries =d first so motion photos are fetched as their JPEG container
// (e.g. *.MP.jpg) instead of the short motion-video stream from =dv.
func DownloadMedia(ctx context.Context, client *Client, baseUrl string) ([]byte, string, bool, error) {
	// Always fetch =d first. For motion photos this is the image container file.
	resp, err := client.Get(ctx, baseUrl+"=d")
	if err != nil {
		return nil, "", false, fmt.Errorf("download failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, "", false, fmt.Errorf("download returned status %d", resp.StatusCode)
	}

	ct := resp.Header.Get("Content-Type")
	// If =d is already a video response, avoid reading that whole body unless we
	// have to fall back to it. Prefer =dv for the actual playable payload.
	if strings.HasPrefix(strings.ToLower(ct), "video/") {
		resp2, err := client.Get(ctx, baseUrl+"=dv")
		if err != nil {
			return nil, "", false, fmt.Errorf("video re-download failed: %w", err)
		}
		defer resp2.Body.Close()
		if resp2.StatusCode == 200 {
			videoCt := resp2.Header.Get("Content-Type")
			videoData, err := io.ReadAll(resp2.Body)
			if err != nil {
				return nil, "", false, fmt.Errorf("failed to read video response body: %w", err)
			}
			ext := extensionFromContentType(videoCt)
			return videoData, ext, true, nil
		}
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", false, fmt.Errorf("failed to read response body: %w", err)
	}

	// If =d is actually a video item, prefer =dv for the original video stream.
	isVideo := isVideoMagicBytes(data)
	if isVideo {
		resp2, err := client.Get(ctx, baseUrl+"=dv")
		if err != nil {
			return nil, "", false, fmt.Errorf("video re-download failed: %w", err)
		}
		defer resp2.Body.Close()
		if resp2.StatusCode == 200 {
			videoCt := resp2.Header.Get("Content-Type")
			videoData, err := io.ReadAll(resp2.Body)
			if err != nil {
				return nil, "", false, fmt.Errorf("failed to read video response body: %w", err)
			}
			ext := extensionFromContentType(videoCt)
			return videoData, ext, true, nil
		}
		// =dv failed, fall through and use the =d data as-is
	}

	// Some Google Photos shared-album video items may return an image poster on =d.
	// If this is not a motion-photo container, probe =dv and treat valid video as the primary asset.
	if !hasMotionPhotoXMP(data) {
		if sidecarData, sidecarExt, sidecarErr := DownloadMotionVideoSidecar(ctx, client, baseUrl); sidecarErr == nil {
			return sidecarData, sidecarExt, true, nil
		}
	}

	// Fallback: determine if this is video by checking both Content-Type and magic bytes.
	// If =dv retry failed but Content-Type indicated video, trust that classification.
	isVideoFinal := strings.HasPrefix(strings.ToLower(ct), "video/") || isVideoMagicBytes(data)
	ext := extensionFromContentType(ct)
	return data, ext, isVideoFinal, nil
}

// DownloadMotionVideoSidecar fetches the motion sidecar stream (if present) for an image item.
func DownloadMotionVideoSidecar(ctx context.Context, client *Client, baseUrl string) ([]byte, string, error) {
	resp, err := client.Get(ctx, baseUrl+"=dv")
	if err != nil {
		return nil, "", fmt.Errorf("sidecar download failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, "", fmt.Errorf("sidecar download returned status %d", resp.StatusCode)
	}

	ct := resp.Header.Get("Content-Type")

	var data []byte
	if !strings.HasPrefix(strings.ToLower(ct), "video/") {
		// If it's not explicitly marked as video, sniff a small chunk before downloading fully
		buf := make([]byte, 512)
		n, _ := io.ReadFull(resp.Body, buf)
		if n > 0 && !isVideoMagicBytes(buf[:n]) {
			return nil, "", fmt.Errorf("sidecar is not a video payload (type: %s)", ct)
		}
		
		rest, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, "", fmt.Errorf("failed to read sidecar body: %w", err)
		}
		data = append(buf[:n], rest...)
	} else {
		var err error
		data, err = io.ReadAll(resp.Body)
		if err != nil {
			return nil, "", fmt.Errorf("failed to read sidecar body: %w", err)
		}
	}

	isVideo := strings.HasPrefix(strings.ToLower(ct), "video/") || isVideoMagicBytes(data)
	if !isVideo || len(data) <= 1024 {
		return nil, "", fmt.Errorf("sidecar is not a valid video payload")
	}

	ext := extensionFromContentType(ct)
	if ext == "" || ext == ".jpg" {
		ext = ".mp4"
	}

	return data, ext, nil
}
