package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// Hook inputs as Claude Code 2.1.311 sends them, ids and paths replaced. CWD is the folder the session runs in.
const (
	hookSkill       = `{"session_id":"0b6f5a52-6f2c-4a59-9d0e-3d1f1b0c8e11","transcript_path":"/home/dev/.claude/projects/-work-site/0b6f5a52-6f2c-4a59-9d0e-3d1f1b0c8e11.jsonl","cwd":"CWD","prompt_id":"5c3f8a1e-2b7d-4e9a-8f61-0a2b3c4d5e6f","permission_mode":"default","effort":{"level":"medium"},"hook_event_name":"PostToolUse","tool_name":"Skill","tool_input":{"skill":"SKILL"},"tool_response":{"success":true,"commandName":"SKILL"},"tool_use_id":"toolu_01AbCdEfGhIjKlMnOpQrStUv","duration_ms":6}`
	hookSkillPre    = `{"session_id":"0b6f5a52-6f2c-4a59-9d0e-3d1f1b0c8e11","transcript_path":"/home/dev/.claude/projects/-work-site/0b6f5a52-6f2c-4a59-9d0e-3d1f1b0c8e11.jsonl","cwd":"CWD","prompt_id":"5c3f8a1e-2b7d-4e9a-8f61-0a2b3c4d5e6f","permission_mode":"default","effort":{"level":"medium"},"hook_event_name":"PreToolUse","tool_name":"Skill","tool_input":{"skill":"SKILL"},"tool_use_id":"toolu_01AbCdEfGhIjKlMnOpQrStUv"}`
	hookSkillFailed = `{"session_id":"0b6f5a52-6f2c-4a59-9d0e-3d1f1b0c8e11","transcript_path":"/home/dev/.claude/projects/-work-site/0b6f5a52.jsonl","cwd":"CWD","permission_mode":"default","hook_event_name":"PostToolUseFailure","tool_name":"Skill","tool_input":{"skill":"SKILL","args":"secret plans"},"tool_use_id":"toolu_01AbCdEfGhIjKlMnOpQrStUv","error":"Skill failed","is_interrupt":false,"duration_ms":4}`
	hookTyped       = `{"session_id":"0b6f5a52-6f2c-4a59-9d0e-3d1f1b0c8e11","transcript_path":"/home/dev/.claude/projects/-work-site/0b6f5a52.jsonl","cwd":"CWD","permission_mode":"default","hook_event_name":"UserPromptExpansion","expansion_type":"slash_command","command_name":"SKILL","command_args":"secret plans","command_source":"plugin","prompt":"/SKILL secret plans"}`
	hookMCP         = `{"session_id":"0b6f5a52-6f2c-4a59-9d0e-3d1f1b0c8e11","transcript_path":"/home/dev/.claude/projects/-work-site/0b6f5a52.jsonl","cwd":"CWD","permission_mode":"default","hook_event_name":"PostToolUse","tool_name":"SKILL","tool_input":{"query":"select * from customers","skill":5},"tool_response":[{"type":"text","text":"42 rows"}],"mcp_server":{"name":"plugin:notes:db","source":"plugin"},"tool_use_id":"toolu_01AbCdEfGhIjKlMnOpQrStUv","duration_ms":31}`
	hookMCPFailed   = `{"session_id":"0b6f5a52-6f2c-4a59-9d0e-3d1f1b0c8e11","transcript_path":"/home/dev/.claude/projects/-work-site/0b6f5a52.jsonl","cwd":"CWD","permission_mode":"default","hook_event_name":"PostToolUseFailure","tool_name":"SKILL","tool_input":{"query":"drop table customers"},"mcp_server":{"name":"plugin:notes:db","source":"plugin"},"tool_use_id":"toolu_01AbCdEfGhIjKlMnOpQrStUv","error":"permission denied","duration_ms":12}`
	hookBash        = `{"session_id":"0b6f5a52-6f2c-4a59-9d0e-3d1f1b0c8e11","cwd":"CWD","hook_event_name":"PostToolUse","tool_name":"Bash","tool_input":{"command":"echo hi","description":"Say hi"},"tool_response":{"stdout":"hi","stderr":"","interrupted":false},"tool_use_id":"toolu_01AbCdEfGhIjKlMnOpQrStUv"}`
)

func hookJSON(template, name, cwd string) string {
	return strings.NewReplacer("SKILL", name, "CWD", cwd).Replace(template)
}

func TestReadUsage(t *testing.T) {
	for _, tc := range []struct {
		name, input string
		want        usage
		ok          bool
	}{
		{"Claude runs a skill", hookJSON(hookSkill, "website-creation", "/w"), usage{kind: "activated", trigger: "agent", success: true, skill: "website-creation"}, true},
		{"Claude runs a skill that says it failed", strings.Replace(hookJSON(hookSkill, "notes:jot", "/w"), `"success":true`, `"success":false`, 1), usage{kind: "activated", trigger: "agent", skill: "notes:jot"}, true},
		{"a skill's answer without success", strings.Replace(hookJSON(hookSkill, "jot", "/w"), `{"success":true,"commandName":"jot"}`, `"done"`, 1), usage{kind: "activated", trigger: "agent", success: true, skill: "jot"}, true},
		{"a skill fails", hookJSON(hookSkillFailed, "jot", "/w"), usage{kind: "failed", trigger: "agent", skill: "jot"}, true},
		{"the person types a skill", hookJSON(hookTyped, "notes:jot", "/w"), usage{kind: "activated", trigger: "person", success: true, skill: "notes:jot"}, true},
		{"a plugin's MCP tool", hookJSON(hookMCP, "mcp__plugin_notes_db__query", "/w"), usage{kind: "mcp_call", trigger: "agent", success: true, tool: "mcp__plugin_notes_db__query"}, true},
		{"a plugin's MCP tool fails", hookJSON(hookMCPFailed, "mcp__plugin_notes_db__query", "/w"), usage{kind: "failed", trigger: "agent", tool: "mcp__plugin_notes_db__query"}, true},
		{"an MCP tool whose server isn't a plugin's", strings.Replace(hookJSON(hookMCP, "mcp__plugin_notes_db__query", "/w"), `"source":"plugin"`, `"source":"user"`, 1), usage{}, false},
		{"an MCP tool of another server", hookJSON(hookMCP, "mcp__github__search", "/w"), usage{}, false},
		{"before the skill runs", hookJSON(hookSkillPre, "jot", "/w"), usage{}, false},
		{"another tool", hookJSON(hookBash, "", "/w"), usage{}, false},
		{"an MCP prompt", strings.Replace(hookJSON(hookTyped, "notes:jot", "/w"), "slash_command", "mcp_prompt", 1), usage{}, false},
		{"a Skill call without a skill", strings.Replace(hookJSON(hookSkill, "", "/w"), `"skill":""`, `"skill":7`, 1), usage{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var in hookInput
			if err := json.Unmarshal([]byte(tc.input), &in); err != nil {
				t.Fatal(err)
			}
			got, ok := readUsage(in)
			if ok != tc.ok || got != tc.want {
				t.Errorf("readUsage = %+v, %v; want %+v, %v", got, ok, tc.want, tc.ok)
			}
		})
	}
}

// usageHub records the usage reports it gets, and answers status (202 by default).
type usageHub struct {
	*httptest.Server
	mu      sync.Mutex
	status  int
	delay   time.Duration
	reports []string
}

func newUsageHub(t *testing.T) *usageHub {
	h := &usageHub{status: http.StatusAccepted}
	h.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		status, delay := h.status, h.delay
		h.mu.Unlock()
		time.Sleep(delay)
		if r.Method != "POST" || r.URL.Path != "/api/usage/reports" || r.Header.Get("Authorization") != "Bearer tok" ||
			r.Header.Get("Content-Type") != "application/json" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		body, _ := io.ReadAll(r.Body)
		h.mu.Lock()
		h.reports = append(h.reports, string(body))
		h.mu.Unlock()
		w.WriteHeader(status)
	}))
	t.Cleanup(h.Close)
	return h
}

func (h *usageHub) took() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.reports...)
}

func putFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

// usageMachine is a machine where Claude Code installed, from the hub's marketplace metro-dev, the plugin
// notes 1.2.0 (skills jot, board-apple-ads whose SKILL.md names it apple-ads-board, summary whose SKILL.md
// names it notes:sum, and the MCP server db),
// and from another marketplace the plugin other (skill lint). The person has a personal skill hello and a
// project with the skill deploy. It returns the project's folder.
func usageMachine(t *testing.T, hub string) string {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("LINE_LX_HUB", hub)
	t.Setenv("LINE_LX_TOKEN", "tok")
	claude := filepath.Join(home, ".claude")
	cache := filepath.Join(claude, "plugins", "cache")
	notes := filepath.Join(cache, "metro-dev", "notes", "1.2.0")
	other := filepath.Join(cache, "elsewhere", "other", "0.1.0")
	bundle := filepath.Join(cache, "metro-dev", "metro-lx", "13cd9935af1d")

	putFile(t, filepath.Join(claude, "plugins", "known_marketplaces.json"), `{
  "metro-dev": {"source": {"source": "url", "url": "`+marketplaceURL(hub)+`", "headersHelper": "/opt/homebrew/bin/line-lx auth-header"}, "installLocation": "`+claude+`/plugins/marketplaces/metro-dev", "autoUpdate": true},
  "elsewhere": {"source": {"source": "github", "repo": "someone/plugins"}, "installLocation": "`+claude+`/plugins/marketplaces/elsewhere"}
}`)
	putFile(t, filepath.Join(claude, "plugins", "installed_plugins.json"), `{"version": 2, "plugins": {
  "notes@metro-dev": [{"scope": "user", "installPath": "`+notes+`", "version": "1.2.0"}],
  "metro-lx@metro-dev": [{"scope": "user", "installPath": "`+bundle+`", "version": "13cd9935af1d"}],
  "other@elsewhere": [{"scope": "user", "installPath": "`+other+`", "version": "0.1.0"}]
}}`)
	putFile(t, filepath.Join(notes, ".claude-plugin", "plugin.json"), `{"name": "notes", "version": "1.2.0", "description": ""}`)
	putFile(t, filepath.Join(notes, "skills", "jot", "SKILL.md"), "---\nname: jot\ndescription: Jot a note\n---\nJot.\n")
	putFile(t, filepath.Join(notes, "skills", "board-apple-ads", "SKILL.md"), "---\nname: apple-ads-board\ndescription: Ads\n---\nAds.\n")
	putFile(t, filepath.Join(notes, "skills", "summary", "SKILL.md"), "---\nname: notes:sum\n---\nSum up.\n")
	putFile(t, filepath.Join(notes, ".mcp.json"), `{"mcpServers": {"db": {"command": "db-server"}}}`)
	putFile(t, filepath.Join(bundle, ".claude-plugin", "plugin.json"), `{"name": "metro-lx", "dependencies": ["notes"]}`)
	putFile(t, filepath.Join(other, "skills", "lint", "SKILL.md"), "---\nname: lint\n---\n")
	putFile(t, filepath.Join(other, ".mcp.json"), `{"mcpServers": {"db": {"command": "other-db"}}}`)
	putFile(t, filepath.Join(claude, "skills", "hello", "SKILL.md"), "---\nname: hello\n---\n")

	project := filepath.Join(home, "work", "site")
	t.Setenv("CLAUDE_PROJECT_DIR", project)
	putFile(t, filepath.Join(project, ".claude", "skills", "deploy", "SKILL.md"), "---\nname: deploy\n---\n")
	putFile(t, filepath.Join(project, "apps", "web", "index.html"), "<p>hi</p>\n")
	if err := os.MkdirAll(filepath.Join(project, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	return project
}

func report(plugin, version, name, kind, trigger string, success bool) string {
	data, _ := json.Marshal(usageReport{"dev", "claude-code", kind, plugin, version, name, trigger, success})
	return string(data)
}

func TestReportUsage(t *testing.T) {
	hub := newUsageHub(t)
	project := usageMachine(t, hub.URL)
	src := filepath.Join(project, "src")
	for _, tc := range []struct {
		name, input string
		want        string // "": nothing leaves the machine
	}{
		{"Claude runs a workspace skill", hookJSON(hookSkill, "notes:jot", src), report("notes", "1.2.0", "jot", "activated", "agent", true)},
		{"Claude runs it by its SKILL.md's name", hookJSON(hookSkill, "notes:apple-ads-board", src), report("notes", "1.2.0", "board-apple-ads", "activated", "agent", true)},
		{"a SKILL.md's name with the plugin's prefix", hookJSON(hookSkill, "notes:sum", src), report("notes", "1.2.0", "summary", "activated", "agent", true)},
		{"a workspace skill fails", hookJSON(hookSkillFailed, "notes:jot", src), report("notes", "1.2.0", "jot", "failed", "agent", false)},
		{"the person types a workspace skill", hookJSON(hookTyped, "notes:jot", src), report("notes", "1.2.0", "jot", "activated", "person", true)},
		{"a workspace MCP tool", hookJSON(hookMCP, "mcp__plugin_notes_db__query", src), report("notes", "1.2.0", "db__query", "mcp_call", "agent", true)},
		{"a workspace MCP tool fails", hookJSON(hookMCPFailed, "mcp__plugin_notes_db__query", src), report("notes", "1.2.0", "db__query", "failed", "agent", false)},

		{"a workspace skill by its bare name", hookJSON(hookSkill, "jot", src), report("notes", "1.2.0", "jot", "activated", "agent", true)},
		{"a workspace skill by its bare SKILL.md name", hookJSON(hookSkill, "apple-ads-board", src), report("notes", "1.2.0", "board-apple-ads", "activated", "agent", true)},
		{"a workspace skill typed by its bare name", hookJSON(hookTyped, "jot", src), report("notes", "1.2.0", "jot", "activated", "person", true)},
		{"a bare name without the hook's folder", hookJSON(hookSkill, "jot", ""), ""},
		{"a personal skill", hookJSON(hookSkill, "hello", src), ""},
		{"a personal skill, typed", hookJSON(hookTyped, "hello", src), ""},
		{"a project skill", hookJSON(hookSkill, "deploy", src), ""},
		{"a project skill, typed", hookJSON(hookTyped, "deploy", src), ""},
		{"another marketplace's skill", hookJSON(hookSkill, "lint", src), ""},
		{"another marketplace's skill, by its plugin", hookJSON(hookSkill, "other:lint", src), ""},
		{"another marketplace's MCP tool", hookJSON(hookMCP, "mcp__plugin_other_db__query", src), ""},
		{"a server the plugin doesn't declare", hookJSON(hookMCP, "mcp__plugin_notes_files__read", src), ""},
		{"a skill the plugin doesn't have", hookJSON(hookSkill, "notes:nope", src), ""},
		{"a path for a name", hookJSON(hookSkill, "notes:../../../skills/hello", src), ""},
		{"the bundle", hookJSON(hookSkill, "metro-lx:jot", src), ""},
		{"a built-in command", hookJSON(hookTyped, "help", src), ""},
		{"another tool", hookJSON(hookBash, "", src), ""},
		{"not JSON", "secret plans", ""},
		{"too large", hookJSON(hookSkill, "notes:jot", src) + strings.Repeat(" ", maxHookInput), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := len(hub.took())
			if err := runReportUsage("claude-code", "dev", strings.NewReader(tc.input)); err != nil {
				t.Fatal(err)
			}
			var want []string
			if tc.want != "" {
				want = []string{tc.want}
			}
			if got := hub.took()[before:]; !reflect.DeepEqual(got, want) && !(len(got) == 0 && len(want) == 0) {
				t.Errorf("sent %v, want %v", got, want)
			}
		})
	}
	if d := readDropped(); d.Count != 0 {
		t.Errorf("dropped = %+v", d)
	}
}

// A bare name counts only when the workspace's skill is the one skill or command Claude Code can find under
// it, and when line-lx could look everywhere. Its plugin's name always counts.
func TestReportUsageBareNames(t *testing.T) {
	skill := "---\nname: jot\n---\n"
	for _, tc := range []struct {
		name string
		add  func(t *testing.T, home, project, managed string)
	}{
		{"a personal skill", func(t *testing.T, home, _, _ string) {
			putFile(t, filepath.Join(home, ".claude", "skills", "jot", "SKILL.md"), skill)
		}},
		{"a personal skill named so in its SKILL.md", func(t *testing.T, home, _, _ string) {
			putFile(t, filepath.Join(home, ".claude", "skills", "my-notes", "SKILL.md"), skill)
		}},
		{"a personal command", func(t *testing.T, home, _, _ string) {
			putFile(t, filepath.Join(home, ".claude", "commands", "jot.md"), "Jot.\n")
		}},
		{"a personal skills-directory plugin", func(t *testing.T, home, _, _ string) {
			putFile(t, filepath.Join(home, ".claude", "skills", "kit", ".claude-plugin", "plugin.json"), `{"name": "kit"}`)
			putFile(t, filepath.Join(home, ".claude", "skills", "kit", "skills", "jot", "SKILL.md"), skill)
		}},
		{"a project skill", func(t *testing.T, _, project, _ string) {
			putFile(t, filepath.Join(project, ".claude", "skills", "jot", "SKILL.md"), skill)
		}},
		{"a project command", func(t *testing.T, _, project, _ string) {
			putFile(t, filepath.Join(project, ".claude", "commands", "jot.md"), "Jot.\n")
		}},
		{"a skill above the project", func(t *testing.T, _, project, _ string) {
			putFile(t, filepath.Join(filepath.Dir(project), ".claude", "skills", "jot", "SKILL.md"), skill)
		}},
		{"a nested skill", func(t *testing.T, _, project, _ string) {
			putFile(t, filepath.Join(project, "apps", "web", ".claude", "skills", "jot", "SKILL.md"), skill)
		}},
		{"a managed skill", func(t *testing.T, _, _, managed string) {
			putFile(t, filepath.Join(managed, ".claude", "skills", "jot", "SKILL.md"), skill)
		}},
		{"an additional directory in the person's settings", func(t *testing.T, home, _, _ string) {
			putFile(t, filepath.Join(home, ".claude", "settings.json"), `{"permissions": {"additionalDirectories": ["~/shared"]}}`)
			putFile(t, filepath.Join(home, "shared", ".claude", "skills", "jot", "SKILL.md"), skill)
		}},
		{"an additional directory in the project's local settings", func(t *testing.T, _, project, _ string) {
			putFile(t, filepath.Join(project, ".claude", "settings.local.json"), `{"permissions": {"additionalDirectories": ["../lib"]}}`)
			putFile(t, filepath.Join(filepath.Dir(project), "lib", ".claude", "commands", "jot.md"), "Jot.\n")
		}},
		{"an additional directory in managed settings", func(t *testing.T, home, _, managed string) {
			putFile(t, filepath.Join(managed, "managed-settings.json"), `{"permissions": {"additionalDirectories": ["`+filepath.Join(home, "corp")+`"]}}`)
			putFile(t, filepath.Join(home, "corp", ".claude", "skills", "jot", "SKILL.md"), skill)
		}},
		{"another marketplace's plugin", func(t *testing.T, home, _, _ string) {
			putFile(t, filepath.Join(home, ".claude", "plugins", "cache", "elsewhere", "other", "0.1.0", "skills", "jot", "SKILL.md"), skill)
		}},
		{"another workspace plugin", func(t *testing.T, home, _, _ string) {
			tools := filepath.Join(home, ".claude", "plugins", "cache", "metro-dev", "tools", "2.0.0")
			putFile(t, filepath.Join(tools, "skills", "jot", "SKILL.md"), skill)
			path := filepath.Join(home, ".claude", "plugins", "installed_plugins.json")
			putFile(t, path, strings.Replace(read(t, path), `"plugins": {`, `"plugins": {"tools@metro-dev": [{"scope": "user", "installPath": "`+tools+`"}],`, 1))
		}},

		// Where line-lx can't look, it sends nothing for a bare name.
		{"settings line-lx can't read", func(t *testing.T, _, project, _ string) {
			putFile(t, filepath.Join(project, ".claude", "settings.json"), `{"permissions": `)
		}},
		{"a project folder line-lx can't read", func(t *testing.T, _, project, _ string) {
			if os.Getuid() == 0 {
				t.Skip("root reads every folder")
			}
			locked := filepath.Join(project, "locked")
			if err := os.MkdirAll(locked, 0o755); err != nil {
				t.Fatal(err)
			}
			os.Chmod(locked, 0)
			t.Cleanup(func() { os.Chmod(locked, 0o755) })
		}},
		{"a project too large to look through", func(t *testing.T, _, project, _ string) {
			limit := nestedLimit
			nestedLimit = 3
			t.Cleanup(func() { nestedLimit = limit })
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hub := newUsageHub(t)
			project := usageMachine(t, hub.URL)
			managed := claudeManagedDir
			claudeManagedDir = t.TempDir()
			t.Cleanup(func() { claudeManagedDir = managed })
			tc.add(t, os.Getenv("HOME"), project, claudeManagedDir)

			for _, name := range []string{"jot", "notes:jot"} {
				before := len(hub.took())
				_ = runReportUsage("claude-code", "dev", strings.NewReader(hookJSON(hookSkill, name, filepath.Join(project, "src"))))
				want := 0
				if name == "notes:jot" {
					want = 1
				}
				if got := len(hub.took()) - before; got != want {
					t.Errorf("%s: %d reports, want %d", name, got, want)
				}
			}
		})
	}
}

// A session at / or in the home folder has too much below it to look through: a bare name isn't counted,
// at once, and its plugin's name still is.
func TestReportUsageBareNameAtTheTop(t *testing.T) {
	for _, top := range []string{"the root", "the home folder"} {
		t.Run(top, func(t *testing.T) {
			hub := newUsageHub(t)
			usageMachine(t, hub.URL)
			dir := string(filepath.Separator)
			if top == "the home folder" {
				dir = os.Getenv("HOME")
			}
			t.Setenv("CLAUDE_PROJECT_DIR", dir)
			start := time.Now()
			_ = runReportUsage("claude-code", "dev", strings.NewReader(hookJSON(hookSkill, "jot", dir)))
			if took := time.Since(start); took > 200*time.Millisecond {
				t.Errorf("a bare name took %s", took)
			}
			if got := hub.took(); len(got) != 0 {
				t.Errorf("sent %v", got)
			}
			_ = runReportUsage("claude-code", "dev", strings.NewReader(hookJSON(hookSkill, "notes:jot", dir)))
			if got := hub.took(); len(got) != 1 {
				t.Errorf("sent %v, want the plugin's name", got)
			}
		})
	}
}

// A stdin that never ends doesn't hold report-usage past its deadline.
func TestReportUsageStdinThatNeverEnds(t *testing.T) {
	hub := newUsageHub(t)
	project := usageMachine(t, hub.URL)
	wait := usageWait
	usageWait = 200 * time.Millisecond
	defer func() { usageWait = wait }()
	r, w := io.Pipe()
	defer w.Close()
	go w.Write([]byte(hookJSON(hookSkill, "notes:jot", project)[:40]))

	start := time.Now()
	if err := runReportUsage("claude-code", "dev", r); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > time.Second {
		t.Errorf("report-usage waited %s for stdin", took)
	}
	if got, d := hub.took(), readDropped(); len(got) != 0 || d.Count != 0 {
		t.Errorf("sent %v, dropped %+v", got, d)
	}
}

func TestReportUsageNeedsTheHubsMarketplace(t *testing.T) {
	installed := func() string { return filepath.Join(os.Getenv("HOME"), ".claude", "plugins", "installed_plugins.json") }
	cache := func() string {
		return filepath.Join(os.Getenv("HOME"), ".claude", "plugins", "cache", "metro-dev", "notes")
	}
	// installNotes adds a project-scope copy of notes in the cache folder named folder, whose manifest gives
	// version ("": no manifest).
	installNotes := func(t *testing.T, folder, version string) {
		dir := filepath.Join(cache(), folder)
		putFile(t, filepath.Join(dir, "skills", "jot", "SKILL.md"), "---\nname: jot\n---\n")
		if version != "" {
			putFile(t, filepath.Join(dir, ".claude-plugin", "plugin.json"), `{"name": "notes", "version": "`+version+`"}`)
		}
		entry := `"version": "1.2.0"}, {"scope": "project", "projectPath": "/work/site", "installPath": "` + dir + `", "version": "` + folder + `"}]`
		putFile(t, installed(), strings.Replace(read(t, installed()), `"version": "1.2.0"}]`, entry, 1))
	}
	// hashNotes moves notes to a cache folder named by a hash, as Claude Code does for some installs.
	hashNotes := func(t *testing.T, manifest string) {
		hashed := filepath.Join(cache(), "6b7aaf20fa8f")
		if err := os.Rename(filepath.Join(cache(), "1.2.0"), hashed); err != nil {
			t.Fatal(err)
		}
		putFile(t, filepath.Join(hashed, ".claude-plugin", "plugin.json"), manifest)
		putFile(t, installed(), strings.Replace(read(t, installed()), filepath.Join("notes", "1.2.0"), filepath.Join("notes", "6b7aaf20fa8f"), 1))
	}
	for _, tc := range []struct {
		name, agent, slug, skill string
		edit                     func(t *testing.T)
		sent                     int
	}{
		{name: "the workspace", agent: "claude-code", slug: "dev", skill: "notes:jot", sent: 1},
		{name: "another workspace", agent: "claude-code", slug: "acme", skill: "notes:jot"},
		{name: "another agent", agent: "codex", slug: "dev", skill: "notes:jot"},
		{name: "an invalid slug", agent: "claude-code", slug: "../dev", skill: "notes:jot"},
		{name: "metro-dev from another hub", agent: "claude-code", slug: "dev", skill: "notes:jot", edit: func(t *testing.T) {
			t.Setenv("LINE_LX_HUB", "https://other.example.com")
		}},
		{name: "a plugin outside its folder in Claude Code's cache", agent: "claude-code", slug: "dev", skill: "notes:lint", edit: func(t *testing.T) {
			cache := filepath.Join(os.Getenv("HOME"), ".claude", "plugins", "cache")
			notes, other := filepath.Join(cache, "metro-dev", "notes", "1.2.0"), filepath.Join(cache, "elsewhere", "other", "0.1.0")
			putFile(t, installed(), strings.Replace(read(t, installed()), `"`+notes+`"`, `"`+other+`"`, 1))
		}},
		{name: "a plugin of the same name from another marketplace", agent: "claude-code", slug: "dev", skill: "notes:jot", edit: func(t *testing.T) {
			putFile(t, installed(), strings.Replace(read(t, installed()), `"other@elsewhere"`, `"notes@elsewhere"`, 1))
		}},
		{name: "no record of the plugins", agent: "claude-code", slug: "dev", skill: "notes:jot", edit: func(t *testing.T) {
			os.Remove(installed())
		}},
		{name: "the same version in two scopes", agent: "claude-code", slug: "dev", skill: "notes:jot", sent: 1, edit: func(t *testing.T) {
			installNotes(t, "1.2.0", "1.2.0")
		}},
		{name: "the same version in a folder named by a hash", agent: "claude-code", slug: "dev", skill: "notes:jot", sent: 1, edit: func(t *testing.T) {
			installNotes(t, "3f1c0e9a77d2", "1.2.0")
		}},
		{name: "two versions, one per scope", agent: "claude-code", slug: "dev", skill: "notes:jot", edit: func(t *testing.T) {
			installNotes(t, "1.1.0", "1.1.0")
		}},
		{name: "two versions, one in a folder named by a hash", agent: "claude-code", slug: "dev", skill: "notes:jot", edit: func(t *testing.T) {
			installNotes(t, "3f1c0e9a77d2", "1.1.0")
		}},
		{name: "a folder named by a hash: the manifest's version", agent: "claude-code", slug: "dev", skill: "notes:jot", sent: 1, edit: func(t *testing.T) {
			hashNotes(t, `{"name": "notes", "version": "1.2.0"}`)
		}},
		{name: "a folder named by a hash, without a version in the manifest", agent: "claude-code", slug: "dev", skill: "notes:jot", edit: func(t *testing.T) {
			hashNotes(t, `{"name": "notes"}`)
		}},
		{name: "a version folder without a manifest", agent: "claude-code", slug: "dev", skill: "notes:jot", sent: 1, edit: func(t *testing.T) {
			os.Remove(filepath.Join(cache(), "1.2.0", ".claude-plugin", "plugin.json"))
		}},
		{name: "a manifest line-lx can't read", agent: "claude-code", slug: "dev", skill: "notes:jot", edit: func(t *testing.T) {
			putFile(t, filepath.Join(cache(), "1.2.0", ".claude-plugin", "plugin.json"), `{"version": `)
		}},
		{name: "a manifest version that isn't one", agent: "claude-code", slug: "dev", skill: "notes:jot", edit: func(t *testing.T) {
			putFile(t, filepath.Join(cache(), "1.2.0", ".claude-plugin", "plugin.json"), `{"version": "1.2.0; drop"}`)
		}},
		{name: "a folder that isn't a version, without a manifest", agent: "claude-code", slug: "dev", skill: "notes:jot", edit: func(t *testing.T) {
			path := filepath.Join(cache(), ".v")
			putFile(t, filepath.Join(path, "skills", "jot", "SKILL.md"), "---\nname: jot\n---\n")
			putFile(t, installed(), strings.Replace(read(t, installed()), filepath.Join("notes", "1.2.0"), filepath.Join("notes", ".v"), 1))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hub := newUsageHub(t)
			project := usageMachine(t, hub.URL)
			if tc.edit != nil {
				tc.edit(t)
			}
			_ = runReportUsage(tc.agent, tc.slug, strings.NewReader(hookJSON(hookSkill, tc.skill, project)))
			got := hub.took()
			if len(got) != tc.sent {
				t.Errorf("sent %v, want %d reports", got, tc.sent)
			}
			for _, r := range got {
				if !strings.Contains(r, `"version":"1.2.0"`) {
					t.Errorf("sent %s, want version 1.2.0", r)
				}
			}
		})
	}
}

// captureStderr returns what fn prints on stderr.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stderr := os.Stderr
	os.Stderr = w
	out := make(chan string)
	go func() {
		data, _ := io.ReadAll(r)
		out <- string(data)
	}()
	defer func() { os.Stderr = stderr }()
	fn()
	w.Close()
	return <-out
}

func TestReportUsageCountsWhatItDrops(t *testing.T) {
	hub := newUsageHub(t)
	project := usageMachine(t, hub.URL)
	t.Setenv("LINE_LX_TOKEN", "tok-secret-1234")
	send := func() string {
		var stdout string
		stderr := captureStderr(t, func() {
			stdout = captureStdout(t, func() {
				if err := runReportUsage("claude-code", "dev", strings.NewReader(hookJSON(hookSkill, "notes:jot", project))); err != nil {
					t.Error(err)
				}
			})
		})
		if stdout != "" {
			t.Errorf("stdout = %q: an async hook's stdout goes to Claude", stdout)
		}
		if strings.Contains(stderr, "tok-secret") {
			t.Errorf("the token is on stderr: %q", stderr)
		}
		return stderr
	}

	// The hub refuses the token.
	if out := send(); !strings.Contains(out, "usage report dropped") {
		t.Errorf("stderr = %q", out)
	}
	first := readDropped()
	if first.Count != 1 || time.Since(first.Since) > time.Minute {
		t.Fatalf("dropped = %+v", first)
	}

	// The hub is down.
	t.Setenv("LINE_LX_TOKEN", "tok")
	hub.mu.Lock()
	hub.status = http.StatusServiceUnavailable
	hub.mu.Unlock()
	send()
	if d := readDropped(); d.Count != 2 || !d.Since.Equal(first.Since) {
		t.Errorf("dropped = %+v, want 2 since %v", d, first.Since)
	}
	if msg := droppedText(readDropped()); !strings.HasPrefix(msg, "2 usage reports dropped since ") {
		t.Errorf("doctor says %q", msg)
	}

	// The hub is too slow.
	wait := usageWait
	defer func() { usageWait = wait }()
	usageWait = 100 * time.Millisecond
	hub.mu.Lock()
	hub.status, hub.delay = http.StatusAccepted, 500*time.Millisecond
	hub.mu.Unlock()
	start := time.Now()
	send()
	if took := time.Since(start); took > 400*time.Millisecond {
		t.Errorf("report-usage waited %s for a slow hub", took)
	}
	if d := readDropped(); d.Count != 3 {
		t.Errorf("dropped = %+v, want 3", d)
	}

	// A report that reaches the hub clears the count.
	usageWait = wait
	send()
	if d := readDropped(); d.Count != 0 || droppedText(d) != "" {
		t.Errorf("dropped = %+v after a report went through", d)
	}
}

// A hub that takes the request and never answers: report-usage still ends within 3 seconds, the count of
// the dropped report included.
func TestReportUsageEndsWithin3Seconds(t *testing.T) {
	release := make(chan struct{})
	hub := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	t.Cleanup(hub.Close)
	t.Cleanup(func() { close(release) }) // runs first: Close waits for the handler
	project := usageMachine(t, hub.URL)

	start := time.Now()
	captureStderr(t, func() {
		_ = runReportUsage("claude-code", "dev", strings.NewReader(hookJSON(hookSkill, "notes:jot", project)))
	})
	if took := time.Since(start); took >= 3*time.Second {
		t.Errorf("report-usage took %s", took)
	}
	if d := readDropped(); d.Count != 1 {
		t.Errorf("dropped = %+v, want 1", d)
	}
}

// The count is a plain file only the person can read: a symlink there is refused, and a file others could
// read becomes private.
func TestDroppedCountFile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", "")
	path, err := droppedPath()
	if err != nil {
		t.Fatal(err)
	}
	putFile(t, path, `{"count":1,"since":"2026-11-19T10:00:00Z"}`)
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := countDropped(time.Now()); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, %v; want 0600", info.Mode().Perm(), err)
	}
	if d := readDropped(); d.Count != 2 {
		t.Errorf("dropped = %+v, want 2", d)
	}

	target := filepath.Join(t.TempDir(), "elsewhere.json")
	putFile(t, target, "{}")
	os.Remove(path)
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if err := countDropped(time.Now()); err == nil {
		t.Error("countDropped wrote through a symlink")
	}
	if got := read(t, target); got != "{}" {
		t.Errorf("the symlink's target is now %q", got)
	}
}

func TestReportUsageWithoutHub(t *testing.T) {
	hub := newUsageHub(t)
	project := usageMachine(t, hub.URL)
	hub.Close()
	captureStderr(t, func() {
		_ = runReportUsage("claude-code", "dev", strings.NewReader(hookJSON(hookTyped, "notes:jot", project)))
	})
	if d := readDropped(); d.Count != 1 {
		t.Errorf("dropped = %+v, want 1", d)
	}
}

func TestReportUsageBodyIsTheContract(t *testing.T) {
	got := report("notes", "1.2.0", "jot", "activated", "person", true)
	want := `{"workspace":"dev","agent":"claude-code","kind":"activated","plugin":"notes","version":"1.2.0","name":"jot","trigger":"person","success":true}`
	if got != want {
		t.Errorf("body = %s, want %s", got, want)
	}
}
