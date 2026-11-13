package gphotosweb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func testCookies(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cookies.txt")
	content := "# Netscape HTTP Cookie File\n" +
		".google.com\tTRUE\t/\tTRUE\t9999999999\tSID\tsecret-sid\n" +
		"photos.google.com\tFALSE\t/\tTRUE\t9999999999\tSESSION\tsession-cookie\n"
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func testPage() string {
	return `<html><script>window.WIZ_global_data = {"SNlM0e":"at-token","FdrFJe":"fsid-token","cfb2h":"build-label","Im6cmf":"/_/PhotosUi"};</script></html>`
}

func rpcBody(rpcID string, data any) string {
	inner, _ := json.Marshal(data)
	entry := []any{"wrb.fr", rpcID, string(inner), nil, nil, nil, "generic"}
	chunk, _ := json.Marshal([]any{entry})
	return ")]}'\n\n" + strconv.Itoa(len(chunk)) + "\n" + string(chunk) + "\n"
}

func newTestClient(t *testing.T, handler func(w http.ResponseWriter, r *http.Request)) (*Client, *httptest.Server) {
	t.Helper()
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			fmt.Fprint(w, testPage())
			return
		}
		handler(w, r)
	}))
	t.Cleanup(server.Close)
	client := NewClient(testCookies(t))
	client.BaseURL = server.URL
	return client, server
}

func TestAvailable(t *testing.T) {
	client := NewClient(testCookies(t))
	if !client.Available() {
		t.Fatal("expected the cookies file to be available")
	}
	if NewClient(filepath.Join(t.TempDir(), "missing.txt")).Available() {
		t.Fatal("a missing file must not be available")
	}
}

func TestListAlbums(t *testing.T) {
	shared := []any{
		"AF1QipShared", []any{}, nil, nil, nil, nil,
		[]any{"cover"}, []any{}, nil, nil, []any{},
		map[string]any{"72930366": []any{4, "Leiding", []any{}, 672, true, "token", []any{}, []any{}, nil, nil, "https://photos.app.goo.gl/x"}},
	}
	owned := []any{
		"AF1QipOwned", []any{}, nil, nil, nil, nil,
		[]any{"cover"}, []any{}, nil, nil, []any{},
		map[string]any{"72930366": []any{4, "Mine", []any{}, 3, false, "token", []any{}, []any{}, nil, nil, nil}},
	}
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/_/PhotosUi/data/batchexecute" {
			http.NotFound(w, r)
			return
		}
		if r.FormValue("at") != "at-token" {
			t.Errorf("at token = %q", r.FormValue("at"))
		}
		if r.URL.Query().Get("f.sid") != "fsid-token" || r.URL.Query().Get("bl") != "build-label" {
			t.Errorf("query = %s", r.URL.RawQuery)
		}
		fmt.Fprint(w, rpcBody("Z5xsfc", []any{[]any{shared, owned}, nil}))
	})

	albums, err := client.ListAlbums(context.Background())
	if err != nil {
		t.Fatalf("ListAlbums failed: %v", err)
	}
	if len(albums) != 2 {
		t.Fatalf("albums = %#v", albums)
	}
	if albums[0].Key != "AF1QipShared" || albums[0].Title != "Leiding" || !albums[0].Shared || albums[0].Count != 672 {
		t.Fatalf("shared album = %#v", albums[0])
	}
	if albums[1].Key != "AF1QipOwned" || albums[1].Shared {
		t.Fatalf("owned album = %#v", albums[1])
	}
}

func TestAddItemsSharedPayload(t *testing.T) {
	var captured []any
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		freq := r.FormValue("f.req")
		var request []any
		if err := json.Unmarshal([]byte(freq), &request); err != nil {
			t.Fatalf("f.req: %v", err)
		}
		entry := request[0].([]any)[0].([]any)
		if entry[0] != "laUYf" {
			t.Fatalf("rpc = %v", entry[0])
		}
		var payload []any
		if err := json.Unmarshal([]byte(entry[1].(string)), &payload); err != nil {
			t.Fatalf("payload: %v", err)
		}
		captured = payload
		fmt.Fprint(w, rpcBody("laUYf", []any{"ig", nil, 0}))
	})

	err := client.AddItems(context.Background(), "AF1QipShared", true, []string{"media-1", "media-2"})
	if err != nil {
		t.Fatalf("AddItems failed: %v", err)
	}
	if len(captured) != 2 || captured[0] != "AF1QipShared" {
		t.Fatalf("payload = %#v", captured)
	}
	body := captured[1].([]any)
	if len(body) != 7 || body[0].(float64) != 2 {
		t.Fatalf("body = %#v", body)
	}
	items := body[2].([]any)
	if len(items) != 2 {
		t.Fatalf("items = %#v", items)
	}
	first := items[0].([]any)[0].([]any)
	if len(first) != 1 || first[0] != "media-1" {
		t.Fatalf("first item = %#v", items[0])
	}
}

func TestAddItemsOwnedPayload(t *testing.T) {
	var rpcID string
	var payload []any
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		var request []any
		_ = json.Unmarshal([]byte(r.FormValue("f.req")), &request)
		entry := request[0].([]any)[0].([]any)
		rpcID = entry[0].(string)
		_ = json.Unmarshal([]byte(entry[1].(string)), &payload)
		fmt.Fprint(w, rpcBody("E1Cajb", []any{}))
	})

	if err := client.AddItems(context.Background(), "AF1QipOwned", false, []string{"media-1"}); err != nil {
		t.Fatalf("AddItems failed: %v", err)
	}
	if rpcID != "E1Cajb" {
		t.Fatalf("rpc = %q", rpcID)
	}
	if len(payload) != 2 || payload[1] != "AF1QipOwned" {
		t.Fatalf("payload = %#v", payload)
	}
	if keys := payload[0].([]any); len(keys) != 1 || keys[0] != "media-1" {
		t.Fatalf("keys = %#v", payload[0])
	}
}

func TestListAlbumsExpiredSession(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "<html><body>Sign in</body></html>")
	}))
	defer server.Close()

	client := NewClient(testCookies(t))
	client.BaseURL = server.URL
	_, err := client.ListAlbums(context.Background())
	if !errors.Is(err, ErrNoSession) {
		t.Fatalf("error = %v, want ErrNoSession", err)
	}
}

func TestListAlbumsNotConfigured(t *testing.T) {
	client := NewClient(filepath.Join(t.TempDir(), "missing.txt"))
	_, err := client.ListAlbums(context.Background())
	if err == nil || !strings.Contains(err.Error(), ErrNotConfigured) {
		t.Fatalf("error = %v", err)
	}
}

func TestParseRPCError(t *testing.T) {
	entry := []any{"er", "laUYf", 7, "album not found"}
	chunk, _ := json.Marshal([]any{entry})
	body := ")]}'\n" + string(chunk) + "\n"
	_, err := parseRPCResponse([]byte(body), "laUYf")
	if err == nil || !strings.Contains(err.Error(), "album not found") {
		t.Fatalf("error = %v", err)
	}
}

func TestJarFromCookies(t *testing.T) {
	jar, err := jarFromCookies([]byte("# Netscape HTTP Cookie File\n" +
		".google.com\tTRUE\t/\tTRUE\t9999999999\tSID\tsecret\n" +
		"#HttpOnly_photos.google.com\tFALSE\t/\tTRUE\t9999999999\tSESSION\tvalue\n"))
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse("https://photos.google.com/")
	cookies := jar.Cookies(u)
	names := map[string]string{}
	for _, cookie := range cookies {
		names[cookie.Name] = cookie.Value
	}
	if names["SID"] != "secret" || names["SESSION"] != "value" {
		t.Fatalf("cookies = %#v", names)
	}
}

func TestJarRejectsMalformedLine(t *testing.T) {
	if _, err := jarFromCookies([]byte("not-a-cookie\n")); err == nil {
		t.Fatal("expected malformed cookies to fail")
	}
}

func TestValidateCookies(t *testing.T) {
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {})
	session, err := client.ValidateCookies(context.Background(), []byte(
		"# Netscape HTTP Cookie File\n.google.com\tTRUE\t/\tTRUE\t9999999999\tSID\tsecret\n"))
	if err != nil {
		t.Fatalf("ValidateCookies failed: %v", err)
	}
	if session.At != "at-token" || session.RPCPrefix != "/_/PhotosUi" {
		t.Fatalf("session = %#v", session)
	}
}
