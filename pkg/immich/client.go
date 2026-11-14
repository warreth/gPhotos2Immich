package immich

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"warreth.dev/gphotos2immich/pkg/util"
)

const (
	apiMaxRetries    = 3
	apiRetryDelay    = 2 * time.Second
	uploadMaxRetries = 3
	uploadRetryDelay = 3 * time.Second
)

type Album struct {
	AlbumName string       `json:"albumName"`
	Id        string       `json:"id"`
	Assets    []AlbumAsset `json:"assets"`
}

// AlbumAsset is one asset inside an album listing.
type AlbumAsset struct {
	Id               string    `json:"id"`
	OriginalFileName string    `json:"originalFileName"`
	Type             string    `json:"type"`
	LivePhotoVideoID string    `json:"livePhotoVideoId"`
	FileCreatedAt    time.Time `json:"fileCreatedAt"`
	Checksum         string    `json:"checksum"`
	// OwnerID is the Immich user that owns the asset. It decides which Google
	// account an asset is uploaded with.
	OwnerID string `json:"ownerId"`
}

// User is one Immich user.
type User struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
}

// HTTPError is returned when Immich responds with a non-2xx status code.
// Callers can use errors.As to distinguish client errors (e.g. an invalid API
// key) from transient connection failures.
type HTTPError struct {
	StatusCode int
	Status     string
	Body       string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("API error: %s - %s", e.Status, e.Body)
}

// AssetIndex is an in-memory view of the assets uploaded by this tool. It is
// built from album details or from a metadata search and is used for dedup and
// for checking whether persistent state entries still exist in Immich.
type AssetIndex struct {
	Images       map[string]string // normalized Google item name -> image asset ID
	Videos       map[string]string // normalized Google item name -> video asset ID
	VideoFiles   map[string]string // full video filename -> video asset ID
	IDs          map[string]bool   // every asset ID in the index
	Types        map[string]string // asset ID -> IMAGE or VIDEO
	Descriptions map[string]string // asset ID -> current description (optional)
	Count        int
}

// NewAssetIndex creates an empty asset index.
func NewAssetIndex() *AssetIndex {
	return &AssetIndex{
		Images:       make(map[string]string),
		Videos:       make(map[string]string),
		VideoFiles:   make(map[string]string),
		IDs:          make(map[string]bool),
		Types:        make(map[string]string),
		Descriptions: make(map[string]string),
	}
}

// NormalizeAssetName reduces an Immich filename to the stable Google item name
// used by this tool. It strips the extension and any compressor marker such as
// ".cmp" from "gp_<id>.cmp.jpg", yielding "gp_<id>".
func NormalizeAssetName(filename string) string {
	base := util.StripExtension(filename)
	if strings.HasPrefix(base, "gp_") {
		if dot := strings.Index(base, "."); dot > 0 {
			base = base[:dot]
		}
	}
	return base
}

// Add registers one asset in the index.
func (i *AssetIndex) Add(id, filename, assetType, deviceAssetID string) {
	if i == nil || id == "" {
		return
	}
	i.Count++
	i.IDs[id] = true
	if assetType == "" {
		if util.IsVideoFilename(filename) {
			assetType = "VIDEO"
		} else {
			assetType = "IMAGE"
		}
	}
	i.Types[id] = assetType
	if filename != "" {
		base := NormalizeAssetName(filename)
		if assetType == "VIDEO" {
			i.Videos[base] = id
			i.VideoFiles[filename] = id
		} else {
			i.Images[base] = id
		}
	}
	// Older Immich versions expose deviceAssetId; use it as a fallback for
	// images so old-format names such as "gp_<id>.jpg-12345" still match.
	if deviceAssetID != "" {
		daid := stripNumericSuffix(deviceAssetID)
		if !util.IsVideoFilename(daid) {
			base := NormalizeAssetName(daid)
			if _, exists := i.Images[base]; !exists {
				i.Images[base] = id
			}
		}
	}
}

// FindImage looks up an image by its normalized Google item name.
func (i *AssetIndex) FindImage(baseName string) (string, bool) {
	if i == nil {
		return "", false
	}
	id, ok := i.Images[baseName]
	return id, ok
}

// FindVideo looks up a video by its normalized Google item name or full filename.
func (i *AssetIndex) FindVideo(baseName, filename string) (string, bool) {
	if i == nil {
		return "", false
	}
	if filename != "" {
		if id, ok := i.VideoFiles[filename]; ok {
			return id, true
		}
	}
	id, ok := i.Videos[baseName]
	return id, ok
}

// FindReplacement looks up an existing asset of the same media type when the
// persistent state entry is stale.
func (i *AssetIndex) FindReplacement(baseName string, isVideo bool) (string, bool) {
	if isVideo {
		return i.FindVideo(baseName, "")
	}
	return i.FindImage(baseName)
}

// stripNumericSuffix removes an old-style "-<size>" suffix from a deviceAssetId.
func stripNumericSuffix(s string) string {
	if dash := strings.LastIndex(s, "-"); dash > 0 {
		suffix := s[dash+1:]
		allDigits := suffix != ""
		for _, c := range suffix {
			if c < '0' || c > '9' {
				allDigits = false
				break
			}
		}
		if allDigits {
			return s[:dash]
		}
	}
	return s
}

type Client struct {
	APIURL         string
	APIKey         string
	client         *http.Client
	downloadClient *http.Client
}

// NewClient creates an Immich API client with connection pooling tuned to the given concurrency level
func NewClient(apiURL, apiKey string, maxConnsPerHost int) *Client {
	apiURL = strings.TrimSuffix(apiURL, "/")
	if maxConnsPerHost < 20 {
		maxConnsPerHost = 20
	}
	transport := &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: maxConnsPerHost,
		IdleConnTimeout:     90 * time.Second,
		ForceAttemptHTTP2:   true,
	}
	return &Client{
		APIURL: apiURL,
		APIKey: apiKey,
		client: &http.Client{
			Transport: transport,
			Timeout:   120 * time.Second,
		},
		// Original files can be large videos. API calls keep their timeout,
		// downloads rely on the caller's context instead.
		downloadClient: &http.Client{
			Transport: transport,
			Timeout:   0,
		},
	}
}

// doHTTP executes a single HTTP request and returns the response body, status code, and any error
func (c *Client) doHTTP(ctx context.Context, method, path string, body io.Reader, contentType string) ([]byte, int, error) {
	url := fmt.Sprintf("%s/%s", c.APIURL, path)

	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, 0, err
	}

	req.Header.Set("Accept", "application/json")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	} else {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("x-api-key", c.APIKey)

	res, err := c.client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer res.Body.Close()

	respBody, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, res.StatusCode, err
	}

	if res.StatusCode >= 400 {
		return respBody, res.StatusCode, &HTTPError{
			StatusCode: res.StatusCode,
			Status:     res.Status,
			Body:       string(respBody),
		}
	}

	return respBody, res.StatusCode, nil
}

// request performs a JSON API call with automatic retry on transient errors
func (c *Client) request(ctx context.Context, method, path string, payload []byte, contentType string) ([]byte, error) {
	var lastErr error
	var lastBody []byte

	for attempt := 0; attempt <= apiMaxRetries; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(apiRetryDelay * time.Duration(attempt)):
			}
		}

		var body io.Reader
		if payload != nil {
			body = bytes.NewReader(payload)
		}

		respBody, statusCode, err := c.doHTTP(ctx, method, path, body, contentType)
		if err == nil {
			return respBody, nil
		}

		lastErr = err
		lastBody = respBody

		// Don't retry 4xx client errors (except 429 rate limit)
		if statusCode >= 400 && statusCode < 500 && statusCode != 429 {
			return respBody, err
		}
	}

	if lastErr != nil {
		return lastBody, fmt.Errorf("request %s %s failed after %d retries: %w", method, path, apiMaxRetries, lastErr)
	}
	return lastBody, nil
}

func (c *Client) GetAlbums(ctx context.Context) ([]Album, error) {
	body, err := c.request(ctx, "GET", "albums", nil, "")
	if err != nil {
		return nil, err
	}
	var albums []Album
	err = json.Unmarshal(body, &albums)
	return albums, err
}

// ListAlbums returns every album owned by the API key's user.
func (c *Client) ListAlbums(ctx context.Context) ([]Album, error) {
	body, err := c.request(ctx, "GET", "albums", nil, "")
	if err != nil {
		return nil, err
	}
	var albums []Album
	if err := json.Unmarshal(body, &albums); err != nil {
		return nil, err
	}
	return albums, nil
}

// GetAlbum fetches a single album with its full asset list
func (c *Client) GetAlbum(ctx context.Context, albumId string) (*Album, error) {
	body, err := c.request(ctx, "GET", fmt.Sprintf("albums/%s", albumId), nil, "")
	if err != nil {
		return nil, err
	}
	var album Album
	err = json.Unmarshal(body, &album)
	return &album, err
}

func (c *Client) CreateAlbum(ctx context.Context, name string) (*Album, error) {
	payload := map[string]string{"albumName": name}
	jsonPayload, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal create album payload: %w", err)
	}
	body, err := c.request(ctx, "POST", "albums", jsonPayload, "")
	if err != nil {
		return nil, err
	}
	var album Album
	err = json.Unmarshal(body, &album)
	if err != nil {
		return nil, err
	}
	if album.Id == "" {
		return nil, fmt.Errorf("created album has no ID (response: %s)", string(body))
	}
	return &album, err
}

func (c *Client) AddAssetsToAlbum(ctx context.Context, albumId string, assetIds []string) error {
	const batchSize = 100
	for i := 0; i < len(assetIds); i += batchSize {
		if err := ctx.Err(); err != nil {
			return err
		}

		end := i + batchSize
		if end > len(assetIds) {
			end = len(assetIds)
		}

		chunk := assetIds[i:end]
		payload := map[string]interface{}{"ids": chunk}
		jsonPayload, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("failed to marshal add assets payload: %w", err)
		}
		_, err = c.request(ctx, "PUT", fmt.Sprintf("albums/%s/assets", albumId), jsonPayload, "")
		if err != nil {
			return err
		}
	}
	return nil
}

// UploadAsset uploads a file to Immich with automatic retry on transient errors
func (c *Client) UploadAsset(ctx context.Context, data []byte, filename string, createdAt time.Time, description string) (string, bool, error) {
	return c.uploadAssetWithOptions(ctx, data, filename, createdAt, description, "", "")
}

// UploadAssetWithLive uploads an asset and optionally links it to a live photo video, with retry
func (c *Client) UploadAssetWithLive(ctx context.Context, data []byte, filename string, createdAt time.Time, description string, livePhotoVideoId string) (string, bool, error) {
	return c.uploadAssetWithOptions(ctx, data, filename, createdAt, description, livePhotoVideoId, "")
}

// UploadAssetWithVisibility uploads an asset with optional custom visibility (e.g. "hidden").
func (c *Client) UploadAssetWithVisibility(ctx context.Context, data []byte, filename string, createdAt time.Time, description string, visibility string) (string, bool, error) {
	return c.uploadAssetWithOptions(ctx, data, filename, createdAt, description, "", visibility)
}

func (c *Client) uploadAssetWithOptions(ctx context.Context, data []byte, filename string, createdAt time.Time, description string, livePhotoVideoId string, visibility string) (string, bool, error) {
	return c.uploadWithRetry(ctx, func() (io.Reader, error) {
		return bytes.NewReader(data), nil
	}, filename, createdAt, description, livePhotoVideoId, visibility)
}

// UploadAssetFromFile uploads a file without reading it into memory. The file
// is reopened for each retry so a transient failure does not send an empty body.
func (c *Client) UploadAssetFromFile(ctx context.Context, path, filename string, createdAt time.Time, description string) (string, bool, error) {
	if filename == "" {
		filename = filepath.Base(path)
	}
	return c.uploadWithRetry(ctx, func() (io.Reader, error) {
		return os.Open(path)
	}, filename, createdAt, description, "", "")
}

func (c *Client) uploadWithRetry(ctx context.Context, open func() (io.Reader, error), filename string, createdAt time.Time, description string, livePhotoVideoId string, visibility string) (string, bool, error) {
	var lastErr error
	for attempt := 0; attempt <= uploadMaxRetries; attempt++ {
		if err := ctx.Err(); err != nil {
			return "", false, err
		}
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return "", false, ctx.Err()
			case <-time.After(uploadRetryDelay * time.Duration(attempt)):
			}
		}
		source, err := open()
		if err != nil {
			return "", false, err
		}
		id, isDup, uploadErr := c.doUpload(ctx, source, filename, createdAt, description, livePhotoVideoId, visibility)
		if closer, ok := source.(io.Closer); ok {
			_ = closer.Close()
		}
		if uploadErr == nil {
			return id, isDup, nil
		}
		lastErr = uploadErr
	}
	return "", false, fmt.Errorf("upload of %s failed after %d retries: %w", filename, uploadMaxRetries, lastErr)
}

// doUpload performs a single multipart upload attempt. The caller owns source.
func (c *Client) doUpload(ctx context.Context, source io.Reader, filename string, createdAt time.Time, description string, livePhotoVideoId string, visibility string) (string, bool, error) {
	pr, pw := io.Pipe()
	multipartWriter := multipart.NewWriter(pw)

	go func() {
		defer pw.Close()
		defer multipartWriter.Close()

		_ = multipartWriter.WriteField("deviceAssetId", filename)
		_ = multipartWriter.WriteField("deviceId", "gphotos2immich-go")

		creationTime := time.Now()
		if !createdAt.IsZero() {
			creationTime = createdAt
		}

		_ = multipartWriter.WriteField("fileCreatedAt", creationTime.Format(time.RFC3339))
		_ = multipartWriter.WriteField("fileModifiedAt", creationTime.Format(time.RFC3339))
		_ = multipartWriter.WriteField("isFavorite", "false")
		if description != "" {
			_ = multipartWriter.WriteField("description", description)
		}
		if livePhotoVideoId != "" {
			_ = multipartWriter.WriteField("livePhotoVideoId", livePhotoVideoId)
		}
		if visibility != "" {
			_ = multipartWriter.WriteField("visibility", visibility)
		}

		part, err := multipartWriter.CreateFormFile("assetData", filename)
		if err != nil {
			pw.CloseWithError(err)
			return
		}
		if _, err := io.Copy(part, source); err != nil {
			pw.CloseWithError(err)
			return
		}
	}()

	resp, _, err := c.doHTTP(ctx, "POST", "assets", pr, multipartWriter.FormDataContentType())
	if err != nil {
		return "", false, err
	}

	var res map[string]interface{}
	json.Unmarshal(resp, &res)

	isDup := false
	if d, ok := res["duplicate"].(bool); ok && d {
		isDup = true
	}
	// Newer Immich versions use "status": "duplicate" instead of "duplicate": true
	if s, ok := res["status"].(string); ok && s == "duplicate" {
		isDup = true
	}

	if id, ok := res["id"].(string); ok {
		return id, isDup, nil
	}

	if msg, ok := res["message"].(string); ok {
		return "", false, fmt.Errorf("upload failed: %s", msg)
	}

	return "", false, fmt.Errorf("upload returned no ID (response: %s)", string(resp))
}

func (c *Client) GetUser(ctx context.Context) (string, string, error) {
	body, err := c.request(ctx, "GET", "users/me", nil, "")
	if err != nil {
		return "", "", err
	}
	var user struct {
		Id   string `json:"id"`
		Name string `json:"name"`
	}
	err = json.Unmarshal(body, &user)
	return user.Id, user.Name, err
}

// GetUserOnce performs a single users/me request without the API retry loop.
// The web UI connection test uses it so a wrong host fails fast.
func (c *Client) GetUserOnce(ctx context.Context) (string, string, error) {
	body, _, err := c.doHTTP(ctx, "GET", "users/me", nil, "")
	if err != nil {
		return "", "", err
	}
	var user struct {
		Id   string `json:"id"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(body, &user); err != nil {
		return "", "", err
	}
	return user.Id, user.Name, nil
}

// ListUsers returns every user on the Immich server.
func (c *Client) ListUsers(ctx context.Context) ([]User, error) {
	body, err := c.request(ctx, "GET", "users", nil, "")
	if err != nil {
		return nil, err
	}
	var users []User
	if err := json.Unmarshal(body, &users); err != nil {
		return nil, err
	}
	return users, nil
}

// LinkLivePhotoToAsset associates an existing image asset with a motion video asset.
func (c *Client) LinkLivePhotoToAsset(ctx context.Context, assetId string, livePhotoVideoId string) error {
	if assetId == "" || livePhotoVideoId == "" {
		return fmt.Errorf("assetId and livePhotoVideoId are required")
	}

	payload := map[string]string{"livePhotoVideoId": livePhotoVideoId}
	jsonPayload, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal live photo link payload: %w", err)
	}

	_, err = c.request(ctx, "PUT", fmt.Sprintf("assets/%s", assetId), jsonPayload, "")
	if err != nil {
		return fmt.Errorf("failed to link live photo video to asset %s: %w", assetId, err)
	}

	return nil
}

type assetSearchItem struct {
	Id               string `json:"id"`
	OriginalFileName string `json:"originalFileName"`
	DeviceAssetId    string `json:"deviceAssetId"`
	Type             string `json:"type"`
	ExifInfo         struct {
		Description string `json:"description"`
	} `json:"exifInfo"`
}

type assetSearchResponse struct {
	Assets struct {
		Items      []assetSearchItem `json:"items"`
		NextPage   interface{}       `json:"nextPage"`
		NextCursor *string           `json:"nextCursor"`
	} `json:"assets"`
}

// SearchAssets fetches the assets uploaded by this tool. Immich v3 removed
// deviceId from the search API, so it first tries the filename filter
// ("gp_...") and falls back to the legacy deviceId search for older servers.
func (c *Client) SearchAssets(ctx context.Context, deviceId string) (*AssetIndex, error) {
	index, filtered, err := c.searchAssetsFiltered(ctx)
	if err == nil && filtered {
		return index, nil
	}

	legacy, legacyErr := c.searchAssetsLegacy(ctx, deviceId)
	if legacyErr == nil {
		return legacy, nil
	}
	if err != nil {
		return nil, fmt.Errorf("filtered search failed (%v); legacy search failed: %w", err, legacyErr)
	}
	return nil, legacyErr
}

// searchAssetsFiltered uses the Immich v3.2+ metadata filter. The bool result
// reports whether the server honoured the filter; older servers ignore it and
// return the whole library, in which case the caller must use the legacy path.
func (c *Client) searchAssetsFiltered(ctx context.Context) (*AssetIndex, bool, error) {
	index := NewAssetIndex()
	cursor := ""

	for page := 0; page < 10000; page++ {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}

		payload := map[string]interface{}{
			"filter": map[string]interface{}{
				"originalFileName": map[string]interface{}{"startsWith": "gp_"},
			},
			"size":     1000,
			"withExif": true,
		}
		if cursor != "" {
			payload["cursor"] = cursor
		}
		jsonPayload, err := json.Marshal(payload)
		if err != nil {
			return nil, false, fmt.Errorf("failed to marshal search payload: %w", err)
		}

		body, err := c.request(ctx, "POST", "search/metadata", jsonPayload, "")
		if err != nil {
			return nil, false, err
		}

		var resp assetSearchResponse
		if err := json.Unmarshal(body, &resp); err != nil {
			return nil, false, fmt.Errorf("failed to parse search response: %w", err)
		}

		if page == 0 {
			for _, asset := range resp.Assets.Items {
				if asset.OriginalFileName != "" && !strings.HasPrefix(asset.OriginalFileName, "gp_") {
					// The filter was ignored; this is an older Immich server.
					return nil, false, nil
				}
			}
		}

		for _, asset := range resp.Assets.Items {
			index.Add(asset.Id, asset.OriginalFileName, asset.Type, asset.DeviceAssetId)
			index.Descriptions[asset.Id] = asset.ExifInfo.Description
		}

		if resp.Assets.NextCursor != nil && *resp.Assets.NextCursor != "" {
			cursor = *resp.Assets.NextCursor
			continue
		}
		// A server that returns nextPage instead of nextCursor does not support
		// cursor pagination; let the legacy path handle it.
		if resp.Assets.NextPage != nil {
			return nil, false, nil
		}
		break
	}

	return index, true, nil
}

// searchAssetsLegacy uses the pre-v3 deviceId filter with page pagination.
func (c *Client) searchAssetsLegacy(ctx context.Context, deviceId string) (*AssetIndex, error) {
	index := NewAssetIndex()
	page := 1
	const pageSize = 1000

	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		payload := map[string]interface{}{
			"deviceId": deviceId,
			"page":     page,
			"size":     pageSize,
			"withExif": true,
		}
		jsonPayload, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal search payload: %w", err)
		}

		body, err := c.request(ctx, "POST", "search/metadata", jsonPayload, "")
		if err != nil {
			return nil, fmt.Errorf("search metadata failed on page %d: %w", page, err)
		}

		var resp assetSearchResponse
		if err := json.Unmarshal(body, &resp); err != nil {
			return nil, fmt.Errorf("failed to parse search response: %w", err)
		}

		if page == 1 && len(resp.Assets.Items) == 0 {
			var raw map[string]interface{}
			if json.Unmarshal(body, &raw) == nil {
				if _, hasAssets := raw["assets"]; !hasAssets && len(raw) > 0 {
					return nil, fmt.Errorf("unexpected search response format (missing 'assets' key)")
				}
			}
		}

		for _, asset := range resp.Assets.Items {
			index.Add(asset.Id, asset.OriginalFileName, asset.Type, asset.DeviceAssetId)
			index.Descriptions[asset.Id] = asset.ExifInfo.Description
		}

		nextPageEmpty := resp.Assets.NextPage == nil
		if np, ok := resp.Assets.NextPage.(string); ok && np == "" {
			nextPageEmpty = true
		}
		if nextPageEmpty || len(resp.Assets.Items) < pageSize {
			break
		}
		page++
	}

	return index, nil
}

// Asset is the subset of an Immich asset needed for description updates.
type Asset struct {
	Id               string    `json:"id"`
	OriginalFileName string    `json:"originalFileName"`
	Type             string    `json:"type"`
	HasMetadata      bool      `json:"hasMetadata"`
	LivePhotoVideoID string    `json:"livePhotoVideoId"`
	FileCreatedAt    time.Time `json:"fileCreatedAt"`
	Checksum         string    `json:"checksum"`
	OwnerID          string    `json:"ownerId"`
	ExifInfo         struct {
		Description string `json:"description"`
	} `json:"exifInfo"`
}

// GetAsset fetches a single asset.
func (c *Client) GetAsset(ctx context.Context, assetID string) (*Asset, error) {
	body, err := c.request(ctx, "GET", fmt.Sprintf("assets/%s", assetID), nil, "")
	if err != nil {
		return nil, err
	}
	var asset Asset
	if err := json.Unmarshal(body, &asset); err != nil {
		return nil, err
	}
	return &asset, nil
}

// DownloadAssetToFile streams an asset's original bytes to path. It uses a
// separate client without an overall timeout so large videos can finish.
func (c *Client) DownloadAssetToFile(ctx context.Context, assetID, path string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	url := fmt.Sprintf("%s/assets/%s/original", c.APIURL, assetID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("create download request: %w", err)
	}
	req.Header.Set("x-api-key", c.APIKey)

	resp, err := c.downloadClient.Do(req)
	if err != nil {
		return fmt.Errorf("download asset %s: %w", assetID, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("download asset %s returned %s: %s", assetID, resp.Status, strings.TrimSpace(string(body)))
	}

	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("create download directory: %w", err)
	}
	file, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create download file: %w", err)
	}
	_, copyErr := io.Copy(file, resp.Body)
	closeErr := file.Close()
	if copyErr != nil {
		_ = os.Remove(path)
		return fmt.Errorf("write asset %s: %w", assetID, copyErr)
	}
	if closeErr != nil {
		_ = os.Remove(path)
		return fmt.Errorf("close asset %s: %w", assetID, closeErr)
	}
	return nil
}

// UpdateAssetDescription replaces an asset's description. Immich v3 no longer
// accepts description on upload, so this is called after metadata extraction.
func (c *Client) UpdateAssetDescription(ctx context.Context, assetID, description string) error {
	payload, err := json.Marshal(map[string]string{"description": description})
	if err != nil {
		return fmt.Errorf("failed to marshal description payload: %w", err)
	}
	_, err = c.request(ctx, "PUT", fmt.Sprintf("assets/%s", assetID), payload, "")
	if err != nil {
		return fmt.Errorf("failed to update description for asset %s: %w", assetID, err)
	}
	return nil
}
