package googlephotos

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"time"
)

const userAgent = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"

const (
	maxRetries  = 5
	baseBackoff = 5 * time.Second
	minJitter   = 100
	jitterRange = 250
)

type Client struct {
	client *http.Client
	logger *slog.Logger
}

// NewClient creates a Google Photos HTTP client with connection pooling tuned to the given concurrency level
func NewClient(logger *slog.Logger, maxConnsPerHost int) *Client {
	if maxConnsPerHost < 10 {
		maxConnsPerHost = 10
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		logger.Warn("Failed to create cookie jar, continuing without cookies", "error", err)
	}
	return &Client{
		client: &http.Client{
			Jar: jar,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return nil
			},
			Transport: &http.Transport{
				MaxIdleConns:          100,
				MaxIdleConnsPerHost:   maxConnsPerHost,
				IdleConnTimeout:       90 * time.Second,
				ResponseHeaderTimeout: 60 * time.Second,
				TLSHandshakeTimeout:   15 * time.Second,
				ExpectContinueTimeout: time.Second,
				ForceAttemptHTTP2:     true,
			},
			// No overall client timeout: shared-album videos can be several
			// gigabytes and legitimately take longer than the old 120s limit.
			// Header/TLS timeouts above still bound connection setup, and the
			// sync context cancels downloads on shutdown.
			Timeout: 0,
		},
		logger: logger,
	}
}

// Get performs a GET request with retry logic
func (c *Client) Get(ctx context.Context, targetURL string) (*http.Response, error) {
	return c.doWithRetry(ctx, func(ctx context.Context) (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, "GET", targetURL, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", userAgent)
		return req, nil
	})
}

// Head performs a HEAD request with retry logic
func (c *Client) Head(ctx context.Context, targetURL string) (*http.Response, error) {
	return c.doWithRetry(ctx, func(ctx context.Context) (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, "HEAD", targetURL, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", userAgent)
		return req, nil
	})
}

// Post performs a POST request with retry logic and cookie/session support
func (c *Client) Post(ctx context.Context, targetURL string, contentType string, body string) (*http.Response, error) {
	return c.doWithRetry(ctx, func(ctx context.Context) (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, "POST", targetURL, strings.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", userAgent)
		req.Header.Set("Content-Type", contentType)
		return req, nil
	})
}

// doWithRetry executes a request with retries on network errors/429/5xx and exponential backoff.
// Jitter is only applied between retries, not before the first attempt.
func (c *Client) doWithRetry(ctx context.Context, makeReq func(context.Context) (*http.Request, error)) (*http.Response, error) {
	var lastErr error
	for i := 0; i < maxRetries; i++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		req, err := makeReq(ctx)
		if err != nil {
			return nil, err
		}

		resp, err := c.client.Do(req)
		if err != nil {
			lastErr = err
			sleepTime := baseBackoff * time.Duration(i+1)
			c.logger.Warn("Network error, retrying", "error", err, "sleep", sleepTime, "attempt", i+1)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(sleepTime):
			}
			continue
		}

		// Success or non-retryable client error (4xx except 429)
		if resp.StatusCode < 429 || (resp.StatusCode > 429 && resp.StatusCode < 500) {
			return resp, nil
		}

		// Retryable: 429 (rate limit) or 5xx (server error)
		resp.Body.Close()
		sleepTime := baseBackoff * time.Duration(i+1)
		if resp.StatusCode == 429 {
			if retryAfter := resp.Header.Get("Retry-After"); retryAfter != "" {
				if seconds, parseErr := time.ParseDuration(retryAfter + "s"); parseErr == nil {
					sleepTime = seconds
				}
			}
		}
		lastErr = fmt.Errorf("HTTP %d", resp.StatusCode)
		c.logger.Warn("Retryable HTTP error, retrying", "status", resp.StatusCode, "sleep", sleepTime, "attempt", i+1)

		// Add jitter only between retries
		jitter := time.Duration(minJitter+rand.Intn(jitterRange)) * time.Millisecond
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(sleepTime + jitter):
		}
	}

	return nil, fmt.Errorf("request failed after %d retries: %w", maxRetries, lastErr)
}
