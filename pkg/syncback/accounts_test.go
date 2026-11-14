package syncback

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"warreth.dev/gphotos2immich/pkg/config"
	"warreth.dev/gphotos2immich/pkg/gphotosweb"
)

// countingWeb records how often the session is validated, so the tests can
// prove the account page reuses a recent result.
type countingWeb struct {
	*fakeWeb
	validates int
}

func (w *countingWeb) Validate(ctx context.Context) (gphotosweb.Session, error) {
	w.validates++
	return w.fakeWeb.Validate(ctx)
}

// immichServer serves the Immich endpoints the account flow uses. An empty
// wantKey accepts any key; a non-empty one answers 401 for other keys.
func immichServer(t *testing.T, wantKey, me, users string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if wantKey != "" && r.Header.Get("x-api-key") != wantKey {
			http.Error(w, `{"message":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/users/me":
			_, _ = w.Write([]byte(me))
		case "/users":
			_, _ = w.Write([]byte(users))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// unusedWebFactory fails the test when a test expects no per-account client.
func unusedWebFactory(t *testing.T) WebClientFactory {
	t.Helper()
	return func(path string) WebClient {
		t.Fatalf("web factory must not be called, got path %q", path)
		return nil
	}
}

func TestSetAccountsNormalizesAndCopies(t *testing.T) {
	service := New(Options{Google: &fakeGoogle{}})
	service.SetAccounts([]config.AccountMapping{{
		ImmichUserID: " owner-1 ",
		Name:         " One ",
		GoogleEmail:  " One@Gmail.com ",
	}}, true)

	accounts, strict := service.accountsSnapshot()
	if !strict {
		t.Fatal("the strict flag was not stored")
	}
	if len(accounts) != 1 {
		t.Fatalf("accounts = %#v", accounts)
	}
	acc := accounts[0]
	if acc.ID != "owner-1" || acc.Name != "One" || acc.GoogleEmail != "One@Gmail.com" {
		t.Fatalf("mapping was not normalized: %#v", acc)
	}
	if acc.WebCookiesFile != "./data/gphotos-web-cookies-owner-1.txt" {
		t.Fatalf("cookies path = %q", acc.WebCookiesFile)
	}

	// A snapshot is a copy, so callers cannot mutate the service's state.
	accounts[0].Name = "changed"
	again, _ := service.accountsSnapshot()
	if again[0].Name != "One" {
		t.Fatal("accountsSnapshot must return a copy")
	}
}

func TestResolveOwnerWithoutAccounts(t *testing.T) {
	service := New(Options{Google: &fakeGoogle{}})
	route, ok, fallback := service.ResolveOwner("owner-1", true)
	if !ok || fallback {
		t.Fatalf("ok = %v, fallback = %v", ok, fallback)
	}
	if route.GoogleEmail != "" || route.SharedWriter != nil || route.Inspector != nil {
		t.Fatalf("route = %#v", route)
	}
}

func TestResolveOwnerMapsConfiguredOwner(t *testing.T) {
	factoryCalls := 0
	service := New(Options{
		Google: &fakeGoogle{},
		WebFactory: func(string) WebClient {
			factoryCalls++
			return &fakeWeb{available: true}
		},
	})
	service.SetAccounts([]config.AccountMapping{
		{ID: "acc-lotte", ImmichUserID: "owner-lotte", GoogleEmail: "lotte@gmail.com"},
	}, false)

	route, ok, fallback := service.ResolveOwner(" OWNER-LOTTE ", true)
	if !ok || fallback {
		t.Fatalf("ok = %v, fallback = %v", ok, fallback)
	}
	if route.GoogleEmail != "lotte@gmail.com" {
		t.Fatalf("route = %#v", route)
	}
	if route.SharedWriter == nil || route.Inspector == nil {
		t.Fatalf("the mapped account must carry its own web session: %#v", route)
	}
	if factoryCalls != 1 {
		t.Fatalf("factory calls = %d", factoryCalls)
	}
}

func TestResolveOwnerFallsBackWhenNotStrict(t *testing.T) {
	service := New(Options{
		Google:     &fakeGoogle{},
		WebFactory: func(string) WebClient { return &fakeWeb{available: true} },
	})
	service.SetAccounts([]config.AccountMapping{
		{ID: "acc-1", ImmichUserID: "owner-1", GoogleEmail: "one@gmail.com", IsDefault: true},
		{ID: "acc-2", ImmichUserID: "owner-2", GoogleEmail: "two@gmail.com"},
	}, false)

	route, ok, fallback := service.ResolveOwner("owner-3", false)
	if !ok || !fallback {
		t.Fatalf("ok = %v, fallback = %v", ok, fallback)
	}
	if route.GoogleEmail != "one@gmail.com" {
		t.Fatalf("route = %#v", route)
	}
}

func TestResolveOwnerFallsBackToPrimaryWithoutDefault(t *testing.T) {
	service := New(Options{
		Google:     &fakeGoogle{},
		WebFactory: func(string) WebClient { return &fakeWeb{available: true} },
	})
	service.SetAccounts([]config.AccountMapping{
		{ID: "acc-1", ImmichUserID: "owner-1", GoogleEmail: "one@gmail.com"},
	}, false)

	route, ok, fallback := service.ResolveOwner("owner-3", false)
	if !ok || !fallback {
		t.Fatalf("ok = %v, fallback = %v", ok, fallback)
	}
	if route.GoogleEmail != "" || route.SharedWriter != nil {
		t.Fatalf("without a default the primary account takes over: %#v", route)
	}
}

func TestResolveOwnerSkipsWhenStrict(t *testing.T) {
	service := New(Options{Google: &fakeGoogle{}})
	service.SetAccounts([]config.AccountMapping{
		{ID: "acc-1", ImmichUserID: "owner-1", GoogleEmail: "one@gmail.com", IsDefault: true},
	}, true)

	if _, ok, _ := service.ResolveOwner("owner-3", true); ok {
		t.Fatal("strict matching must skip an unmapped owner")
	}
}

func TestRouteForEmail(t *testing.T) {
	service := New(Options{
		Google:     &fakeGoogle{},
		WebFactory: func(string) WebClient { return &fakeWeb{available: true} },
	})
	service.SetAccounts([]config.AccountMapping{
		{ID: "acc-1", ImmichUserID: "owner-1", GoogleEmail: "one@gmail.com"},
	}, false)

	route, ok := service.RouteForEmail(" ONE@Gmail.com ")
	if !ok || route.GoogleEmail != "one@gmail.com" {
		t.Fatalf("route = %#v, ok = %v", route, ok)
	}
	if _, ok := service.RouteForEmail("   "); ok {
		t.Fatal("an empty email must not resolve")
	}
	if _, ok := service.RouteForEmail("nobody@example.com"); ok {
		t.Fatal("an unknown email must not resolve")
	}
}

func TestWebClientForReusesPrimarySession(t *testing.T) {
	primary := &fakeWeb{available: true}
	service := New(Options{
		Google:         &fakeGoogle{},
		Web:            primary,
		WebCookiesPath: "/tmp/gphotos-test-primary.txt",
		WebFactory:     unusedWebFactory(t),
	})
	service.setStatus(AccountStatus{Connected: true, Email: "one@gmail.com"})

	acc := config.NormalizeAccount(config.AccountMapping{
		ID: "acc-1", ImmichUserID: "owner-1", GoogleEmail: "one@gmail.com",
	})
	if got := service.webClientFor(acc); got != WebClient(primary) {
		t.Fatalf("primary account must reuse the primary session, got %#v", got)
	}
	path, isPrimary := service.sessionPath(acc)
	if !isPrimary || path != "/tmp/gphotos-test-primary.txt" {
		t.Fatalf("sessionPath = %q, primary = %v", path, isPrimary)
	}
	if reported := service.accountCookiesPath(acc); reported != path {
		t.Fatalf("accountCookiesPath = %q", reported)
	}
	if service.primaryAccountEmail("   ") {
		t.Fatal("an empty email cannot match the primary account")
	}
	if service.primaryAccountEmail("two@gmail.com") {
		t.Fatal("another account must not match the primary account")
	}
}

func TestWebClientForCreatesAndCachesPerAccountClient(t *testing.T) {
	var paths []string
	service := New(Options{
		Google:         &fakeGoogle{},
		Web:            &fakeWeb{available: true},
		WebCookiesPath: "/tmp/gphotos-test-primary.txt",
		WebFactory: func(path string) WebClient {
			paths = append(paths, path)
			return &fakeWeb{available: true}
		},
	})
	service.setStatus(AccountStatus{Connected: true, Email: "one@gmail.com"})

	acc := config.NormalizeAccount(config.AccountMapping{
		ID: "acc-2", ImmichUserID: "owner-2", GoogleEmail: "two@gmail.com",
	})
	first := service.webClientFor(acc)
	second := service.webClientFor(acc)
	if first != second {
		t.Fatal("the per-account client must be cached")
	}
	if len(paths) != 1 || paths[0] != "./data/gphotos-web-cookies-acc-2.txt" {
		t.Fatalf("factory paths = %#v", paths)
	}
}

func TestWebClientForHonoursExplicitCookiesPath(t *testing.T) {
	primary := &fakeWeb{available: true}
	service := New(Options{
		Google:         &fakeGoogle{},
		Web:            primary,
		WebCookiesPath: "/tmp/gphotos-test-primary.txt",
	})
	service.setStatus(AccountStatus{Connected: true, Email: "one@gmail.com"})

	// An explicit path that is the primary file still reuses that session.
	acc := config.NormalizeAccount(config.AccountMapping{
		ID: "acc-1", ImmichUserID: "owner-1", GoogleEmail: "one@gmail.com",
		WebCookiesFile: "/tmp/gphotos-test-primary.txt",
	})
	if got := service.webClientFor(acc); got != WebClient(primary) {
		t.Fatalf("explicit primary path must reuse the primary session: %#v", got)
	}
}

func TestWebClientForExplicitPathBeatsPrimaryEmail(t *testing.T) {
	var created []string
	service := New(Options{
		Google:         &fakeGoogle{},
		Web:            &fakeWeb{available: true},
		WebCookiesPath: "/tmp/gphotos-test-primary.txt",
		WebFactory: func(path string) WebClient {
			created = append(created, path)
			return &fakeWeb{available: true}
		},
	})
	service.setStatus(AccountStatus{Connected: true, Email: "one@gmail.com"})

	acc := config.NormalizeAccount(config.AccountMapping{
		ID: "acc-1", ImmichUserID: "owner-1", GoogleEmail: "one@gmail.com",
		WebCookiesFile: "/tmp/gphotos-test-other.txt",
	})
	if got := service.webClientFor(acc); got == WebClient(service.Web) {
		t.Fatal("an explicit cookies file must not reuse the primary session")
	}
	if len(created) != 1 || created[0] != "/tmp/gphotos-test-other.txt" {
		t.Fatalf("factory paths = %#v", created)
	}
}

func TestAccountMappingsReportsStatus(t *testing.T) {
	server := immichServer(t, "key-1", `{"id":"owner-1","name":"Warre"}`, `[]`)
	cookiesPath := filepath.Join(t.TempDir(), "primary.txt")
	web := &countingWeb{fakeWeb: &fakeWeb{available: true}}
	service := New(Options{
		Google:         &fakeGoogle{accounts: []string{"one@gmail.com"}},
		Web:            web,
		WebCookiesPath: cookiesPath,
		ImmichURL:      server.URL,
		Accounts: []config.AccountMapping{{
			ID: "acc-1", ImmichUserID: "owner-1", ImmichAPIKey: "key-1",
			GoogleEmail: "one@gmail.com",
		}},
	})
	service.setStatus(AccountStatus{Connected: true, Email: "one@gmail.com"})

	items := service.AccountMappings(context.Background())
	if len(items) != 1 {
		t.Fatalf("items = %#v", items)
	}
	item := items[0]
	if !item.GoogleConnected || item.GoogleError != "" {
		t.Fatalf("google status = %#v", item)
	}
	if !item.ImmichKeyOK || item.ImmichUserName != "Warre" || item.ImmichError != "" {
		t.Fatalf("immich status = %#v", item)
	}
	if !item.CookiesSaved || !item.WebSessionOK || item.WebSessionError != "" {
		t.Fatalf("web status = %#v", item)
	}
	if item.CookiesFile != cookiesPath {
		t.Fatalf("cookies file = %q", item.CookiesFile)
	}
	if web.validates != 1 {
		t.Fatalf("validations = %d", web.validates)
	}

	// A recent result is reused, so opening the page again is cheap.
	service.AccountMappings(context.Background())
	if web.validates != 1 {
		t.Fatalf("the web check was not cached: validations = %d", web.validates)
	}
}

func TestAccountMappingsReportsExpiredWebSession(t *testing.T) {
	service := New(Options{
		Google:     &fakeGoogle{},
		WebFactory: func(string) WebClient { return &fakeWeb{available: true, validateErr: gphotosweb.ErrNoSession} },
	})
	service.SetAccounts([]config.AccountMapping{
		{ImmichUserID: "owner-1", GoogleEmail: "one@gmail.com"},
	}, false)

	item := service.AccountMappings(context.Background())[0]
	if !item.CookiesSaved {
		t.Fatal("an existing cookies file must be reported as saved")
	}
	if item.WebSessionOK || item.WebSessionError == "" {
		t.Fatalf("web status = %#v", item)
	}
}

func TestAccountMappingsWithoutGoogleReportsError(t *testing.T) {
	service := New(Options{Web: &fakeWeb{}})
	service.SetAccounts([]config.AccountMapping{{ImmichUserID: "owner-1"}}, false)

	item := service.AccountMappings(context.Background())[0]
	if item.GoogleError == "" {
		t.Fatal("a missing Google client must be reported")
	}
	if item.GoogleConnected {
		t.Fatal("a missing Google client must not report connected")
	}
	if item.CookiesSaved {
		t.Fatal("an unavailable browser session must not report saved cookies")
	}
}

func TestAccountMappingsImmichKeyFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"message":"unauthorized"}`, http.StatusUnauthorized)
	}))
	t.Cleanup(server.Close)
	service := New(Options{Google: &fakeGoogle{}, ImmichURL: server.URL})
	service.SetAccounts([]config.AccountMapping{
		{ImmichUserID: "owner-1", ImmichAPIKey: "bad"},
	}, false)

	item := service.AccountMappings(context.Background())[0]
	if item.ImmichKeyOK || item.ImmichError == "" {
		t.Fatalf("immich status = %#v", item)
	}
}

func TestSaveAccountCreatesMapping(t *testing.T) {
	server := immichServer(t, "key-1", `{"id":"owner-1","name":"Warre"}`, `[]`)
	service := New(Options{
		Google:     &fakeGoogle{accounts: []string{"one@gmail.com"}},
		ImmichURL:  server.URL,
		WebFactory: func(string) WebClient { return &fakeWeb{available: true} },
	})

	status, err := service.SaveAccount(context.Background(), AccountInput{
		ImmichUserID: " owner-1 ",
		Name:         " One ",
		ImmichAPIKey: " key-1 ",
		GoogleEmail:  " one@gmail.com ",
	})
	if err != nil {
		t.Fatalf("SaveAccount failed: %v", err)
	}
	if status.ID != "owner-1" {
		t.Fatalf("id = %q", status.ID)
	}
	if !status.ImmichKeyOK || status.ImmichUserName != "Warre" {
		t.Fatalf("immich status = %#v", status)
	}
	if !status.GoogleConnected || !status.CookiesSaved || !status.WebSessionOK {
		t.Fatalf("status = %#v", status)
	}

	accounts, strict := service.accountsSnapshot()
	if strict {
		t.Fatal("strict flag changed")
	}
	if len(accounts) != 1 {
		t.Fatalf("accounts = %#v", accounts)
	}
	if accounts[0].Name != "One" || accounts[0].GoogleEmail != "one@gmail.com" {
		t.Fatalf("saved mapping = %#v", accounts[0])
	}
}

func TestSaveAccountRequiresImmichUser(t *testing.T) {
	service := New(Options{Google: &fakeGoogle{}})
	if _, err := service.SaveAccount(context.Background(), AccountInput{Name: "One"}); err == nil {
		t.Fatal("an account without immichUserId must be rejected")
	}
}

func TestSaveAccountUpdatesExistingMapping(t *testing.T) {
	server := immichServer(t, "key-1", `{"id":"owner-1","name":"Warre"}`, `[]`)
	service := New(Options{
		Google:     &fakeGoogle{},
		ImmichURL:  server.URL,
		WebFactory: func(string) WebClient { return &fakeWeb{available: true} },
	})
	service.SetAccounts([]config.AccountMapping{{
		ID: "acc-1", ImmichUserID: "owner-1", Name: "Old",
		ImmichAPIKey: "key-1", GoogleEmail: "one@gmail.com",
		WebCookiesFile: "/tmp/gphotos-test-one.txt",
	}}, false)

	// Only the edited fields are sent, so the merge keeps the stored id, email,
	// key and cookies file instead of erasing them.
	status, err := service.SaveAccount(context.Background(), AccountInput{
		ImmichUserID: "owner-1",
		Name:         "New",
	})
	if err != nil {
		t.Fatalf("SaveAccount failed: %v", err)
	}
	if status.ID != "acc-1" {
		t.Fatalf("id = %q", status.ID)
	}
	if status.Name != "New" || status.GoogleEmail != "one@gmail.com" || status.ImmichAPIKey != "key-1" {
		t.Fatalf("merged mapping = %#v", status.AccountMapping)
	}
	if status.WebCookiesFile != "/tmp/gphotos-test-one.txt" {
		t.Fatalf("cookies path = %q", status.WebCookiesFile)
	}
	if status.CookiesFile != "/tmp/gphotos-test-one.txt" {
		t.Fatalf("reported cookies file = %q", status.CookiesFile)
	}

	accounts, _ := service.accountsSnapshot()
	if len(accounts) != 1 {
		t.Fatalf("an update must not add a mapping: %#v", accounts)
	}
}

func TestSaveAccountConnectsOAuthToken(t *testing.T) {
	google := &fakeGoogle{accounts: []string{"user@example.com"}}
	service := New(Options{
		Google:     google,
		WebFactory: func(string) WebClient { return &fakeWeb{available: true} },
	})

	status, err := service.SaveAccount(context.Background(), AccountInput{
		ImmichUserID: "owner-1",
		OAuthToken:   " token-123 ",
	})
	if err != nil {
		t.Fatalf("SaveAccount failed: %v", err)
	}
	if google.added != "token-123" {
		t.Fatalf("token passed = %q", google.added)
	}
	if status.GoogleEmail != "user@example.com" {
		t.Fatalf("google email = %q", status.GoogleEmail)
	}
	if !status.GoogleConnected {
		t.Fatalf("status = %#v", status)
	}
}

func TestSaveAccountRejectsMismatchedOAuthToken(t *testing.T) {
	service := New(Options{Google: &fakeGoogle{}, WebFactory: func(string) WebClient { return &fakeWeb{} }})

	_, err := service.SaveAccount(context.Background(), AccountInput{
		ImmichUserID: "owner-1",
		GoogleEmail:  "other@example.com",
		OAuthToken:   "token-123",
	})
	if err == nil || !strings.Contains(err.Error(), "belongs to") {
		t.Fatalf("err = %v", err)
	}
	accounts, _ := service.accountsSnapshot()
	if len(accounts) != 0 {
		t.Fatalf("a rejected token must not save the mapping: %#v", accounts)
	}
}

func TestSaveAccountRequiresGoogleForOAuthToken(t *testing.T) {
	service := New(Options{})
	_, err := service.SaveAccount(context.Background(), AccountInput{
		ImmichUserID: "owner-1",
		OAuthToken:   "token-123",
	})
	if !errors.Is(err, ErrDisabled) {
		t.Fatalf("err = %v", err)
	}
}

func TestSaveAccountRejectsBadImmichKey(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"message":"unauthorized"}`, http.StatusUnauthorized)
	}))
	t.Cleanup(server.Close)
	service := New(Options{Google: &fakeGoogle{}, ImmichURL: server.URL})

	_, err := service.SaveAccount(context.Background(), AccountInput{
		ImmichUserID: "owner-1",
		ImmichAPIKey: "bad",
	})
	if err == nil || !strings.Contains(err.Error(), "Immich API key test failed") {
		t.Fatalf("err = %v", err)
	}
	accounts, _ := service.accountsSnapshot()
	if len(accounts) != 0 {
		t.Fatalf("a failed key test must not save the mapping: %#v", accounts)
	}
}

func TestSaveAccountStoresCookies(t *testing.T) {
	path := filepath.Join(t.TempDir(), "one.txt")
	web := &fakeWeb{available: true}
	service := New(Options{
		Google:         &fakeGoogle{accounts: []string{"one@gmail.com"}},
		Web:            web,
		WebCookiesPath: path,
	})

	status, err := service.SaveAccount(context.Background(), AccountInput{
		ImmichUserID:   "owner-1",
		GoogleEmail:    "one@gmail.com",
		WebCookiesFile: path,
		Cookies:        " cookie-line ",
	})
	if err != nil {
		t.Fatalf("SaveAccount failed: %v", err)
	}
	if !status.CookiesSaved || !status.WebSessionOK {
		t.Fatalf("status = %#v", status)
	}
	if web.reloads != 1 {
		t.Fatalf("reloads = %d", web.reloads)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "cookie-line\n" {
		t.Fatalf("file = %q", content)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("mode = %o", info.Mode().Perm())
	}
}

func TestSaveAccountRejectsBadCookies(t *testing.T) {
	path := filepath.Join(t.TempDir(), "one.txt")
	service := New(Options{
		Google:     &fakeGoogle{},
		WebFactory: func(string) WebClient { return &fakeWeb{available: true, validateCookiesErr: gphotosweb.ErrNoSession} },
	})

	_, err := service.SaveAccount(context.Background(), AccountInput{
		ImmichUserID:   "owner-1",
		WebCookiesFile: path,
		Cookies:        "cookie-line",
	})
	if err == nil || !strings.Contains(err.Error(), "browser session test failed") {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("invalid cookies must not be written")
	}
	accounts, _ := service.accountsSnapshot()
	if len(accounts) != 0 {
		t.Fatalf("invalid cookies must not save the mapping: %#v", accounts)
	}
}

func TestSaveAccountReportsCookiesWriteFailure(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	service := New(Options{
		Google:     &fakeGoogle{},
		WebFactory: func(string) WebClient { return &fakeWeb{available: true} },
	})

	_, err := service.SaveAccount(context.Background(), AccountInput{
		ImmichUserID:   "owner-1",
		WebCookiesFile: filepath.Join(blocker, "cookies.txt"),
		Cookies:        "cookie-line",
	})
	if err == nil {
		t.Fatal("a cookies file that cannot be written must fail the save")
	}
	if accounts, _ := service.accountsSnapshot(); len(accounts) != 0 {
		t.Fatalf("accounts = %#v", accounts)
	}
}

func TestSaveAccountKeepsSingleDefault(t *testing.T) {
	service := New(Options{Google: &fakeGoogle{}, WebFactory: func(string) WebClient { return &fakeWeb{available: true} }})

	if _, err := service.SaveAccount(context.Background(), AccountInput{
		ImmichUserID: "owner-1", GoogleEmail: "one@gmail.com", IsDefault: true,
	}); err != nil {
		t.Fatalf("first account: %v", err)
	}
	if _, err := service.SaveAccount(context.Background(), AccountInput{
		ImmichUserID: "owner-2", GoogleEmail: "two@gmail.com", IsDefault: true,
	}); err != nil {
		t.Fatalf("second account: %v", err)
	}

	accounts, _ := service.accountsSnapshot()
	if len(accounts) != 2 {
		t.Fatalf("accounts = %#v", accounts)
	}
	if accounts[0].IsDefault || !accounts[1].IsDefault {
		t.Fatalf("only the latest account may stay default: %#v", accounts)
	}
}

func TestDeleteAccount(t *testing.T) {
	service := New(Options{Google: &fakeGoogle{}})
	service.SetAccounts([]config.AccountMapping{
		{ID: "acc-1", ImmichUserID: "owner-1"},
		{ID: "acc-2", ImmichUserID: "owner-2"},
	}, true)

	if err := service.DeleteAccount(context.Background(), " ACC-1 "); err != nil {
		t.Fatalf("DeleteAccount failed: %v", err)
	}
	accounts, strict := service.accountsSnapshot()
	if !strict {
		t.Fatal("the strict flag must survive a delete")
	}
	if len(accounts) != 1 || accounts[0].ID != "acc-2" {
		t.Fatalf("accounts = %#v", accounts)
	}

	// An Immich user id also identifies the mapping.
	if err := service.DeleteAccount(context.Background(), "owner-2"); err != nil {
		t.Fatalf("DeleteAccount by owner failed: %v", err)
	}
	if accounts, _ := service.accountsSnapshot(); len(accounts) != 0 {
		t.Fatalf("accounts = %#v", accounts)
	}
}

func TestDeleteAccountUnknown(t *testing.T) {
	service := New(Options{Google: &fakeGoogle{}})
	if err := service.DeleteAccount(context.Background(), "nope"); !errors.Is(err, ErrAccountNotFound) {
		t.Fatalf("err = %v", err)
	}
	if err := service.DeleteAccount(context.Background(), "  "); err == nil {
		t.Fatal("an empty id must be rejected")
	}
}

func TestRoutingPreviewFromServiceAccounts(t *testing.T) {
	service := New(Options{Google: &fakeGoogle{}})
	service.SetAccounts([]config.AccountMapping{
		{ID: "acc-2", Name: "Lotte", ImmichUserID: "owner-2", GoogleEmail: "lotte@gmail.com"},
		{ID: "acc-1", Name: "Warre", ImmichUserID: "owner-1", GoogleEmail: "warre@gmail.com", IsDefault: true},
	}, false)

	preview := service.RoutingPreview(context.Background(), nil)
	if preview.Strict {
		t.Fatal("strict = true")
	}
	if preview.Unmapped != "fallback" {
		t.Fatalf("unmapped = %q", preview.Unmapped)
	}
	if preview.Fallback != "warre@gmail.com" {
		t.Fatalf("fallback = %q", preview.Fallback)
	}
	if len(preview.Accounts) != 2 || preview.Accounts[0].ID != "acc-1" || preview.Accounts[1].ID != "acc-2" {
		t.Fatalf("accounts = %#v", preview.Accounts)
	}
	if preview.ImmichError != "" {
		t.Fatalf("immich error = %q", preview.ImmichError)
	}
	if len(preview.ImmichUsers) != 0 {
		t.Fatalf("without credentials no users are listed: %#v", preview.ImmichUsers)
	}
}

func TestRoutingPreviewWithoutDefaultUsesPrimary(t *testing.T) {
	service := New(Options{Google: &fakeGoogle{}})
	service.SetAccounts([]config.AccountMapping{
		{ID: "acc-1", ImmichUserID: "owner-1", GoogleEmail: "one@gmail.com"},
	}, false)

	preview := service.RoutingPreview(context.Background(), nil)
	if preview.Fallback != "primary" {
		t.Fatalf("fallback = %q", preview.Fallback)
	}
}

func TestRoutingPreviewFromConfigWithUsers(t *testing.T) {
	server := immichServer(t, "cfg-key", `{"id":"owner-1","name":"Warre"}`, `[
		{"id":"owner-1","name":"Warre","email":"warre@example.com"},
		{"id":"owner-2","name":"Lotte","email":"lotte@example.com"}
	]`)
	service := New(Options{Google: &fakeGoogle{}, ImmichURL: "http://127.0.0.1:1/api"})
	service.SetAccounts([]config.AccountMapping{
		{ID: "stale", ImmichUserID: "someone", GoogleEmail: "stale@gmail.com", IsDefault: true},
	}, false)

	cfg := &config.Config{
		ApiKey: "cfg-key",
		ApiURL: server.URL,
		Accounts: []config.AccountMapping{{
			ID: "acc-1", Name: "Warre", ImmichUserID: "owner-1", GoogleEmail: "warre@gmail.com",
		}},
		GoogleUpload: config.GoogleUploadConfig{StrictAccountMatching: true},
	}

	preview := service.RoutingPreview(context.Background(), cfg)
	if !preview.Strict {
		t.Fatal("the config strict flag must win")
	}
	if preview.Unmapped != "skip" {
		t.Fatalf("unmapped = %q", preview.Unmapped)
	}
	if preview.ImmichError != "" {
		t.Fatalf("immich error = %q", preview.ImmichError)
	}
	if len(preview.Accounts) != 1 || preview.Accounts[0].ID != "acc-1" {
		t.Fatalf("accounts = %#v", preview.Accounts)
	}
	if len(preview.ImmichUsers) != 2 {
		t.Fatalf("users = %#v", preview.ImmichUsers)
	}
	mapped := preview.ImmichUsers[0]
	if mapped.Status != "mapped" || mapped.AccountID != "acc-1" || mapped.GoogleEmail != "warre@gmail.com" {
		t.Fatalf("mapped user = %#v", mapped)
	}
	skipped := preview.ImmichUsers[1]
	if skipped.Status != "skipped" || skipped.AccountID != "" || skipped.GoogleEmail != "" {
		t.Fatalf("unmapped user = %#v", skipped)
	}
}

func TestRoutingPreviewRoutesUnmappedUserToDefault(t *testing.T) {
	server := immichServer(t, "cfg-key", `{}`, `[
		{"id":"owner-1","name":"Warre","email":"warre@example.com"},
		{"id":"owner-2","name":"Lotte","email":"lotte@example.com"}
	]`)
	service := New(Options{Google: &fakeGoogle{}})

	cfg := &config.Config{
		ApiKey: "cfg-key",
		ApiURL: server.URL,
		Accounts: []config.AccountMapping{{
			ID: "acc-1", ImmichUserID: "owner-1", GoogleEmail: "warre@gmail.com", IsDefault: true,
		}},
	}

	preview := service.RoutingPreview(context.Background(), cfg)
	if preview.Strict {
		t.Fatal("strict = true")
	}
	if preview.Unmapped != "fallback" || preview.Fallback != "warre@gmail.com" {
		t.Fatalf("preview = %#v", preview)
	}
	if len(preview.ImmichUsers) != 2 {
		t.Fatalf("users = %#v", preview.ImmichUsers)
	}
	fallback := preview.ImmichUsers[1]
	if fallback.Status != "fallback" || fallback.AccountID != "acc-1" || fallback.GoogleEmail != "warre@gmail.com" {
		t.Fatalf("fallback user = %#v", fallback)
	}
}

func TestRoutingPreviewReportsImmichError(t *testing.T) {
	service := New(Options{Google: &fakeGoogle{}})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	preview := service.RoutingPreview(ctx, &config.Config{ApiKey: "k", ApiURL: "http://127.0.0.1:1/api"})
	if preview.ImmichError == "" {
		t.Fatal("a failed user listing must be reported")
	}
}

func TestKnownImmichUsersFallsBackToServiceURL(t *testing.T) {
	server := immichServer(t, "cfg-key", `{}`, `[{"id":"owner-1","name":"Warre"}]`)
	service := New(Options{Google: &fakeGoogle{}, ImmichURL: server.URL})

	users, err := service.knownImmichUsers(context.Background(), &config.Config{ApiKey: "cfg-key"})
	if err != nil {
		t.Fatalf("knownImmichUsers failed: %v", err)
	}
	if len(users) != 1 || users[0].ID != "owner-1" {
		t.Fatalf("users = %#v", users)
	}

	// Without any credentials the preview simply lists no users.
	users, err = New(Options{}).knownImmichUsers(context.Background(), nil)
	if err != nil || users != nil {
		t.Fatalf("users = %#v, err = %v", users, err)
	}
}

func TestTestImmichKey(t *testing.T) {
	server := immichServer(t, "key-1", `{"id":"owner-1","name":"Warre"}`, `[]`)
	service := New(Options{Google: &fakeGoogle{}, ImmichURL: server.URL})

	if _, _, err := service.testImmichKey(context.Background(), "  "); err == nil {
		t.Fatal("an empty key must be rejected")
	}
	if _, _, err := New(Options{}).testImmichKey(context.Background(), "key-1"); err == nil {
		t.Fatal("a missing Immich URL must be rejected")
	}

	id, name, err := service.testImmichKey(context.Background(), " key-1 ")
	if err != nil {
		t.Fatalf("testImmichKey failed: %v", err)
	}
	if id != "owner-1" || name != "Warre" {
		t.Fatalf("id = %q, name = %q", id, name)
	}

	if _, _, err := service.testImmichKey(context.Background(), "bad"); err == nil {
		t.Fatal("an unauthorized key must fail")
	}
}

func TestWriteCookiesFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "cookies.txt")
	if err := writeCookiesFile(path, "cookie-line"); err != nil {
		t.Fatalf("writeCookiesFile failed: %v", err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "cookie-line\n" {
		t.Fatalf("content = %q", content)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("mode = %o", info.Mode().Perm())
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("the temporary file must be renamed, not left behind")
	}

	// Replacing an existing file keeps the same permissions.
	if err := writeCookiesFile(path, "second-line"); err != nil {
		t.Fatalf("replace failed: %v", err)
	}
	content, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "second-line\n" {
		t.Fatalf("content = %q", content)
	}

	// A path whose parent is a file cannot be created.
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeCookiesFile(filepath.Join(blocker, "cookies.txt"), "line"); err == nil {
		t.Fatal("an impossible path must fail")
	}
}
