package immich

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"time"

	"warreth.dev/immich-sync/pkg/util"
)

const (
	apiMaxRetries    = 3
	apiRetryDelay    = 2 * time.Second
	uploadMaxRetries = 3
	uploadRetryDelay = 3 * time.Second
)

type Album struct {
	AlbumName string `json:"albumName"`
	Id        string `json:"id"`
	Assets    []struct {
		Id               string `json:"id"`
		OriginalFileName string `json:"originalFileName"`
	} `json:"assets"`
}

type Client struct {
	APIURL string
	APIKey string
	client *http.Client
}

// NewClient creates an Immich API client with connection pooling tuned to the given concurrency level
func NewClient(apiURL, apiKey string, maxConnsPerHost int) *Client {
	if strings.HasSuffix(apiURL, "/") {
		apiURL = apiURL[:len(apiURL)-1]
	}
	if maxConnsPerHost < 20 {
		maxConnsPerHost = 20
	}
	return &Client{
		APIURL: apiURL,
		APIKey: apiKey,
		client: &http.Client{
			Transport: &http.Transport{
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: maxConnsPerHost,
				IdleConnTimeout:     90 * time.Second,
				ForceAttemptHTTP2:   true,
			},
			Timeout: 120 * time.Second,
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
		return respBody, res.StatusCode, fmt.Errorf("API error: %s - %s", res.Status, string(respBody))
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
	return c.UploadAssetWithLive(ctx, data, filename, createdAt, description, "")
}

// UploadAssetWithLive uploads an asset and optionally links it to a live photo video, with retry
func (c *Client) UploadAssetWithLive(ctx context.Context, data []byte, filename string, createdAt time.Time, description string, livePhotoVideoId string) (string, bool, error) {
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

		id, isDup, err := c.doUpload(ctx, data, filename, createdAt, description, livePhotoVideoId)
		if err == nil {
			return id, isDup, nil
		}
		lastErr = err
	}
	return "", false, fmt.Errorf("upload of %s failed after %d retries: %w", filename, uploadMaxRetries, lastErr)
}

// doUpload performs a single multipart upload attempt
func (c *Client) doUpload(ctx context.Context, data []byte, filename string, createdAt time.Time, description string, livePhotoVideoId string) (string, bool, error) {
	pr, pw := io.Pipe()
	multipartWriter := multipart.NewWriter(pw)

	go func() {
		defer pw.Close()
		defer multipartWriter.Close()

		_ = multipartWriter.WriteField("deviceAssetId", filename)
		_ = multipartWriter.WriteField("deviceId", "immich-sync-go")

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

		part, err := multipartWriter.CreateFormFile("assetData", filename)
		if err != nil {
			pw.CloseWithError(err)
			return
		}
		if _, err := io.Copy(part, bytes.NewReader(data)); err != nil {
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

// SearchAssetsByDevice fetches all assets uploaded by the given deviceId.
// Returns a map of baseName (without extension) -> asset ID.
func (c *Client) SearchAssetsByDevice(ctx context.Context, deviceId string) (map[string]string, error) {
	result := make(map[string]string)
	page := 1
	pageSize := 1000

	for {
		if err := ctx.Err(); err != nil {
			return result, err
		}

		payload := map[string]interface{}{
			"deviceId": deviceId,
			"page":     page,
			"size":     pageSize,
		}
		jsonPayload, err := json.Marshal(payload)
		if err != nil {
			return result, fmt.Errorf("failed to marshal search payload: %w", err)
		}

		body, err := c.request(ctx, "POST", "search/metadata", jsonPayload, "")
		if err != nil {
			return result, fmt.Errorf("search metadata failed on page %d: %w", page, err)
		}

		var searchResp struct {
			Assets struct {
				Items []struct {
					Id               string `json:"id"`
					OriginalFileName string `json:"originalFileName"`
					DeviceAssetId    string `json:"deviceAssetId"`
				} `json:"items"`
				NextPage interface{} `json:"nextPage"`
			} `json:"assets"`
		}
		if err := json.Unmarshal(body, &searchResp); err != nil {
			return result, fmt.Errorf("failed to parse search response: %w", err)
		}

		if page == 1 && len(searchResp.Assets.Items) == 0 {
			var raw map[string]interface{}
			if json.Unmarshal(body, &raw) == nil {
				if _, hasAssets := raw["assets"]; !hasAssets && len(raw) > 0 {
					return result, fmt.Errorf("unexpected search response format (missing 'assets' key)")
				}
			}
		}

		for _, asset := range searchResp.Assets.Items {
			name := util.StripExtension(asset.OriginalFileName)
			result[name] = asset.Id

			// Also index by deviceAssetId to handle old format "gp_xxx.jpg-12345"
			if asset.DeviceAssetId != "" {
				// Strip numeric size suffix from old-format deviceAssetId
				daid := asset.DeviceAssetId
				if dashIdx := strings.LastIndex(daid, "-"); dashIdx > 0 {
					suffix := daid[dashIdx+1:]
					allDigits := true
					for _, c := range suffix {
						if c < '0' || c > '9' {
							allDigits = false
							break
						}
					}
					if allDigits && len(suffix) > 0 {
						daid = daid[:dashIdx]
					}
				}
				daidBase := util.StripExtension(daid)
				if _, exists := result[daidBase]; !exists {
					result[daidBase] = asset.Id
				}
			}
		}

		// Stop if no more pages
		nextPageEmpty := searchResp.Assets.NextPage == nil
		if np, ok := searchResp.Assets.NextPage.(string); ok && np == "" {
			nextPageEmpty = true
		}
		if nextPageEmpty || len(searchResp.Assets.Items) < pageSize {
			break
		}
		page++
	}

	return result, nil
}
