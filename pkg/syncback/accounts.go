package syncback

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"warreth.dev/gphotos2immich/pkg/config"
	"warreth.dev/gphotos2immich/pkg/gphotosweb"
	"warreth.dev/gphotos2immich/pkg/immich"
	"warreth.dev/gphotos2immich/pkg/upload"
)

// ErrAccountNotFound means no configured mapping has the requested id.
var ErrAccountNotFound = errors.New("account mapping not found")

// accountWebCheckTTL is how long a browser-session check is reused, so opening
// the account page does not call Google for every account on every request.
const accountWebCheckTTL = 5 * time.Minute

// webCheck is the cached browser-session result for one cookies file.
type webCheck struct {
	status WebStatus
	at     time.Time
}

// AccountInput is one account mapping as submitted by the UI. The token and
// cookies fields are write-only: they are consumed here and never stored in
// the configuration.
type AccountInput struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	ImmichUserID   string `json:"immichUserId"`
	ImmichAPIKey   string `json:"immichApiKey"`
	GoogleEmail    string `json:"googleEmail"`
	WebCookiesFile string `json:"webCookiesFile"`
	IsDefault      bool   `json:"isDefault"`
	OAuthToken     string `json:"oauthToken,omitempty"`
	Cookies        string `json:"cookies,omitempty"`
}

// AccountMappingStatus is a configured mapping plus its live status.
type AccountMappingStatus struct {
	config.AccountMapping
	CookiesFile     string `json:"cookiesFile,omitempty"`
	CookiesSaved    bool   `json:"cookiesSaved"`
	WebSessionOK    bool   `json:"webSessionOk"`
	WebSessionError string `json:"webSessionError,omitempty"`
	ImmichKeyOK     bool   `json:"immichKeyOk"`
	ImmichUserName  string `json:"immichUserName,omitempty"`
	ImmichError     string `json:"immichError,omitempty"`
	GoogleConnected bool   `json:"googleConnected"`
	GoogleError     string `json:"googleError,omitempty"`
}

// RoutingPreviewAccount is one account in the routing preview.
type RoutingPreviewAccount struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	GoogleEmail string `json:"googleEmail,omitempty"`
	IsDefault   bool   `json:"isDefault,omitempty"`
}

// RoutingPreviewUser is one known Immich user and where its photos go.
type RoutingPreviewUser struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Email       string `json:"email,omitempty"`
	AccountID   string `json:"accountId,omitempty"`
	GoogleEmail string `json:"googleEmail,omitempty"`
	Status      string `json:"status"` // mapped, fallback or skipped
}

// RoutingPreview explains the Immich owner -> Google account routing.
type RoutingPreview struct {
	Strict      bool                    `json:"strict"`
	Unmapped    string                  `json:"unmapped"` // fallback or skip
	Fallback    string                  `json:"fallback,omitempty"`
	Accounts    []RoutingPreviewAccount `json:"accounts"`
	ImmichUsers []RoutingPreviewUser    `json:"immichUsers"`
	ImmichError string                  `json:"immichError,omitempty"`
}

// SetAccounts replaces the configured owner -> account mappings.
func (s *Service) SetAccounts(accounts []config.AccountMapping, strict bool) {
	normalized := make([]config.AccountMapping, 0, len(accounts))
	for _, acc := range accounts {
		normalized = append(normalized, config.NormalizeAccount(acc))
	}
	s.accountsMu.Lock()
	s.accounts = normalized
	s.strict = strict
	s.accountsMu.Unlock()
}

// Accounts returns a copy of the configured mappings. The web API uses it to
// persist exactly what SaveAccount and DeleteAccount produced.
func (s *Service) Accounts() []config.AccountMapping {
	accounts, _ := s.accountsSnapshot()
	return accounts
}

// accountsSnapshot returns a copy of the configured mappings.
func (s *Service) accountsSnapshot() ([]config.AccountMapping, bool) {
	s.accountsMu.Lock()
	defer s.accountsMu.Unlock()
	accounts := make([]config.AccountMapping, len(s.accounts))
	copy(accounts, s.accounts)
	return accounts, s.strict
}

// ResolveOwner implements upload.AccountResolver. Without configured accounts
// every asset keeps the single-account behavior.
func (s *Service) ResolveOwner(ownerID string, strict bool) (upload.AccountRoute, bool, bool) {
	accounts, _ := s.accountsSnapshot()
	if len(accounts) == 0 {
		return upload.AccountRoute{}, true, false
	}
	ownerID = strings.TrimSpace(ownerID)
	if ownerID != "" {
		for _, acc := range accounts {
			if strings.EqualFold(acc.ImmichUserID, ownerID) {
				return s.routeFor(acc), true, false
			}
		}
	}
	if strict {
		return upload.AccountRoute{}, false, false
	}
	if fallback, ok := defaultAccount(accounts); ok {
		return s.routeFor(fallback), true, true
	}
	// No mapping claims the default role, so the primary gotohp account (the
	// one an existing installation already uses) takes unmapped owners.
	return upload.AccountRoute{}, true, true
}

// RouteForEmail implements upload.AccountRouteByEmail, so a pending album add
// retries through the session of the account that uploaded the files.
func (s *Service) RouteForEmail(email string) (upload.AccountRoute, bool) {
	email = strings.TrimSpace(email)
	if email == "" {
		return upload.AccountRoute{}, false
	}
	accounts, _ := s.accountsSnapshot()
	for _, acc := range accounts {
		if strings.EqualFold(acc.GoogleEmail, email) {
			return s.routeFor(acc), true
		}
	}
	return upload.AccountRoute{}, false
}

// routeFor builds the upload route for one mapping. Missing sessions leave the
// writer nil, so the uploader falls back to the service level session.
func (s *Service) routeFor(acc config.AccountMapping) upload.AccountRoute {
	web := s.webClientFor(acc)
	return upload.AccountRoute{
		GoogleEmail:  acc.GoogleEmail,
		SharedWriter: web,
		Inspector:    web,
	}
}

// defaultAccount returns the mapping marked as default.
func defaultAccount(accounts []config.AccountMapping) (config.AccountMapping, bool) {
	for _, acc := range accounts {
		if acc.IsDefault {
			return acc, true
		}
	}
	return config.AccountMapping{}, false
}

// implicitCookiesFile reports whether a mapping relies on its generated
// per-account file instead of a path someone chose.
func implicitCookiesFile(acc config.AccountMapping) bool {
	path := strings.TrimSpace(acc.WebCookiesFile)
	if path == "" {
		return true
	}
	return path == config.EffectiveAccountCookiesFile(config.AccountMapping{
		ID:           acc.ID,
		ImmichUserID: acc.ImmichUserID,
		GoogleEmail:  acc.GoogleEmail,
	})
}

// sessionPath returns where one account's cookies are stored and whether that
// is the primary browser-session file. The connected primary account keeps
// using the session an existing installation already stores, so enabling
// accounts does not ask for the same cookies twice.
func (s *Service) sessionPath(acc config.AccountMapping) (string, bool) {
	path := config.EffectiveAccountCookiesFile(acc)
	servicePath := strings.TrimSpace(s.webCookiesPath)
	if !implicitCookiesFile(acc) {
		return path, servicePath != "" && path == servicePath
	}
	if servicePath != "" && s.primaryAccountEmail(acc.GoogleEmail) {
		return servicePath, true
	}
	return path, servicePath != "" && path == servicePath
}

// accountCookiesPath is the file the UI reports for one account.
func (s *Service) accountCookiesPath(acc config.AccountMapping) string {
	path, _ := s.sessionPath(acc)
	return path
}

// webClientFor returns the browser-session client of one account.
func (s *Service) webClientFor(acc config.AccountMapping) WebClient {
	path, primary := s.sessionPath(acc)
	if primary && s.Web != nil && s.Web.Available() {
		return s.Web
	}

	s.accountsMu.Lock()
	defer s.accountsMu.Unlock()
	if client, ok := s.webClients[path]; ok {
		return client
	}
	factory := s.webFactory
	if factory == nil {
		factory = func(cookiesPath string) WebClient { return gphotosweb.NewClient(cookiesPath) }
	}
	client := factory(path)
	if s.webClients == nil {
		s.webClients = make(map[string]WebClient)
	}
	s.webClients[path] = client
	return client
}

// primaryAccountEmail reports whether email belongs to the connected primary
// gotohp account.
func (s *Service) primaryAccountEmail(email string) bool {
	email = strings.TrimSpace(email)
	if email == "" {
		return false
	}
	return strings.EqualFold(email, strings.TrimSpace(s.Account().Email))
}

// AccountMappings lists every configured mapping with its live status.
func (s *Service) AccountMappings(ctx context.Context) []AccountMappingStatus {
	accounts, _ := s.accountsSnapshot()
	googleAccounts, googleErr := s.googleAccountList(ctx)
	out := make([]AccountMappingStatus, 0, len(accounts))
	for _, acc := range accounts {
		out = append(out, s.accountMappingStatus(ctx, acc, googleAccounts, googleErr))
	}
	return out
}

// googleAccountList returns the emails stored in gotohp, when available.
func (s *Service) googleAccountList(ctx context.Context) ([]string, error) {
	if s == nil || s.Google == nil {
		return nil, ErrDisabled
	}
	return s.Google.ListAccounts(ctx)
}

// accountMappingStatus collects the live status of one mapping.
func (s *Service) accountMappingStatus(ctx context.Context, acc config.AccountMapping, googleAccounts []string, googleErr error) AccountMappingStatus {
	status := AccountMappingStatus{
		AccountMapping: acc,
		CookiesFile:    s.accountCookiesPath(acc),
	}
	if googleErr != nil {
		status.GoogleError = googleErr.Error()
	} else {
		for _, email := range googleAccounts {
			if strings.EqualFold(strings.TrimSpace(email), acc.GoogleEmail) {
				status.GoogleConnected = true
				break
			}
		}
	}

	if acc.ImmichAPIKey != "" {
		if _, name, err := s.testImmichKey(ctx, acc.ImmichAPIKey); err != nil {
			status.ImmichError = err.Error()
		} else {
			status.ImmichKeyOK = true
			status.ImmichUserName = name
		}
	}

	web := s.webClientFor(acc)
	if web == nil || !web.Available() {
		return status
	}
	status.CookiesSaved = true
	webStatus := s.accountWebStatus(ctx, status.CookiesFile, web)
	status.WebSessionOK = webStatus.Connected
	status.WebSessionError = webStatus.LastError
	return status
}

// accountWebStatus validates a cookies file, reusing a recent result.
func (s *Service) accountWebStatus(ctx context.Context, path string, client WebClient) WebStatus {
	s.accountsMu.Lock()
	check, ok := s.webChecks[path]
	s.accountsMu.Unlock()
	if ok && time.Since(check.at) < accountWebCheckTTL {
		return check.status
	}

	status := WebStatus{}
	if _, err := client.Validate(ctx); err != nil {
		status.LastError = err.Error()
	} else {
		status.Connected = true
		status.Validated = time.Now()
	}

	s.accountsMu.Lock()
	if s.webChecks == nil {
		s.webChecks = make(map[string]webCheck)
	}
	s.webChecks[path] = webCheck{status: status, at: time.Now()}
	s.accountsMu.Unlock()
	return status
}

// SaveAccount adds or updates one mapping. It connects the oauth_token and
// stores the cookies when supplied, and verifies the Immich API key first so a
// broken key never reaches the configuration.
func (s *Service) SaveAccount(ctx context.Context, in AccountInput) (AccountMappingStatus, error) {
	acc := config.AccountMapping{
		ID:             in.ID,
		Name:           in.Name,
		ImmichUserID:   in.ImmichUserID,
		ImmichAPIKey:   in.ImmichAPIKey,
		GoogleEmail:    in.GoogleEmail,
		WebCookiesFile: in.WebCookiesFile,
		IsDefault:      in.IsDefault,
	}
	if strings.TrimSpace(acc.ImmichUserID) == "" {
		return AccountMappingStatus{}, fmt.Errorf("immichUserId is required")
	}

	accounts, strict := s.accountsSnapshot()
	index := -1
	for i, existing := range accounts {
		if wanted := strings.TrimSpace(acc.ID); wanted != "" && strings.EqualFold(existing.ID, wanted) {
			index = i
			break
		}
		if strings.EqualFold(existing.ImmichUserID, strings.TrimSpace(acc.ImmichUserID)) {
			index = i
			break
		}
	}
	if index >= 0 {
		// A form only sends the fields that changed, so an omitted value keeps
		// the stored one instead of erasing it. The merge happens before
		// normalization, so the derived cookies path of the existing id wins
		// over one derived from the raw user id.
		existing := accounts[index]
		if strings.TrimSpace(acc.ID) == "" {
			acc.ID = existing.ID
		}
		if strings.TrimSpace(acc.Name) == "" {
			acc.Name = existing.Name
		}
		if strings.TrimSpace(acc.ImmichAPIKey) == "" {
			acc.ImmichAPIKey = existing.ImmichAPIKey
		}
		if strings.TrimSpace(acc.GoogleEmail) == "" {
			acc.GoogleEmail = existing.GoogleEmail
		}
		if strings.TrimSpace(acc.WebCookiesFile) == "" {
			acc.WebCookiesFile = existing.WebCookiesFile
		}
	}
	acc = config.NormalizeAccount(acc)

	// Validate the resulting list before any side effect runs.
	updated := make([]config.AccountMapping, len(accounts))
	copy(updated, accounts)
	if index >= 0 {
		updated[index] = acc
	} else {
		updated = append(updated, acc)
	}
	if acc.IsDefault {
		for i := range updated {
			if !strings.EqualFold(updated[i].ID, acc.ID) {
				updated[i].IsDefault = false
			}
		}
	}
	if err := (&config.Config{Accounts: updated}).ValidateAccounts(); err != nil {
		return AccountMappingStatus{}, err
	}

	if token := strings.TrimSpace(in.OAuthToken); token != "" {
		if s.Google == nil {
			return AccountMappingStatus{}, ErrDisabled
		}
		email, err := s.Google.AddAccount(ctx, token)
		if err != nil {
			return AccountMappingStatus{}, err
		}
		if acc.GoogleEmail == "" {
			acc.GoogleEmail = strings.TrimSpace(email)
		} else if !strings.EqualFold(acc.GoogleEmail, strings.TrimSpace(email)) {
			return AccountMappingStatus{}, fmt.Errorf("the oauth_token belongs to %s, not %s", email, acc.GoogleEmail)
		}
		acc = config.NormalizeAccount(acc)
	}

	if acc.ImmichAPIKey != "" {
		if _, _, err := s.testImmichKey(ctx, acc.ImmichAPIKey); err != nil {
			return AccountMappingStatus{}, fmt.Errorf("Immich API key test failed: %w", err)
		}
	}

	if cookies := strings.TrimSpace(in.Cookies); cookies != "" {
		web := s.webClientFor(acc)
		if _, err := web.ValidateCookies(ctx, []byte(cookies)); err != nil {
			return AccountMappingStatus{}, fmt.Errorf("Google Photos browser session test failed: %w", err)
		}
		path, _ := s.sessionPath(acc)
		if err := writeCookiesFile(path, cookies); err != nil {
			return AccountMappingStatus{}, err
		}
		web.Reload()
		s.accountsMu.Lock()
		delete(s.webChecks, path)
		s.accountsMu.Unlock()
	}

	if index >= 0 {
		updated[index] = acc
	} else {
		updated[len(updated)-1] = acc
	}
	if acc.IsDefault {
		for i := range updated {
			if !strings.EqualFold(updated[i].ID, acc.ID) {
				updated[i].IsDefault = false
			}
		}
	}
	if err := (&config.Config{Accounts: updated}).ValidateAccounts(); err != nil {
		return AccountMappingStatus{}, err
	}
	s.SetAccounts(updated, strict)

	googleAccounts, googleErr := s.googleAccountList(ctx)
	return s.accountMappingStatus(ctx, acc, googleAccounts, googleErr), nil
}

// DeleteAccount removes one mapping. Google credentials and cookies are left
// alone, because another mapping may share them.
func (s *Service) DeleteAccount(_ context.Context, id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("account id is required")
	}
	accounts, strict := s.accountsSnapshot()
	remaining := make([]config.AccountMapping, 0, len(accounts))
	found := false
	for _, acc := range accounts {
		if strings.EqualFold(acc.ID, id) || strings.EqualFold(acc.ImmichUserID, id) {
			found = true
			continue
		}
		remaining = append(remaining, acc)
	}
	if !found {
		return ErrAccountNotFound
	}
	s.SetAccounts(remaining, strict)
	return nil
}

// RoutingPreview summarizes which Google account receives each known Immich
// user's photos, and what happens to users without a mapping.
func (s *Service) RoutingPreview(ctx context.Context, cfg *config.Config) RoutingPreview {
	accounts, strict := s.accountsSnapshot()
	if cfg != nil {
		accounts = make([]config.AccountMapping, 0, len(cfg.Accounts))
		for _, acc := range cfg.Accounts {
			accounts = append(accounts, config.NormalizeAccount(acc))
		}
		strict = cfg.GoogleUpload.StrictAccountMatching
	}

	preview := RoutingPreview{Strict: strict, Unmapped: "skip"}
	for _, acc := range accounts {
		preview.Accounts = append(preview.Accounts, RoutingPreviewAccount{
			ID:          acc.ID,
			Name:        acc.Name,
			GoogleEmail: acc.GoogleEmail,
			IsDefault:   acc.IsDefault,
		})
	}
	// Keep the account list stable for the UI, even if configuration order
	// changes.
	sort.Slice(preview.Accounts, func(i, j int) bool {
		return preview.Accounts[i].ID < preview.Accounts[j].ID
	})
	if fallback, ok := defaultAccount(accounts); ok {
		preview.Fallback = fallback.GoogleEmail
	} else if len(accounts) > 0 {
		preview.Fallback = "primary"
	}
	if !strict && len(accounts) > 0 {
		preview.Unmapped = "fallback"
	}

	users, err := s.knownImmichUsers(ctx, cfg)
	if err != nil {
		preview.ImmichError = err.Error()
	}
	routeUser := func(user immich.User) RoutingPreviewUser {
		row := RoutingPreviewUser{ID: user.ID, Name: user.Name, Email: user.Email}
		for _, acc := range accounts {
			if strings.EqualFold(acc.ImmichUserID, strings.TrimSpace(user.ID)) {
				row.AccountID = acc.ID
				row.GoogleEmail = acc.GoogleEmail
				row.Status = "mapped"
				return row
			}
		}
		if strict {
			row.Status = "skipped"
			return row
		}
		row.Status = "fallback"
		if fallback, ok := defaultAccount(accounts); ok {
			row.AccountID = fallback.ID
			row.GoogleEmail = fallback.GoogleEmail
		}
		return row
	}
	for _, user := range users {
		preview.ImmichUsers = append(preview.ImmichUsers, routeUser(user))
	}
	return preview
}

// knownImmichUsers lists the Immich users when credentials are available.
func (s *Service) knownImmichUsers(ctx context.Context, cfg *config.Config) ([]immich.User, error) {
	apiURL, apiKey := "", ""
	if cfg != nil {
		apiURL = strings.TrimSpace(cfg.ApiURL)
		apiKey = strings.TrimSpace(cfg.ApiKey)
	}
	if apiURL == "" {
		apiURL = s.immichURL
	}
	if apiURL == "" || apiKey == "" {
		return nil, nil
	}
	return immich.NewClient(apiURL, apiKey, 1).ListUsers(ctx)
}

// testImmichKey verifies an Immich API key and returns the user it belongs to.
func (s *Service) testImmichKey(ctx context.Context, apiKey string) (string, string, error) {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return "", "", fmt.Errorf("an Immich API key is required")
	}
	apiURL := strings.TrimSpace(s.immichURL)
	if apiURL == "" {
		return "", "", fmt.Errorf("the Immich URL is not configured for account testing")
	}
	return immich.NewClient(apiURL, apiKey, 1).GetUserOnce(ctx)
}

// writeCookiesFile stores a browser session atomically with owner-only
// permissions.
func writeCookiesFile(path, cookies string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("create browser session directory: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(cookies+"\n"), 0600); err != nil {
		return fmt.Errorf("write browser session: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("replace browser session: %w", err)
	}
	return nil
}
