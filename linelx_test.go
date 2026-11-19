package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf16"
)

func TestSafePath(t *testing.T) {
	for p, want := range map[string]bool{
		"refs/a.md":     true,
		"script.sh":     true,
		"":              false,
		"SKILL.md":      false,
		"/etc/passwd":   false,
		"../evil":       false,
		"a/../../evil":  false,
		"a//b":          false,
		"a/":            false,
		"./a":           false,
		`a\b`:           false,
		"C:/Windows":    false,
		"refs/SKILL.md": true, // only the skill's own SKILL.md is reserved
		"skill.md":      false,
		"a\x00b":        false,
	} {
		if got := safePath(p); got != want {
			t.Errorf("safePath(%q) = %v, want %v", p, got, want)
		}
	}
}

func TestNeedsRefresh(t *testing.T) {
	now := time.Now()
	if needsRefresh(&creds{ExpiresAt: now.Add(10 * time.Minute)}, now) {
		t.Error("a token valid for 10 minutes doesn't need a refresh")
	}
	if !needsRefresh(&creds{ExpiresAt: now.Add(30 * time.Second)}, now) {
		t.Error("a token expiring within a minute needs a refresh")
	}
	if !needsRefresh(&creds{ExpiresAt: now.Add(-time.Hour)}, now) {
		t.Error("an expired token needs a refresh")
	}
}

func testRelease(id, v string, skills ...skill) release {
	r := release{name: "demo", version: pluginVersion{ID: id, Version: v}}
	r.version.Snapshot.Skills = skills
	return r
}

func read(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestWriteNested(t *testing.T) {
	dir := t.TempDir()
	tg := fileTarget{name: "pi", home: dir, dir: filepath.Join(dir, "skills")}
	r := testRelease("v1", "1.0.0", skill{Name: "greet", Content: "hi", Files: []skillFile{{"refs/a.md", "A"}, {"../x", "X"}}})

	unsupported, err := tg.write("http://hub", r)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{`skill greet: file "../x" (unsafe path)`}; !reflect.DeepEqual(unsupported, want) {
		t.Errorf("unsupported = %v, want %v", unsupported, want)
	}
	if got := read(t, filepath.Join(tg.dir, "demo", "greet", "SKILL.md")); got != "hi" {
		t.Errorf("SKILL.md = %q", got)
	}
	if got := read(t, filepath.Join(tg.dir, "demo", "greet", "refs", "a.md")); got != "A" {
		t.Errorf("refs/a.md = %q", got)
	}
	if s := tg.stamps()["demo"]; s.VersionID != "v1" || s.Hub != "http://hub" {
		t.Errorf("stamp = %+v", s)
	}

	// A new version replaces the folder whole: the old skill is gone.
	if _, err := tg.write("http://hub", testRelease("v2", "2.0.0", skill{Name: "other", Content: "o"})); err != nil {
		t.Fatal(err)
	}
	if exists(filepath.Join(tg.dir, "demo", "greet")) {
		t.Error("the previous version's skill is still there")
	}
	entries, _ := os.ReadDir(tg.dir)
	if len(entries) != 1 { // only the plugin: staging lives outside the skills folder
		t.Errorf("leftovers next to the plugin: %v", entries)
	}
	if staged, _ := os.ReadDir(tg.staging()); len(staged) != 0 {
		t.Errorf("leftovers in staging: %v", staged)
	}

	if err := tg.remove("demo", tg.stamps()["demo"]); err != nil || exists(filepath.Join(tg.dir, "demo")) {
		t.Errorf("remove: %v", err)
	}
}

func TestWriteFlat(t *testing.T) {
	dir := t.TempDir()
	tg := fileTarget{name: "codex", home: dir, dir: filepath.Join(dir, "skills"), flat: true}
	if _, err := tg.write("http://hub", testRelease("v1", "1.0.0", skill{Name: "a", Content: "A"}, skill{Name: "b", Content: "B"})); err != nil {
		t.Fatal(err)
	}
	if got := read(t, filepath.Join(tg.dir, "demo--b", "SKILL.md")); got != "B" {
		t.Errorf("demo--b/SKILL.md = %q", got)
	}
	if s := tg.stamps()["demo"]; !reflect.DeepEqual(s.Skills, []string{"a", "b"}) {
		t.Errorf("stamp skills = %v", s.Skills)
	}

	if _, err := tg.write("http://hub", testRelease("v2", "2.0.0", skill{Name: "a", Content: "A2"})); err != nil {
		t.Fatal(err)
	}
	if exists(filepath.Join(tg.dir, "demo--b")) || read(t, filepath.Join(tg.dir, "demo--a", "SKILL.md")) != "A2" {
		t.Error("the flat layout didn't follow the new version")
	}

	if err := tg.remove("demo", tg.stamps()["demo"]); err != nil || exists(filepath.Join(tg.dir, "demo--a")) || len(tg.stamps()) != 0 {
		t.Errorf("remove: %v", err)
	}
}

func TestWriteNeverTakesAFolderItDoesntOwn(t *testing.T) {
	dir := t.TempDir()
	nested := fileTarget{name: "pi", home: dir, dir: filepath.Join(dir, "pi")}
	flat := fileTarget{name: "codex", home: dir, dir: filepath.Join(dir, "codex"), flat: true}
	for _, tg := range []fileTarget{nested, flat} {
		mine := filepath.Join(tg.dir, "demo")
		if tg.flat {
			mine = filepath.Join(tg.dir, "demo--greet")
		}
		if err := writeFile(filepath.Join(mine, "SKILL.md"), "someone's"); err != nil {
			t.Fatal(err)
		}
		if _, err := tg.write("http://hub", testRelease("v1", "1.0.0", skill{Name: "greet", Content: "ours"})); err == nil {
			t.Errorf("%s: overwrote a folder without our stamp", tg.name)
		}
		if read(t, filepath.Join(mine, "SKILL.md")) != "someone's" {
			t.Errorf("%s: touched a folder without our stamp", tg.name)
		}
	}
}

func TestHubClient(t *testing.T) {
	var reports []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if ua := r.Header.Get("User-Agent"); ua != "line-lx/"+version { // the hub tells line-lx from Claude Code
			w.WriteHeader(http.StatusTeapot)
			fmt.Fprintf(w, `{"error":{"message":"User-Agent %q"}}`, ua)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/versions") && r.URL.RawQuery != "limit=1" {
			w.WriteHeader(http.StatusBadRequest) // line-lx needs the newest version only
			return
		}
		switch r.Method + " " + r.URL.Path {
		case "GET /api/plugins":
			w.Write([]byte(`[{"id":"p1","name":"basics"},{"id":"p2","name":"draft"}]`))
		case "GET /api/plugins/p1/versions":
			w.Write([]byte(`[{"id":"v2","version":"1.1.0","snapshot":{"skills":[{"name":"s","content":"c","files":[]}]}},{"id":"v1","version":"1.0.0"}]`))
		case "GET /api/plugins/p2/versions":
			w.Write([]byte(`[]`))
		case "POST /api/deliveries", "PATCH /api/deliveries/d1":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			reports = append(reports, body)
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"id":"d1"}`))
		default:
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"error":{"code":"forbidden","message":"Not yours"}}`))
		}
	}))
	defer srv.Close()

	c := &hubClient{hub: srv.URL, token: "tok"}
	rels, err := c.releases()
	if err != nil {
		t.Fatal(err)
	}
	if len(rels) != 1 || rels[0].name != "basics" || rels[0].version.ID != "v2" || rels[0].version.Snapshot.Skills[0].Content != "c" {
		t.Errorf("releases = %+v (want only basics, newest version)", rels)
	}

	if err := c.report("v2", "pi", "partial", []string{"x"}, ""); err != nil {
		t.Fatal(err)
	}
	// The hub refuses to PATCH a delivery POSTed with a final status, so a partial one is POSTed pending.
	if len(reports) != 2 || reports[0]["status"] != nil || reports[1]["status"] != "partial" || reports[1]["unsupported"] == nil {
		t.Errorf("reports = %v (want a POST without status, then a PATCH with status and unsupported)", reports)
	}
	reports = nil
	if err := c.report("v2", "pi", "delivered", nil, ""); err != nil {
		t.Fatal(err)
	}
	if len(reports) != 1 || reports[0]["status"] != "delivered" {
		t.Errorf("reports = %v (want a single POST, delivered)", reports)
	}

	var ae *apiError
	if err := c.do("GET", "/api/other", nil, nil); !errors.As(err, &ae) || ae.message != "Not yours" {
		t.Errorf("error = %v, want the hub's message", err)
	}
	if err := (&hubClient{hub: srv.URL, token: "bad"}).do("GET", "/api/plugins", nil, nil); err == nil {
		t.Error("a refused token must be an error")
	}
}

func TestReportStaysWithinTheHubLimits(t *testing.T) {
	var patch struct {
		Unsupported []string
		Error       string
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PATCH" {
			_ = json.NewDecoder(r.Body).Decode(&patch)
		}
		w.Write([]byte(`{"id":"d1"}`))
	}))
	defer srv.Close()

	long := strings.Repeat("é", 150) + strings.Repeat("😀", 100) // 350 UTF-16 code units
	unsupported := make([]string, 150)
	for i := range unsupported {
		unsupported[i] = long
	}
	c := &hubClient{hub: srv.URL, token: "tok"}
	if err := c.report("v1", "pi", "failed", unsupported, strings.Repeat("😀", 3000)); err != nil {
		t.Fatal(err)
	}
	units := func(s string) int { return len(utf16.Encode([]rune(s))) }
	if len(patch.Unsupported) != 100 || patch.Unsupported[99] != "… and 51 more" {
		t.Fatalf("unsupported: %d entries, last %q", len(patch.Unsupported), patch.Unsupported[len(patch.Unsupported)-1])
	}
	if got := patch.Unsupported[0]; got != strings.Repeat("é", 150)+strings.Repeat("😀", 25) || units(got) != 200 {
		t.Errorf("unsupported[0] has %d code units, cut at %q", units(got), got[len(got)-8:])
	}
	if units(patch.Error) != 4000 || patch.Error != strings.Repeat("😀", 2000) {
		t.Errorf("error has %d code units", units(patch.Error))
	}
}

func TestRetryAfterRateLimit(t *testing.T) {
	var calls int
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		data, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(data))
		if calls == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"error":{"code":"rate_limited","message":"Too many requests, retry in 1 s"}}`))
			return
		}
		w.Write([]byte(`{"id":"d1"}`))
	}))
	defer srv.Close()

	c := &hubClient{hub: srv.URL, token: "tok"}
	var d struct{ ID string }
	if err := c.do("POST", "/api/deliveries", map[string]string{"target": "pi"}, &d); err != nil || d.ID != "d1" {
		t.Fatalf("do = %v, %+v (want the retry's answer)", err, d)
	}
	if calls != 2 || bodies[1] != bodies[0] || bodies[1] == "" {
		t.Errorf("calls = %d, bodies = %q (want the same body sent twice)", calls, bodies)
	}

	// A second 429 in a row is the caller's to see.
	calls = 0
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	var ae *apiError
	if err := c.do("GET", "/api/me", nil, nil); !errors.As(err, &ae) || ae.status != http.StatusTooManyRequests || calls != 2 {
		t.Errorf("error = %v after %d calls, want a 429 after 2", err, calls)
	}
}
