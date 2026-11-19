package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// --- A bare skill name: which skill it runs ---
//
// Claude Code runs a plugin's skill under its bare name, `/skill`, only when no other skill or command has
// that name (https://code.claude.com/docs/en/skills). So a bare name counts as the workspace's only when
// exactly one skill or command that Claude Code can find on this machine has it, and that one is a skill of
// a workspace plugin. line-lx looks where Claude Code documents skills and commands:
//
//   - the managed policy folder's .claude/, and Claude Code's own folder (personal skills and commands,
//     and the skills-directory plugins in its skills/);
//   - the .claude/ of the hook's folder and of the session's project folder, and of every folder above them;
//   - every .claude/ below the session's project folder (nested skills, which load once Claude works there);
//   - the .claude/ of each permissions.additionalDirectories of the settings files line-lx can read;
//   - every plugin Claude Code installed, from any marketplace.
//
// A location line-lx can't read, a session at / or in the home folder, or a project too large to look
// through in time, means no report: line-lx fails closed. Folders added for one session with --add-dir or
// /add-dir aren't recorded anywhere; a skill there can take the bare name, and the report then names the
// workspace's skill, which the hub knows.

// nestedLimit and nestedWait bound the look for nested skills below the project folder.
var (
	nestedLimit = 20000
	nestedWait  = time.Second
)

// bareSkill resolves a bare skill name to the workspace's plugin, its version and the skill's folder.
func (w *installedWorkspace) bareSkill(name, cwd string) (plugin, version, skill string, ok bool) {
	if name == "" || strings.ContainsAny(name, `/\:`) || name == "." || name == ".." || !filepath.IsAbs(cwd) {
		return "", "", "", false
	}
	// Every installed plugin that has it: exactly one, a workspace plugin.
	having := map[string]bool{} // by name@marketplace
	var dir string
	for _, c := range w.copies {
		has, err := pluginHas(c.dir, c.name, name)
		if err != nil {
			return "", "", "", false
		}
		if has {
			having[c.name+"@"+c.market] = true
			plugin, dir = c.name, c.dir
		}
	}
	if len(having) != 1 {
		return "", "", "", false
	}
	ours, version, ok := w.plugin(plugin)
	if !ok || !slices.Contains(ours, dir) {
		return "", "", "", false
	}
	if skill, ok = skillIn(dir, plugin, name); !ok {
		return "", "", "", false
	}
	if taken, err := takenElsewhere(name, cwd); taken || err != nil {
		return "", "", "", false
	}
	return plugin, version, skill, true
}

// takenElsewhere is true when a skill or command outside the installed plugins has the name.
func takenElsewhere(name, cwd string) (bool, error) {
	project := os.Getenv("CLAUDE_PROJECT_DIR") // where the session started
	if !filepath.IsAbs(project) {
		project = cwd
	}
	// A session started at / or in the home folder has the whole disk or home below it: too much to look
	// through, so a bare name there isn't counted.
	project = filepath.Clean(project)
	if home, err := os.UserHomeDir(); filepath.Dir(project) == project || err != nil || project == filepath.Clean(home) {
		return false, errTooLarge
	}
	var bases []string // folders laid out as a .claude/ folder: skills/ and commands/
	if claudeManagedDir != "" {
		bases = append(bases, filepath.Join(claudeManagedDir, ".claude"))
	}
	config, err := claudeConfigDir()
	if err != nil {
		return false, err
	}
	bases = append(bases, config)
	for _, from := range []string{cwd, project} {
		for dir := filepath.Clean(from); ; dir = filepath.Dir(dir) {
			bases = append(bases, filepath.Join(dir, ".claude"))
			if filepath.Dir(dir) == dir {
				break
			}
		}
	}
	added, err := additionalDirectories(project)
	if err != nil {
		return false, err
	}
	for _, dir := range added {
		bases = append(bases, filepath.Join(dir, ".claude"))
	}
	for _, base := range bases {
		if taken, err := claudeFolderHas(base, name); taken || err != nil {
			return taken, err
		}
	}
	return nestedHas(project, name)
}

// present is true when path exists. Only a missing path is false without an error: one line-lx can't
// check is an error, so the caller fails closed.
func present(path string) (bool, error) {
	_, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

// claudeFolderHas is true when a folder laid out as .claude/ has a skill or a command of that name: a skill
// folder or a SKILL.md's name in skills/, a skill of a skills-directory plugin there, or commands/<name>.md.
func claudeFolderHas(base, name string) (bool, error) {
	if has, err := skillsHave(filepath.Join(base, "skills"), name, true); has || err != nil {
		return has, err
	}
	return present(filepath.Join(base, "commands", name+".md"))
}

// skillsHave is true when a skills/ folder has a skill of that name, by its folder or its SKILL.md's name
// (bare, or after a plugin's prefix). With plugins, it also looks into the skills-directory plugins there.
func skillsHave(dir, name string, plugins bool) (bool, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	for _, e := range entries {
		sub := filepath.Join(dir, e.Name())
		info, err := os.Stat(sub) // a skill folder can be a symlink
		switch {
		case errors.Is(err, fs.ErrNotExist) || err == nil && !info.IsDir():
			continue
		case err != nil:
			return false, err
		}
		md, err := present(filepath.Join(sub, "SKILL.md"))
		if err != nil {
			return false, err
		}
		if md {
			if n := frontmatterName(filepath.Join(sub, "SKILL.md")); e.Name() == name || n == name || strings.HasSuffix(n, ":"+name) {
				return true, nil
			}
		}
		if !plugins {
			continue
		}
		plugin, err := present(filepath.Join(sub, ".claude-plugin", "plugin.json"))
		if err != nil {
			return false, err
		}
		if plugin {
			if has, err := pluginHas(sub, e.Name(), name); has || err != nil {
				return has, err
			}
		}
	}
	return false, nil
}

// pluginHas is true when the plugin in dir has a skill or a command of that bare name: in skills/, as its
// root SKILL.md, or in commands/.
func pluginHas(dir, plugin, name string) (bool, error) {
	if has, err := skillsHave(filepath.Join(dir, "skills"), name, false); has || err != nil {
		return has, err
	}
	if n := frontmatterName(filepath.Join(dir, "SKILL.md")); n == name || n == plugin+":"+name {
		return true, nil
	}
	return present(filepath.Join(dir, "commands", name+".md"))
}

// additionalDirectories lists permissions.additionalDirectories from the settings files: managed, the
// person's, and the project's (shared and local). A path starting with ~/ is under the home folder, and a
// relative one under the project folder.
func additionalDirectories(project string) ([]string, error) {
	files := claudeSettingsFiles()
	files = append(files, filepath.Join(project, ".claude", "settings.json"), filepath.Join(project, ".claude", "settings.local.json"))
	home, _ := os.UserHomeDir()
	var dirs []string
	for _, path := range files {
		s, real, _, err := readSettingsFile(path)
		if err != nil {
			return nil, err
		}
		raw, ok := s["permissions"]
		if !ok {
			continue
		}
		var p struct {
			AdditionalDirectories []string `json:"additionalDirectories"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, fmt.Errorf("permissions in %s isn't readable: %v", real, err)
		}
		for _, d := range p.AdditionalDirectories {
			switch {
			case d == "~" || strings.HasPrefix(d, "~/"):
				if home == "" {
					return nil, errors.New("no home folder")
				}
				d = filepath.Join(home, strings.TrimPrefix(d, "~"))
			case !filepath.IsAbs(d):
				d = filepath.Join(project, d)
			}
			dirs = append(dirs, filepath.Clean(d))
		}
	}
	return dirs, nil
}

// errTooLarge: the project has too many folders to look through for nested skills in time.
var errTooLarge = errors.New("too many folders to look through")

// nestedHas is true when a .claude/ folder below the project folder has a skill or a command of that name.
func nestedHas(project, name string) (bool, error) {
	deadline, seen, found := time.Now().Add(nestedWait), 0, false
	err := filepath.WalkDir(project, func(path string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case !d.IsDir():
			return nil
		case d.Name() == ".git":
			return filepath.SkipDir
		}
		if seen++; seen > nestedLimit || time.Now().After(deadline) {
			return errTooLarge
		}
		if d.Name() != ".claude" {
			return nil
		}
		has, err := claudeFolderHas(path, name)
		switch {
		case err != nil:
			return err
		case has:
			found = true
			return filepath.SkipAll
		}
		return filepath.SkipDir
	})
	return found, err
}
