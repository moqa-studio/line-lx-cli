package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf16"
)

// The hub's API never redirects; following one could send the bearer token to another host.
var httpClient = &http.Client{
	Timeout:       30 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// limited caps what line-lx reads from any answer, so a hub can't exhaust memory.
func limited(body io.Reader) io.Reader { return io.LimitReader(body, 64<<20) }

// clean drops control and format characters (escape sequences, bidi overrides, zero-width) from a string
// a hub (or `claude`) sent, before it reaches the terminal.
func clean(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, s)
}

// hubClient calls the hub's API as the signed-in person.
type hubClient struct {
	hub   string
	token string
}

type me struct {
	User      struct{ Name, Email string } `json:"user"`
	Org       struct{ Name, Slug string }  `json:"org"`
	Role      string                       `json:"role"`
	Endpoints struct {
		MCP string `json:"mcp"` // the MCP server's URL, for MCP clients
	} `json:"endpoints"`
}

type plugin struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type skillFile struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

type skill struct {
	Name    string      `json:"name"`
	Content string      `json:"content"`
	Files   []skillFile `json:"files"`
}

type pluginVersion struct {
	ID       string `json:"id"`
	Version  string `json:"version"`
	Snapshot struct {
		Skills []skill `json:"skills"`
	} `json:"snapshot"`
}

// release is a plugin's newest published version, what `sync` delivers.
type release struct {
	name    string
	version pluginVersion
}

// apiError is the hub's answer to a failed request.
type apiError struct {
	status  int
	message string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("The hub answered %d: %s", e.status, clean(e.message))
}

// newHubClient resolves the current hub and the person's token.
func newHubClient() (*hubClient, error) {
	hub, err := currentHub()
	if err != nil {
		return nil, err
	}
	token, err := bearer(hub)
	if err != nil {
		return nil, err
	}
	return &hubClient{hub: hub, token: token}, nil
}

// errSignInRefused: the hub answered 401 to the person's token, so they must sign in again.
type errSignInRefused struct{ hub string }

func (e errSignInRefused) Error() string {
	return fmt.Sprintf("The hub at %s didn't accept your sign-in. Run `line-lx login`.", e.hub)
}

// do sends a JSON request and decodes the answer into out (when not nil).
func (c *hubClient) do(method, path string, in, out any) error {
	var data []byte
	if in != nil {
		data, _ = json.Marshal(in)
	}
	res, err := c.send(method, c.hub+path, data)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(limited(res.Body)).Decode(out); err != nil {
		return fmt.Errorf("The hub's answer to %s isn't valid JSON: %v", path, err)
	}
	return nil
}

// send sends a request with the person's token and returns a successful answer, whose body the caller
// closes. A 429 whose Retry-After is at most 30 seconds is waited out and the request sent once more.
func (c *hubClient) send(method, target string, data []byte) (*http.Response, error) {
	var res *http.Response
	for try := 0; ; try++ {
		var body io.Reader
		if data != nil {
			body = bytes.NewReader(data)
		}
		req, err := http.NewRequest(method, target, body)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+c.token)
		if data != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if res, err = httpClient.Do(req); err != nil {
			return nil, fmt.Errorf("Can't reach the hub at %s: %v", c.hub, err)
		}
		wait, err := strconv.Atoi(res.Header.Get("Retry-After"))
		if try > 0 || res.StatusCode != http.StatusTooManyRequests || err != nil || wait < 0 || wait > 30 {
			break
		}
		res.Body.Close()
		time.Sleep(time.Duration(wait) * time.Second)
	}
	if res.StatusCode == http.StatusUnauthorized {
		res.Body.Close()
		return nil, errSignInRefused{c.hub}
	}
	if res.StatusCode >= 300 {
		defer res.Body.Close()
		var e struct {
			Error struct{ Message string } `json:"error"`
		}
		_ = json.NewDecoder(limited(res.Body)).Decode(&e)
		if e.Error.Message == "" {
			e.Error.Message = http.StatusText(res.StatusCode)
		}
		return nil, &apiError{res.StatusCode, e.Error.Message}
	}
	return res, nil
}

// download fetches a file the hub links to, such as a plugin archive, with the person's token. The link
// must be on the hub itself (its scheme, host and path prefix), so the token never goes anywhere else.
// It reads at most max bytes.
func (c *hubClient) download(link string, max int64) ([]byte, error) {
	u, err := url.Parse(link)
	hub, _ := url.Parse(c.hub)
	if err != nil || u.Scheme != hub.Scheme || !strings.EqualFold(u.Host, hub.Host) || u.User != nil ||
		!strings.HasPrefix(u.EscapedPath(), hub.EscapedPath()+"/") || slices.Contains(strings.Split(u.Path, "/"), "..") {
		return nil, fmt.Errorf("the hub links to %q, which isn't on the hub, so line-lx won't download it", clean(link))
	}
	res, err := c.send("GET", u.String(), nil)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, max+1))
	if err != nil {
		return nil, fmt.Errorf("can't download %s: %v", clean(link), err)
	}
	if int64(len(data)) > max {
		return nil, fmt.Errorf("%s is over %d MiB", clean(link), max>>20)
	}
	return data, nil
}

// releases returns each plugin's newest version; plugins never published are skipped.
func (c *hubClient) releases() ([]release, error) {
	var plugins []plugin
	if err := c.do("GET", "/api/plugins", nil, &plugins); err != nil {
		return nil, err
	}
	var out []release
	for _, p := range plugins {
		var versions []pluginVersion // the newest only
		if err := c.do("GET", "/api/plugins/"+url.PathEscape(p.ID)+"/versions?limit=1", nil, &versions); err != nil {
			return nil, err
		}
		if len(versions) > 0 {
			out = append(out, release{name: p.Name, version: versions[0]})
		}
	}
	return out, nil
}

// report records a delivery. A delivered one is a single POST. The hub treats any other POSTed status
// as final and refuses a later PATCH, so a partial or failed one is POSTed as pending, then PATCHed
// with its status and what the target couldn't take or why it failed.
func (c *hubClient) report(versionID, target, status string, unsupported []string, errMsg string) error {
	post := map[string]string{"versionId": versionID, "target": target}
	if status == "delivered" {
		post["status"] = status
	}
	var d struct {
		ID string `json:"id"`
	}
	if err := c.do("POST", "/api/deliveries", post, &d); err != nil {
		return err
	}
	if status == "delivered" {
		return nil
	}
	patch := map[string]any{"status": status}
	if len(unsupported) > 0 {
		patch["unsupported"] = capUnsupported(unsupported)
	}
	if errMsg != "" {
		patch["error"] = cut(errMsg, 4000)
	}
	return c.do("PATCH", "/api/deliveries/"+url.PathEscape(d.ID), patch, nil)
}

// The hub refuses a report over its limits and the delivery stays
// pending: at most 100 unsupported entries of at most 200 characters, and an error of at most 4000.
func capUnsupported(unsupported []string) []string {
	out := make([]string, 0, min(len(unsupported), 100))
	for _, u := range unsupported[:min(len(unsupported), 100)] {
		out = append(out, cut(u, 200))
	}
	if len(unsupported) > 100 {
		out[99] = fmt.Sprintf("… and %d more", len(unsupported)-99)
	}
	return out
}

// cut keeps at most n characters of s, counted as the hub's JavaScript counts them (UTF-16 code units),
// and cuts on a rune boundary.
func cut(s string, n int) string {
	units := 0
	for i, r := range s {
		if units += utf16.RuneLen(r); units > n {
			return s[:i]
		}
	}
	return s
}

func runWhoami() error {
	c, err := newHubClient()
	if err != nil {
		return err
	}
	var m me
	if err := c.do("GET", "/api/me", nil, &m); err != nil {
		return err
	}
	fmt.Printf("%s %s\nworkspace  %s (%s)\nrole       %s\nhub        %s\n",
		clean(m.User.Name), clean(m.User.Email), clean(m.Org.Name), clean(m.Org.Slug), clean(m.Role), c.hub)
	return nil
}
