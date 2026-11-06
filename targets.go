package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// A file target is an agent that reads skills from a folder. line-lx writes each plugin there as
// folders it owns, recorded in a stamp, and never touches a folder it has no stamp for.
type fileTarget struct {
	name    string
	home    string // the agent's folder: the target is detected when it exists
	dir     string // where skills go
	flat    bool   // one top-level folder per skill (<plugin>--<skill>), stamps in <dir>/.line-lx/
	restart string // a reminder for doctor, when the agent only reads skills on launch
}

func fileTargets() []fileTarget {
	home, _ := os.UserHomeDir()
	return []fileTarget{
		// Codex: the docs (https://developers.openai.com/codex/skills) don't say whether a skills
		// folder is scanned recursively, and third-party guides say skills must sit one level deep.
		// So each skill gets its own top-level folder, which works either way. Codex's documented
		// user folder is now ~/.agents/skills; ~/.codex/skills is its older location, kept here
		// because pi also reads ~/.agents/skills and would load every skill twice.
		{name: "codex", home: filepath.Join(home, ".codex"), dir: filepath.Join(home, ".codex", "skills"), flat: true},
		// pi scans <agentDir>/skills recursively for folders holding a SKILL.md
		// (pi-mono packages/coding-agent/docs/skills.md and src/core/skills.ts), so a plugin is one folder.
		{name: "pi", home: filepath.Join(home, ".pi", "agent"), dir: filepath.Join(home, ".pi", "agent", "skills"),
			restart: "pi reads skills on startup: restart it to pick up changes"},
	}
}

func (t fileTarget) detected() bool {
	info, err := os.Stat(t.home)
	return err == nil && info.IsDir()
}

// staging is where writes are prepared: in the agent's folder (the same filesystem, so a rename moves
// them in) but outside its skills folder, so the agent never scans a half-written skill.
func (t fileTarget) staging() string { return filepath.Join(t.home, ".line-lx-staging") }

// sweepStaging removes the staging folder: leftovers of an interrupted sync, or this sync's when done.
func (t fileTarget) sweepStaging() error {
	if err := realDir(t.staging()); err != nil {
		return err
	}
	return os.RemoveAll(t.staging())
}

// realDir refuses a path line-lx is about to write into or delete that exists but isn't a plain folder:
// a symlink planted there must never be followed.
func realDir(path string) error {
	info, err := os.Lstat(path)
	if err == nil && !info.IsDir() {
		return fmt.Errorf("%s isn't a plain folder (a symlink or a file), so line-lx won't use it", path)
	}
	return nil
}

// stamp records what line-lx wrote for one plugin.
type stamp struct {
	Hub       string   `json:"hub"`
	Plugin    string   `json:"plugin"`
	Version   string   `json:"version"`
	VersionID string   `json:"versionId"`
	SyncedAt  string   `json:"syncedAt"`
	Skills    []string `json:"skills,omitempty"` // flat layout only: the folders line-lx owns
}

func (t fileTarget) stampPath(plugin string) string {
	if t.flat {
		return filepath.Join(t.dir, ".line-lx", plugin+".json")
	}
	return filepath.Join(t.dir, plugin, ".line-lx.json")
}

// stamps returns line-lx's plugins in this target, keyed by the name their location gives them.
// Names that aren't valid hub names are ignored, so a stamp can never point line-lx outside its folders.
func (t fileTarget) stamps() map[string]stamp {
	pattern := filepath.Join(t.dir, "*", ".line-lx.json")
	if t.flat {
		pattern = filepath.Join(t.dir, ".line-lx", "*.json")
	}
	paths, _ := filepath.Glob(pattern)
	out := map[string]stamp{}
	for _, p := range paths {
		name := filepath.Base(filepath.Dir(p))
		if t.flat {
			name = strings.TrimSuffix(filepath.Base(p), ".json")
		}
		var s stamp
		data, err := os.ReadFile(p)
		if !validName(name) || err != nil || json.Unmarshal(data, &s) != nil {
			continue
		}
		var skills []string
		for _, sk := range s.Skills {
			if validName(sk) {
				skills = append(skills, sk)
			}
		}
		s.Skills = skills
		out[name] = s
	}
	return out
}

// validName is the hub's rule for plugin and skill names, so a name is always
// one plain folder name and never a flag.
var nameRule = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

func validName(n string) bool { return len(n) <= 64 && nameRule.MatchString(n) }

// safePath accepts a skill's extra file only if it stays inside the skill's folder.
func safePath(p string) bool { return !strings.EqualFold(p, "SKILL.md") && localPath(p) }

// localPath accepts a relative slash path that stays inside its folder: no empty, `.` or `..` segment,
// no backslash, colon or NUL (Windows separators, drive letters, alternate streams).
func localPath(p string) bool {
	if p == "" || strings.ContainsAny(p, "\\:\x00") {
		return false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	return filepath.IsLocal(filepath.FromSlash(p))
}

// write puts a plugin's skills in the target. Everything is first written to a staging folder, then
// renamed into place, so a failed write never leaves half a skill.
// It returns the files it skipped, for the hub's `unsupported`.
func (t fileTarget) write(hub string, r release) (unsupported []string, err error) {
	if !validName(r.name) {
		return nil, fmt.Errorf("invalid plugin name %q", clean(r.name))
	}
	old, hadStamp := t.stamps()[r.name]
	if hadStamp && old.Hub != hub {
		return nil, fmt.Errorf("%s belongs to %s", r.name, old.Hub)
	}
	if err := realDir(t.staging()); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(t.staging(), 0o755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(t.dir, 0o755); err != nil {
		return nil, err
	}
	tmp, err := os.MkdirTemp(t.staging(), "")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)

	var skills []string
	for _, s := range r.version.Snapshot.Skills {
		if !validName(s.Name) {
			unsupported = append(unsupported, fmt.Sprintf("skill %q (invalid name)", clean(s.Name)))
			continue
		}
		dir := filepath.Join(tmp, s.Name)
		if err := writeFile(filepath.Join(dir, "SKILL.md"), s.Content); err != nil {
			return nil, err
		}
		for _, f := range s.Files {
			if !safePath(f.Path) {
				unsupported = append(unsupported, fmt.Sprintf("skill %s: file %q (unsafe path)", s.Name, clean(f.Path)))
				continue
			}
			if err := writeFile(filepath.Join(dir, filepath.FromSlash(f.Path)), f.Content); err != nil {
				return nil, err
			}
		}
		skills = append(skills, s.Name)
	}

	st := stamp{Hub: hub, Plugin: r.name, Version: r.version.Version, VersionID: r.version.ID, SyncedAt: time.Now().UTC().Format(time.RFC3339)}

	if !t.flat {
		dest := filepath.Join(t.dir, r.name)
		if exists(dest) && !hadStamp {
			return nil, fmt.Errorf("%s already exists and isn't line-lx's: move it away first", dest)
		}
		if err := writeJSON(filepath.Join(tmp, ".line-lx.json"), st); err != nil {
			return nil, err
		}
		return unsupported, replaceDir(tmp, dest)
	}

	// Flat: one folder per skill, so check none of them belongs to someone else before touching anything.
	owned := map[string]bool{}
	for _, s := range old.Skills {
		owned[s] = true
	}
	if err := realDir(filepath.Dir(t.stampPath(r.name))); err != nil {
		return nil, err
	}
	union := slices.Clone(old.Skills)
	for _, s := range skills {
		if dest := filepath.Join(t.dir, r.name+"--"+s); exists(dest) && !owned[s] {
			return nil, fmt.Errorf("%s already exists and isn't line-lx's: move it away first", dest)
		}
		if !owned[s] {
			union = append(union, s)
		}
	}
	// Record every folder this write may leave behind before making any, so an interrupted sync never
	// leaves a folder the next one refuses to touch. Its old version id makes the next sync write again.
	pending := old
	pending.Hub, pending.Plugin, pending.Skills = hub, r.name, union
	if err := writeJSON(t.stampPath(r.name), pending); err != nil {
		return nil, err
	}
	kept := map[string]bool{}
	for _, s := range skills {
		if err := replaceDir(filepath.Join(tmp, s), filepath.Join(t.dir, r.name+"--"+s)); err != nil {
			return nil, err
		}
		kept[s] = true
	}
	for _, s := range old.Skills {
		if !kept[s] {
			if err := os.RemoveAll(filepath.Join(t.dir, r.name+"--"+s)); err != nil {
				return nil, err
			}
		}
	}
	st.Skills = skills
	return unsupported, writeJSON(t.stampPath(r.name), st)
}

// remove deletes a plugin line-lx wrote (the caller found its stamp, so the names are valid).
func (t fileTarget) remove(plugin string, st stamp) error {
	if !validName(plugin) {
		return fmt.Errorf("invalid plugin name %q", clean(plugin))
	}
	if !t.flat {
		return os.RemoveAll(filepath.Join(t.dir, plugin))
	}
	if err := realDir(filepath.Dir(t.stampPath(plugin))); err != nil {
		return err
	}
	for _, s := range st.Skills {
		if err := os.RemoveAll(filepath.Join(t.dir, plugin+"--"+s)); err != nil {
			return err
		}
	}
	return os.Remove(t.stampPath(plugin))
}

// replaceDir moves src (in staging) to dest. What was at dest goes back to staging, never next to the
// skills, and is put back if the move fails.
func replaceDir(src, dest string) error {
	if !exists(dest) {
		return os.Rename(src, dest)
	}
	old := src + ".old"
	if err := os.Rename(dest, old); err != nil {
		return err
	}
	if err := os.Rename(src, dest); err != nil {
		_ = os.Rename(old, dest)
		return err
	}
	return os.RemoveAll(old)
}

func writeFile(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0o644)
}

// writeJSON writes through a temp file and a rename, so a stamp is never half written.
func writeJSON(path string, v any) error {
	data, _ := json.MarshalIndent(v, "", "  ")
	if err := writeFile(path+".tmp", string(data)+"\n"); err != nil {
		return err
	}
	return os.Rename(path+".tmp", path)
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}
