package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/zalando/go-keyring"
)

// The cloud hub. Not the landing's domain: that site answers every path with its HTML page.
const defaultHub = "https://metro-lx-moqastudio-e1425ede.koyeb.app"

// The keychain holds one entry per hub (account = hub URL), so a cloud and a self-hosted hub coexist.
const keychainService = "line-lx"

// errNotSignedIn: the keychain has no usable sign-in for the hub, so `line-lx login` is the fix.
type errNotSignedIn struct{ msg string }

func (e errNotSignedIn) Error() string { return e.msg }

var errExpired = errNotSignedIn{"Your session expired. Run `line-lx login`."}

type creds struct {
	ClientID     string    `json:"client_id"`
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	ExpiresAt    time.Time `json:"expires_at"`
	Resource     string    `json:"resource"` // the tokens' audience; earlier versions didn't store it
}

// --- Config: the current hub ---

func configPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "line-lx", "config.json"), nil
}

// checkHub accepts https URLs, and http only on this machine, with an optional path prefix.
// It returns the URL without a trailing slash: the form used as the keychain account and in stamps.
func checkHub(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || !allowedScheme(u) {
		return "", fmt.Errorf("%q isn't a valid hub URL: use https://host[/path], or http only for localhost.", clean(raw))
	}
	return strings.TrimRight(u.Scheme+"://"+strings.ToLower(u.Host)+u.EscapedPath(), "/"), nil
}

func allowedScheme(u *url.URL) bool {
	host := u.Hostname()
	return u.Scheme == "https" || u.Scheme == "http" && (host == "localhost" || host == "127.0.0.1" || host == "::1")
}

// currentHub picks the hub: LINE_LX_HUB, else the one saved by `line-lx login --hub`, else the cloud hub.
func currentHub() (string, error) {
	if env := os.Getenv("LINE_LX_HUB"); env != "" {
		return checkHub(env)
	}
	if path, err := configPath(); err == nil {
		var cfg struct{ Hub string }
		if data, err := os.ReadFile(path); err == nil && json.Unmarshal(data, &cfg) == nil && cfg.Hub != "" {
			return checkHub(cfg.Hub)
		}
	}
	return defaultHub, nil
}

func saveHub(hub string) error {
	path, err := configPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, _ := json.Marshal(map[string]string{"hub": hub})
	return os.WriteFile(path, append(data, '\n'), 0o600)
}

// --- Keychain (credstore.go: or a private file, on a machine without one) ---

func loadCreds(hub string) (*creds, error) {
	raw, err := credGet(hub)
	if errors.Is(err, keyring.ErrNotFound) {
		return nil, errNotSignedIn{fmt.Sprintf("Not signed in to %s. Run `line-lx login`.", hub)}
	}
	if err != nil {
		return nil, err
	}
	var c creds
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		return nil, errNotSignedIn{fmt.Sprintf("The stored sign-in for %s is unreadable. Run `line-lx login`.", hub)}
	}
	return &c, nil
}

func saveCreds(hub string, c *creds) error {
	data, _ := json.Marshal(c)
	return credSet(hub, string(data))
}

// --- Tokens ---

// needsRefresh is true when the token expires within a minute: refresh before a request can fail mid-way.
func needsRefresh(c *creds, now time.Time) bool {
	return now.Add(60 * time.Second).After(c.ExpiresAt)
}

// keychainToken returns a valid access token for the hub from the keychain, refreshing it when needed.
// With a still-valid token it makes no network call.
func keychainToken(hub string) (string, error) {
	c, err := loadCreds(hub)
	if err != nil {
		return "", err
	}
	// Earlier versions signed in for the hub's MCP server, whose tokens the API refuses.
	if c.Resource != apiResource(hub) {
		return "", errNotSignedIn{fmt.Sprintf("Your sign-in to %s is from an earlier line-lx. Run `line-lx login`.", hub)}
	}
	if !needsRefresh(c, time.Now()) {
		return c.AccessToken, nil
	}
	if c.RefreshToken == "" {
		return "", errExpired
	}
	// No lock: parallel line-lx commands near expiry each refresh. The hub's OAuth provider accepts a
	// reused refresh token for 30 s, so they all get valid tokens.
	oauth, err := discover(hub)
	if err != nil {
		return "", err
	}
	fresh, err := requestToken(oauth.TokenEndpoint, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {c.RefreshToken},
		"client_id":     {c.ClientID},
		"resource":      {c.Resource},
	})
	// The hub refusing the refresh (any 4xx but a rate limit: invalid_grant, or invalid_client for a
	// client it no longer has) means signing in again; a network or server error is just reported.
	var oe *oauthError
	if errors.As(err, &oe) && oe.status >= 400 && oe.status < 500 && oe.status != http.StatusTooManyRequests {
		return "", errExpired
	}
	if err != nil {
		return "", fmt.Errorf("Can't refresh the sign-in: %v", err)
	}
	fresh.ClientID, fresh.Resource = c.ClientID, c.Resource
	if fresh.RefreshToken == "" {
		fresh.RefreshToken = c.RefreshToken
	}
	if err := saveCreds(hub, fresh); err != nil {
		return "", err
	}
	return fresh.AccessToken, nil
}

// bearer is the token every command uses: LINE_LX_TOKEN (CI) wins over the keychain, but only for the
// hub LINE_LX_HUB names, so the token never goes to another hub (`line-lx setup --hub`).
func bearer(hub string) (string, error) {
	if t := os.Getenv("LINE_LX_TOKEN"); t != "" {
		if os.Getenv("LINE_LX_HUB") == "" {
			return "", errors.New("LINE_LX_TOKEN needs LINE_LX_HUB, so the token only goes to the hub it belongs to.")
		}
		if own, err := checkHub(os.Getenv("LINE_LX_HUB")); err != nil || own != hub {
			return "", fmt.Errorf("LINE_LX_TOKEN belongs to LINE_LX_HUB, not to %s.", hub)
		}
		return t, nil
	}
	return keychainToken(hub)
}

// authHeaderWait is how long auth-header waits for the hub: under the 10 s Claude Code gives it.
const authHeaderWait = 9 * time.Second

// runAuthHeader is what Claude Code runs to sign in to the hub: the headersHelper of the hub's marketplace.
// It prints the person's token as one JSON object of HTTP headers and nothing else on stdout. Claude Code
// waits at most 10 s for it, and reuses its output for 60 s, which a token refreshed in its last minute covers.
func runAuthHeader() error {
	hub, err := currentHub()
	if err != nil {
		return err
	}
	// Claude Code says which marketplace or archive it asks headers for, and the token only goes to its own
	// hub. Without either, it isn't Claude Code asking: line-lx refuses.
	market, archive := os.Getenv("CLAUDE_CODE_MARKETPLACE_URL"), os.Getenv("CLAUDE_CODE_PLUGIN_ARCHIVE_URL")
	if market == "" && archive == "" {
		return errors.New("line-lx auth-header is only for Claude Code, which runs it to sign in to the hub's marketplace.")
	}
	for _, raw := range []string{market, archive} {
		if raw != "" && !onHub(raw, hub) {
			return fmt.Errorf("%s isn't on %s, so line-lx won't give it your sign-in.", clean(raw), hub)
		}
	}
	// A refresh can take longer than Claude Code waits: line-lx stops first, and says why.
	stop := time.AfterFunc(authHeaderWait, func() {
		fmt.Fprintf(os.Stderr, "The hub at %s didn't answer within %s, so line-lx stopped.\n", hub, authHeaderWait)
		os.Exit(1)
	})
	defer stop.Stop()
	token, err := bearer(hub)
	if err != nil {
		return err
	}
	data, _ := json.Marshal(map[string]string{"Authorization": "Bearer " + token})
	fmt.Println(string(data))
	return nil
}

// onHub is true when raw is on the hub: its scheme, host and port, and under its path.
func onHub(raw, hub string) bool {
	u, err := url.Parse(raw)
	h, _ := url.Parse(hub)
	return err == nil && u.User == nil && u.Scheme == h.Scheme && strings.EqualFold(u.Host, h.Host) &&
		strings.HasPrefix(u.EscapedPath(), h.EscapedPath()+"/") && !slices.Contains(strings.Split(u.Path, "/"), "..")
}

// --- OAuth (the hub is the provider) ---

type oauthConfig struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	RegistrationEndpoint  string `json:"registration_endpoint"`
	RevocationEndpoint    string `json:"revocation_endpoint"`
}

// apiResource is the hub's API, the resource (RFC 8707) line-lx asks tokens for: the API refuses tokens
// issued for anything else, such as the hub's MCP server.
func apiResource(hub string) string { return hub + "/api" }

// discover reads the hub's OAuth metadata. The authorization endpoint is opened in the browser, so every
// endpoint must be on the hub itself: a hub can't send line-lx (or the browser opener) anywhere else.
func discover(hub string) (*oauthConfig, error) {
	var oc oauthConfig
	if err := getJSON(hub+"/.well-known/oauth-authorization-server", &oc); err != nil {
		return nil, err
	}
	hubURL, _ := url.Parse(hub)
	onHub := func(endpoint string) bool {
		u, err := url.Parse(endpoint)
		return err == nil && allowedScheme(u) && u.Host == hubURL.Host && u.User == nil
	}
	ok := strings.TrimRight(oc.Issuer, "/") == hub &&
		onHub(oc.AuthorizationEndpoint) && onHub(oc.TokenEndpoint) && onHub(oc.RegistrationEndpoint) &&
		(oc.RevocationEndpoint == "" || onHub(oc.RevocationEndpoint))
	if !ok {
		return nil, fmt.Errorf("The hub at %s announces OAuth endpoints that aren't its own, so line-lx won't use them.", hub)
	}
	return &oc, nil
}

func getJSON(u string, out any) error {
	res, err := httpClient.Get(u)
	if err != nil {
		return fmt.Errorf("Can't reach %s: %v", u, err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("%s answered %d.", u, res.StatusCode)
	}
	if err := json.NewDecoder(limited(res.Body)).Decode(out); err != nil {
		return fmt.Errorf("%s isn't valid JSON: %v", u, err)
	}
	return nil
}

// oauthError is the token endpoint's refusal; its code tells an expired sign-in from other failures.
type oauthError struct {
	status            int
	code, description string
}

func (e *oauthError) Error() string {
	return clean(strings.TrimSpace(fmt.Sprintf("the hub refused the token request (%d %s %s)", e.status, e.code, e.description)))
}

func requestToken(endpoint string, form url.Values) (*creds, error) {
	res, err := httpClient.PostForm(endpoint, form)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	var body struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
		Error        string `json:"error"`
		Description  string `json:"error_description"`
	}
	_ = json.NewDecoder(limited(res.Body)).Decode(&body)
	if res.StatusCode != http.StatusOK || body.AccessToken == "" {
		return nil, &oauthError{res.StatusCode, body.Error, body.Description}
	}
	// A missing lifetime gets 5 minutes; an absurd one is capped at 30 days.
	expiresIn := time.Duration(body.ExpiresIn) * time.Second
	if body.ExpiresIn <= 0 {
		expiresIn = 5 * time.Minute
	}
	expiresIn = min(expiresIn, 30*24*time.Hour)
	return &creds{
		AccessToken:  body.AccessToken,
		RefreshToken: body.RefreshToken,
		ExpiresAt:    time.Now().Add(expiresIn),
	}, nil
}

func randomString() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

type callbackResult struct{ code, err string }

// callbackHandler answers the browser's redirect after sign-in. A request without this login's state is
// refused and ignored, so a stray or forged request can't end the login.
func callbackHandler(state string, done chan<- callbackResult) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if r.URL.Path != "/callback" || q.Get("state") != state {
			http.Error(w, "This isn't the sign-in line-lx is waiting for.", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		res := callbackResult{code: q.Get("code")}
		switch {
		case q.Get("error") != "":
			res = callbackResult{err: clean(strings.TrimSpace(q.Get("error") + " " + q.Get("error_description")))}
		case res.code == "":
			res = callbackResult{err: "the hub sent no authorization code"}
		}
		if res.err != "" {
			fmt.Fprintln(w, "Sign-in failed. You can close this tab.")
		} else {
			fmt.Fprintln(w, "Signed in to line-lx. You can close this tab.")
		}
		select {
		case done <- res:
		default:
		}
	})
}

// flagOrCurrentHub is the hub a `--hub` flag names, else the current one.
func flagOrCurrentHub(hubFlag string) (string, error) {
	if hubFlag != "" {
		return checkHub(hubFlag)
	}
	return currentHub()
}

func runLogin(hubFlag string) error {
	hub, err := flagOrCurrentHub(hubFlag)
	if err != nil {
		return err
	}
	_, err = login(hub)
	return err
}

// login: OAuth 2.1 authorization code + PKCE, with a fresh client registered for a loopback callback.
// It never reads stdin, so it works without a terminal: the person signs in in the browser. It returns
// who signed in.
func login(hub string) (*me, error) {
	oauth, err := discover(hub)
	if err != nil {
		return nil, err
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("Can't open a local port for the sign-in callback: %v", err)
	}
	defer listener.Close()
	// The loopback IP, not "localhost", as RFC 8252 recommends: it can't resolve elsewhere.
	port := listener.Addr().(*net.TCPAddr).Port
	redirectURI := fmt.Sprintf("http://127.0.0.1:%d/callback", port)

	clientID, err := registerClient(oauth.RegistrationEndpoint, redirectURI)
	if err != nil {
		return nil, err
	}

	verifier, state := randomString(), randomString()
	challenge := sha256.Sum256([]byte(verifier))
	authURL := oauth.AuthorizationEndpoint + "?" + url.Values{
		"response_type":         {"code"},
		"client_id":             {clientID},
		"redirect_uri":          {redirectURI},
		"scope":                 {"openid profile email offline_access"},
		"state":                 {state},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(challenge[:])},
		"code_challenge_method": {"S256"},
		"resource":              {apiResource(hub)},
	}.Encode()

	done := make(chan callbackResult, 1)
	server := &http.Server{Handler: callbackHandler(state, done), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = server.Serve(listener) }()
	defer server.Shutdown(context.Background())

	fmt.Printf("Opening your browser to sign in. If it doesn't open, visit:\n%s\n", authURL)
	if remoteSession() {
		// The hub sends the browser back to 127.0.0.1 on this machine, which a browser elsewhere can't reach.
		fmt.Printf("On a remote machine, the browser must reach this machine's port %d: on your computer, run\n"+
			"  ssh -N -L %d:127.0.0.1:%d <this machine>\nthen open the URL above there.\n", port, port, port)
	}
	openBrowser(authURL)

	var res callbackResult
	select {
	case res = <-done:
	case <-time.After(5 * time.Minute):
		return nil, errors.New("Sign-in timed out after 5 minutes. Run `line-lx login` again.")
	}
	if res.err != "" {
		return nil, fmt.Errorf("Sign-in failed: %s.", res.err)
	}

	c, err := requestToken(oauth.TokenEndpoint, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {res.code},
		"redirect_uri":  {redirectURI},
		"client_id":     {clientID},
		"code_verifier": {verifier},
		"resource":      {apiResource(hub)},
	})
	if err != nil {
		return nil, fmt.Errorf("Sign-in failed: %v.", err)
	}
	c.ClientID, c.Resource = clientID, apiResource(hub)
	// The sign-in this one replaces, if any: revoked once the new one is saved.
	previous, _ := loadCreds(hub)
	// The hub first: a failed config write must not leave a sign-in the next commands can't find.
	if err := saveHub(hub); err != nil {
		return nil, fmt.Errorf("Can't save the hub in the config: %v", err)
	}
	if err := saveCreds(hub, c); err != nil {
		return nil, err
	}
	revoke(oauth, previous)

	// Use the fresh token, not LINE_LX_TOKEN, to confirm who just signed in.
	api := &hubClient{hub: hub, token: c.AccessToken}
	var m me
	if err := api.do("GET", "/api/me", nil, &m); err != nil {
		return nil, err
	}
	fmt.Printf("Signed in as %s %s · workspace %s · %s\n", clean(m.User.Name), clean(m.User.Email), clean(m.Org.Name), hub)
	return &m, nil
}

// remoteSession guesses that the browser runs on another machine: line-lx runs over SSH, or on Linux
// without a display.
func remoteSession() bool {
	if os.Getenv("SSH_CONNECTION") != "" || os.Getenv("SSH_TTY") != "" {
		return true
	}
	return runtime.GOOS == "linux" && os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == ""
}

func registerClient(endpoint, redirectURI string) (string, error) {
	body, _ := json.Marshal(map[string]any{
		"client_name":                "line-lx CLI",
		"redirect_uris":              []string{redirectURI},
		"token_endpoint_auth_method": "none",
		"grant_types":                []string{"authorization_code", "refresh_token"},
	})
	res, err := httpClient.Post(endpoint, "application/json", strings.NewReader(string(body)))
	if err != nil {
		return "", fmt.Errorf("Can't reach the hub: %v", err)
	}
	defer res.Body.Close()
	var out struct {
		ClientID string `json:"client_id"`
	}
	_ = json.NewDecoder(limited(res.Body)).Decode(&out)
	if res.StatusCode != http.StatusCreated && res.StatusCode != http.StatusOK || out.ClientID == "" {
		return "", fmt.Errorf("The hub refused to register the CLI (%d).", res.StatusCode)
	}
	return out.ClientID, nil
}

// openBrowser is a variable so tests can play the browser.
var openBrowser = func(u string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", u)
	default:
		cmd = exec.Command("xdg-open", u)
	}
	_ = cmd.Start() // The URL is printed too, so a missing opener is fine.
}

func runLogout() error {
	hub, err := currentHub()
	if err != nil {
		return err
	}
	// Forget the sign-in first, so being offline never keeps it; then revoke what's in memory.
	c, _ := loadCreds(hub)
	err = credDelete(hub)
	if errors.Is(err, keyring.ErrNotFound) {
		fmt.Printf("Not signed in to %s.\n", hub)
		return nil
	}
	if err != nil {
		return err
	}
	fmt.Printf("Signed out of %s.\n", hub)
	if c != nil && c.RefreshToken != "" {
		if oauth, err := discover(hub); err == nil {
			revoke(oauth, c)
		}
	}
	return nil
}

// revoke revokes a sign-in's refresh token at the hub, so a copy of its keychain entry is useless. Best
// effort: a failure changes nothing.
func revoke(oauth *oauthConfig, c *creds) {
	if c == nil || c.RefreshToken == "" || oauth.RevocationEndpoint == "" {
		return
	}
	form := url.Values{"token": {c.RefreshToken}, "token_type_hint": {"refresh_token"}, "client_id": {c.ClientID}}
	if res, err := httpClient.PostForm(oauth.RevocationEndpoint, form); err == nil {
		res.Body.Close()
	}
}
