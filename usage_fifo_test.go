//go:build darwin || linux

package main

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// mkfifo puts a FIFO at path. Opening it to read waits for a writer: the cleanup opens it to write, so a
// reader still waiting goes on.
func mkfifo(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	os.Remove(path)
	if err := syscall.Mkfifo(path, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if f, err := os.OpenFile(path, os.O_RDWR, 0); err == nil {
			f.Close()
		}
	})
}

// runQuick runs report-usage and fails the test when it takes over a second.
func runQuick(t *testing.T, input string) {
	t.Helper()
	start := time.Now()
	captureStderr(t, func() { _ = runReportUsage("claude-code", "dev", strings.NewReader(input)) })
	if took := time.Since(start); took > time.Second {
		t.Errorf("report-usage took %s", took)
	}
}

// A FIFO where a plugin's manifest or a SKILL.md should be is passed over, at once.
func TestReportUsagePassesOverAFIFO(t *testing.T) {
	t.Run("as a plugin's manifest", func(t *testing.T) {
		hub := newUsageHub(t)
		project := usageMachine(t, hub.URL)
		mkfifo(t, filepath.Join(os.Getenv("HOME"), ".claude", "plugins", "cache", "metro-dev", "notes", "1.2.0", ".claude-plugin", "plugin.json"))
		runQuick(t, hookJSON(hookSkill, "notes:jot", project))
		if got := hub.took(); len(got) != 0 {
			t.Errorf("sent %v: the version can't be read", got)
		}
	})
	t.Run("as a personal SKILL.md", func(t *testing.T) {
		hub := newUsageHub(t)
		project := usageMachine(t, hub.URL)
		mkfifo(t, filepath.Join(os.Getenv("HOME"), ".claude", "skills", "pipe", "SKILL.md"))
		runQuick(t, hookJSON(hookSkill, "jot", filepath.Join(project, "src")))
		if got := hub.took(); len(got) != 1 {
			t.Errorf("sent %v, want the workspace's skill", got)
		}
	})
}

// A file that hangs while line-lx looks for the plugin doesn't hold it past its deadline, and isn't a
// dropped report.
func TestReportUsageDeadlineCoversTheLook(t *testing.T) {
	hub := newUsageHub(t)
	project := usageMachine(t, hub.URL)
	wait := usageWait
	usageWait = 300 * time.Millisecond
	defer func() { usageWait = wait }()
	mkfifo(t, filepath.Join(project, ".claude", "settings.json")) // read for permissions.additionalDirectories

	runQuick(t, hookJSON(hookSkill, "jot", filepath.Join(project, "src")))
	if got, d := hub.took(), readDropped(); len(got) != 0 || d.Count != 0 {
		t.Errorf("sent %v, dropped %+v", got, d)
	}
}
