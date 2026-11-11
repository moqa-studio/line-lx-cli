package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zalando/go-keyring"
)

func TestValidName(t *testing.T) {
	for n, want := range map[string]bool{
		"moqa-basics": true,
		"a1":          true,
		"":            false,
		"-rf":         false,
		"--help":      false,
		"a--b":        false,
		"Upper":       false,
		"a/b":         false,
		"..":          false,
		"a b":         false,
		"a-":          false,
	} {
		if got := validName(n); got != want {
			t.Errorf("validName(%q) = %v, want %v", n, got, want)
		}
	}
	long := make([]byte, 65)
	for i := range long {
		long[i] = 'a'
	}
	if validName(string(long)) {
		t.Error("a name over 64 characters is invalid")
	}
}

func TestCheckHub(t *testing.T) {
	for raw, want := range map[string]string{
		"https://hub.example.com/":        "https://hub.example.com",
		"https://hub.example.com/hub/":    "https://hub.example.com/hub",
		"http://localhost:5173":           "http://localhost:5173",
		"http://127.0.0.1:3000/":          "http://127.0.0.1:3000",
		"http://hub.example.com":          "", // http only on this machine
		"https://user:pw@hub.example.com": "",
		"https://hub.example.com/?x=1":    "",
		"https://hub.example.com/#x":      "",
		"ftp://hub.example.com":           "",
		"hub.example.com":                 "",
	} {
		got, err := checkHub(raw)
		if want == "" && err == nil || want != "" && got != want {
			t.Errorf("checkHub(%q) = %q, %v; want %q", raw, got, err, want)
		}
	}
}

func TestBearerNeedsHubWithToken(t *testing.T) {
	t.Setenv("LINE_LX_TOKEN", "tok")
	t.Setenv("LINE_LX_HUB", "")
	if _, err := bearer("https://hub.example.com"); err == nil {
		t.Error("LINE_LX_TOKEN without LINE_LX_HUB must be refused")
	}
}

func TestWriteRefusesAnotherHubsPlugin(t *testing.T) {
	dir := t.TempDir()
	for _, tg := range []fileTarget{{name: "pi", home: dir, dir: filepath.Join(dir, "pi")}, {name: "codex", home: dir, dir: filepath.Join(dir, "codex"), flat: true}} {
		if _, err := tg.write("https://a.example", testRelease("v1", "1.0.0", skill{Name: "greet", Content: "from a"})); err != nil {
			t.Fatal(err)
		}
		if _, err := tg.write("https://b.example", testRelease("v9", "9.0.0", skill{Name: "greet", Content: "from b"})); err == nil {
			t.Errorf("%s: hub b replaced hub a's plugin", tg.name)
		}
		if s := tg.stamps()["demo"]; s.Hub != "https://a.example" {
			t.Errorf("%s: stamp = %+v", tg.name, s)
		}
	}
}

// An interrupted flat write leaves skill folders the stamp already lists, so the next write takes them over.
func TestFlatWriteAfterInterruption(t *testing.T) {
	dir := t.TempDir()
	tg := fileTarget{name: "codex", home: dir, dir: filepath.Join(dir, "skills"), flat: true}
	if _, err := tg.write("http://hub", testRelease("v1", "1.0.0", skill{Name: "a", Content: "A"})); err != nil {
		t.Fatal(err)
	}
	// What a write of v2 {a, b} leaves when stopped right after renaming b into place.
	st := tg.stamps()["demo"]
	st.Skills = []string{"a", "b"}
	if err := writeJSON(tg.stampPath("demo"), st); err != nil {
		t.Fatal(err)
	}
	if err := writeFile(filepath.Join(tg.dir, "demo--b", "SKILL.md"), "half"); err != nil {
		t.Fatal(err)
	}
	if _, err := tg.write("http://hub", testRelease("v2", "2.0.0", skill{Name: "a", Content: "A2"}, skill{Name: "b", Content: "B2"})); err != nil {
		t.Fatalf("the next sync refused a folder line-lx left: %v", err)
	}
	if read(t, filepath.Join(tg.dir, "demo--b", "SKILL.md")) != "B2" {
		t.Error("the leftover folder wasn't replaced")
	}
}

func discoveryServer(t *testing.T, edit func(doc map[string]string)) *httptest.Server {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		doc := map[string]string{
			"issuer":                 srv.URL,
			"authorization_endpoint": srv.URL + "/api/auth/oauth2/authorize",
			"token_endpoint":         srv.URL + "/api/auth/oauth2/token",
			"registration_endpoint":  srv.URL + "/api/auth/oauth2/register",
		}
		edit(doc)
		if r.URL.Path != "/.well-known/oauth-authorization-server" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(doc)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestDiscoverRefusesEndpointsElsewhere(t *testing.T) {
	good := discoveryServer(t, func(map[string]string) {})
	if _, err := discover(good.URL); err != nil {
		t.Fatalf("a hub serving its own endpoints was refused: %v", err)
	}
	for name, edit := range map[string]func(map[string]string){
		"authorize elsewhere":  func(d map[string]string) { d["authorization_endpoint"] = "https://evil.example/authorize" },
		"token elsewhere":      func(d map[string]string) { d["token_endpoint"] = "http://127.0.0.1:1/token" },
		"not a URL to open":    func(d map[string]string) { d["authorization_endpoint"] = "file:///etc/passwd" },
		"another issuer":       func(d map[string]string) { d["issuer"] = "https://evil.example" },
		"revocation elsewhere": func(d map[string]string) { d["revocation_endpoint"] = "https://evil.example/revoke" },
		"registration missing": func(d map[string]string) { delete(d, "registration_endpoint") },
	} {
		srv := discoveryServer(t, edit)
		if _, err := discover(srv.URL); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestHubClientDoesntFollowRedirects(t *testing.T) {
	hits := 0
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++ }))
	defer elsewhere.Close()
	hub := httptest.NewServer(http.RedirectHandler(elsewhere.URL+"/steal", http.StatusFound))
	defer hub.Close()

	if err := (&hubClient{hub: hub.URL, token: "tok"}).do("GET", "/api/plugins", nil, nil); err == nil {
		t.Error("a redirect must be an error")
	}
	if hits != 0 {
		t.Error("the redirect was followed, with the token")
	}
}

func TestCallbackIgnoresAnotherState(t *testing.T) {
	done := make(chan callbackResult, 1)
	h := callbackHandler("right", done)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/callback?state=wrong&code=c", nil))
	if rec.Code != http.StatusBadRequest || len(done) != 0 {
		t.Errorf("a wrong state got %d and ended the login: %v", rec.Code, len(done) != 0)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/callback?state=right&code=c", nil))
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Error("the callback page must not be sniffed")
	}
	if res := <-done; res.code != "c" || res.err != "" {
		t.Errorf("result = %+v", res)
	}
}

func TestClean(t *testing.T) {
	if got := clean("ok\x1b[31m red\n\u009b‮​"); got != "ok[31m red" {
		t.Errorf("clean = %q", got)
	}
}

func TestStagingSymlinkIsRefused(t *testing.T) {
	dir, victim := t.TempDir(), t.TempDir()
	if err := writeFile(filepath.Join(victim, "keep"), "x"); err != nil {
		t.Fatal(err)
	}
	tg := fileTarget{name: "pi", home: dir, dir: filepath.Join(dir, "skills")}
	if err := os.Symlink(victim, tg.staging()); err != nil {
		t.Skip("no symlinks here")
	}
	if _, err := tg.write("http://hub", testRelease("v1", "1.0.0", skill{Name: "a", Content: "A"})); err == nil {
		t.Error("wrote through a symlinked staging folder")
	}
	if err := tg.sweepStaging(); err == nil || !exists(filepath.Join(victim, "keep")) {
		t.Error("swept through a symlinked staging folder")
	}
}

func TestRefusedRefreshMeansSignIn(t *testing.T) {
	keyring.MockInit()
	t.Setenv("LINE_LX_TOKEN", "")
	var status int
	var code, resource string
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/oauth-authorization-server":
			_ = json.NewEncoder(w).Encode(map[string]string{"issuer": srv.URL, "authorization_endpoint": srv.URL + "/authorize",
				"token_endpoint": srv.URL + "/token", "registration_endpoint": srv.URL + "/register"})
		case "/token":
			resource = r.FormValue("resource")
			w.WriteHeader(status)
			if status == http.StatusOK {
				_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "new", "expires_in": 3600})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"error": code})
		}
	}))
	t.Cleanup(srv.Close)

	// A refresh asks for the hub's API, and the new token stays for it.
	status = http.StatusOK
	if err := saveCreds(srv.URL, &creds{ClientID: "c", AccessToken: "old", RefreshToken: "r", Resource: srv.URL + "/api"}); err != nil {
		t.Fatal(err)
	}
	if tok, err := keychainToken(srv.URL); err != nil || tok != "new" || resource != srv.URL+"/api" {
		t.Fatalf("refresh = %q, %v, for resource %q", tok, err, resource)
	}
	if c, _ := loadCreds(srv.URL); c.Resource != srv.URL+"/api" || c.RefreshToken != "r" {
		t.Errorf("refreshed sign-in = %+v", c)
	}

	for _, c := range []struct {
		status int
		code   string
		signIn bool
	}{
		{http.StatusBadRequest, "invalid_grant", true},
		{http.StatusBadRequest, "invalid_target", true},
		{http.StatusUnauthorized, "invalid_client", true}, // the hub no longer knows line-lx's OAuth client
		{http.StatusBadRequest, "invalid_client", true},
		{http.StatusTooManyRequests, "", false}, // a rate limit, not a refusal
		{http.StatusInternalServerError, "server_error", false},
		{http.StatusBadGateway, "", false},
	} {
		status, code = c.status, c.code
		if err := saveCreds(srv.URL, &creds{ClientID: "c", AccessToken: "old", RefreshToken: "r", ExpiresAt: time.Now(), Resource: srv.URL + "/api"}); err != nil {
			t.Fatal(err)
		}
		_, err := keychainToken(srv.URL)
		if signIn := errors.As(err, new(errNotSignedIn)); err == nil || signIn != c.signIn {
			t.Errorf("refresh answered %d %s: err = %v, sign in again = %v", c.status, c.code, err, signIn)
		}
	}
}

func TestEarlierSignInMeansSignIn(t *testing.T) {
	keyring.MockInit()
	t.Setenv("LINE_LX_TOKEN", "")
	hub := "https://hub.example.com"
	// Earlier versions signed in for the hub's MCP server and stored no resource: never sent to the API.
	for _, resource := range []string{"", hub + "/mcp"} {
		if err := saveCreds(hub, &creds{AccessToken: "tok", RefreshToken: "r", ExpiresAt: time.Now().Add(time.Hour), Resource: resource}); err != nil {
			t.Fatal(err)
		}
		tok, err := keychainToken(hub)
		if !errors.As(err, new(errNotSignedIn)) || tok != "" || !strings.Contains(err.Error(), "Run `line-lx login`") {
			t.Errorf("resource %q: token %q, %v", resource, tok, err)
		}
	}
}

// noKeychainHere makes the keyring act as on a machine without one, and puts it back after the test.
func noKeychainHere(t *testing.T) {
	keyring.MockInitWithError(keyring.ErrUnsupportedPlatform)
	t.Cleanup(func() {
		keyring.MockInit()
		noKeychain = false
	})
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", "")
}

func TestSignInFileWithoutKeychain(t *testing.T) {
	noKeychainHere(t)
	hub := "https://hub.example.com"
	if err := saveCreds(hub, &creds{AccessToken: "tok", ExpiresAt: time.Now().Add(time.Hour), Resource: hub + "/api"}); err != nil {
		t.Fatal(err)
	}
	path, _ := credPath(hub)
	for p, want := range map[string]os.FileMode{path: 0o600, filepath.Dir(path): 0o700} {
		if info, err := os.Stat(p); err != nil || info.Mode().Perm() != want {
			t.Errorf("%s: mode %v, %v; want %v", p, info.Mode().Perm(), err, want)
		}
	}
	if tok, err := keychainToken(hub); err != nil || tok != "tok" {
		t.Fatalf("token from the file = %q, %v", tok, err)
	}
	if _, err := loadCreds("https://other.example.com"); !errors.As(err, new(errNotSignedIn)) {
		t.Errorf("another hub's sign-in = %v", err)
	}

	// Others can read it: refused, never used.
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadCreds(hub); err == nil || !strings.Contains(err.Error(), "Other users can read") {
		t.Errorf("a world-readable sign-in file = %v", err)
	}
	// A symlink to a file line-lx would read: refused too.
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(filepath.Dir(path), "elsewhere")
	if err := os.Rename(path, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(moved, path); err != nil {
		t.Fatal(err)
	}
	if _, err := loadCreds(hub); err == nil || !strings.Contains(err.Error(), "isn't a plain file") {
		t.Errorf("a symlinked sign-in file = %v", err)
	}
	// A new sign-in replaces the symlink itself, never what it points to.
	if err := saveCreds(hub, &creds{AccessToken: "new", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(path); err != nil || !info.Mode().IsRegular() || !strings.Contains(read(t, moved), `\"tok\"`) {
		t.Errorf("saving went through the symlink: %v, %v", info, err)
	}

	// Signing out removes it.
	if err := credDelete(hub); err != nil || exists(path) {
		t.Errorf("logout = %v, file left: %v", err, exists(path))
	}
	if err := credDelete(hub); !errors.Is(err, keyring.ErrNotFound) {
		t.Errorf("a second logout = %v", err)
	}
}

func TestKeychainErrorIsNotAMissingKeychain(t *testing.T) {
	locked := errors.New("the keyring is locked")
	if keychainMissing(locked) {
		t.Skip("no Secret Service on this machine: every keyring error means there's no keychain")
	}
	keyring.MockInitWithError(locked)
	t.Cleanup(keyring.MockInit)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", "")
	hub := "https://hub.example.com"
	if err := saveCreds(hub, &creds{AccessToken: "tok"}); err == nil {
		t.Error("a failing keychain was ignored")
	}
	if _, err := loadCreds(hub); err == nil || !strings.Contains(err.Error(), "Can't read the keychain") {
		t.Errorf("load = %v", err)
	}
	if path, _ := credPath(hub); noKeychain || exists(path) {
		t.Error("a keychain that fails sent the sign-in to a file")
	}
}

func TestAuthHeaderStaysOnItsHub(t *testing.T) {
	const hub = "https://hub.example.com/hub"
	t.Setenv("LINE_LX_HUB", hub)
	t.Setenv("LINE_LX_TOKEN", "tok")
	t.Setenv("CLAUDE_CODE_MARKETPLACE_URL", "")
	t.Setenv("CLAUDE_CODE_PLUGIN_ARCHIVE_URL", "")
	run := func() (string, error) {
		var err error
		out := captureStdout(t, func() { err = runAuthHeader() })
		return out, err
	}

	// Only Claude Code asks: it says for which marketplace or archive.
	if out, err := run(); err == nil || !strings.Contains(err.Error(), "only for Claude Code") || out != "" {
		t.Errorf("auth-header outside Claude Code = %q, %v", out, err)
	}
	const want = `{"Authorization":"Bearer tok"}` + "\n"
	archive := "https://HUB.example.com/hub/api/connectors/claude-code/plugins/basics.zip"
	for _, env := range [][2]string{{marketplaceURL(hub), ""}, {"", archive}, {marketplaceURL(hub), archive}} {
		t.Setenv("CLAUDE_CODE_MARKETPLACE_URL", env[0])
		t.Setenv("CLAUDE_CODE_PLUGIN_ARCHIVE_URL", env[1])
		if out, err := run(); err != nil || out != want {
			t.Errorf("auth-header for %v = %q, %v", env, out, err)
		}
	}

	// Every URL Claude Code names must be on the hub: never another origin, nor outside the hub's path.
	for _, v := range []string{"CLAUDE_CODE_MARKETPLACE_URL", "CLAUDE_CODE_PLUGIN_ARCHIVE_URL"} {
		for _, link := range []string{
			"https://evil.example.com/hub/api/connectors/claude-code/marketplace.json",
			"https://hub.example.com.evil.example.com/hub/api/connectors/claude-code/marketplace.json",
			"https://hub.example.com:8443/hub/api/connectors/claude-code/marketplace.json",
			"http://hub.example.com/hub/api/connectors/claude-code/marketplace.json",
			"https://user@hub.example.com/hub/api/connectors/claude-code/marketplace.json",
			"https://hub.example.com/other/api/connectors/claude-code/marketplace.json",
			"https://hub.example.com/hubby/api/connectors/claude-code/marketplace.json",
			"https://hub.example.com/hub/../other/marketplace.json",
			"https://hub.example.com/hub",
			"::not a url",
		} {
			t.Run(v, func(t *testing.T) {
				t.Setenv(v, link)
				if out, err := run(); err == nil || out != "" {
					t.Errorf("%s=%q: printed %q, %v", v, link, out, err)
				}
			})
		}
	}

	// Not signed in: an error, and nothing on stdout.
	keyring.MockInit()
	t.Setenv("CLAUDE_CODE_MARKETPLACE_URL", marketplaceURL(hub))
	t.Setenv("CLAUDE_CODE_PLUGIN_ARCHIVE_URL", "")
	t.Setenv("LINE_LX_TOKEN", "")
	t.Setenv("HOME", t.TempDir())
	if out, err := run(); !errors.As(err, new(errNotSignedIn)) || out != "" {
		t.Errorf("auth-header without a sign-in = %q, %v", out, err)
	}
}

func TestLoginRevokesThePreviousSignIn(t *testing.T) {
	keyring.MockInit()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("LINE_LX_TOKEN", "")
	var revoked url.Values
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/oauth-authorization-server":
			_ = json.NewEncoder(w).Encode(map[string]string{"issuer": srv.URL, "authorization_endpoint": srv.URL + "/authorize",
				"token_endpoint": srv.URL + "/token", "registration_endpoint": srv.URL + "/register", "revocation_endpoint": srv.URL + "/revoke"})
		case "/register":
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{"client_id": "new-client"})
		case "/token":
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "new", "refresh_token": "new-r", "expires_in": 3600})
		case "/revoke":
			_ = r.ParseForm()
			revoked = r.PostForm
		case "/api/me":
			_ = json.NewEncoder(w).Encode(map[string]any{"user": map[string]string{"name": "Ada"}, "org": map[string]string{"name": "Moqa"}})
		}
	}))
	t.Cleanup(srv.Close)
	// The browser: the person signs in, and the hub sends them back to line-lx with a code.
	browser := openBrowser
	t.Cleanup(func() { openBrowser = browser })
	openBrowser = func(u string) {
		asked, _ := url.Parse(u)
		back := asked.Query().Get("redirect_uri") + "?" + url.Values{"code": {"c"}, "state": {asked.Query().Get("state")}}.Encode()
		go func() {
			if res, err := http.Get(back); err == nil {
				res.Body.Close()
			}
		}()
	}

	if err := saveCreds(srv.URL, &creds{ClientID: "old-client", RefreshToken: "old-r", Resource: srv.URL + "/api"}); err != nil {
		t.Fatal(err)
	}
	if _, err := login(srv.URL); err != nil {
		t.Fatal(err)
	}
	if revoked.Get("token") != "old-r" || revoked.Get("client_id") != "old-client" || revoked.Get("token_type_hint") != "refresh_token" {
		t.Errorf("revoked %v, want the previous refresh token", revoked)
	}
	if c, _ := loadCreds(srv.URL); c == nil || c.RefreshToken != "new-r" || c.ClientID != "new-client" {
		t.Errorf("saved sign-in = %+v", c)
	}
}
