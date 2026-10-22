package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Choice 2 only prints the command; it must not write any autostart file.
func TestRunInstallManualAutostartOnlyPrints(t *testing.T) {
	home := t.TempDir()
	config := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", config)

	var out strings.Builder
	if code := RunInstall(strings.NewReader("2\n"), &out, false); code != 0 {
		t.Fatalf("RunInstall = %d, want 0\n%s", code, out.String())
	}

	target := filepath.Join(home, ".local", "bin", "ts6tray")
	if !strings.Contains(out.String(), target+" --daemon") {
		t.Errorf("output does not print %q --daemon:\n%s", target, out.String())
	}
	for _, p := range []string{
		filepath.Join(config, "autostart", "ts6tray.desktop"),
		filepath.Join(config, "hypr", "hyprland.conf"),
	} {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("%s was created", p)
		}
	}
}

// neverReader fails the test if anything reads from it.
type neverReader struct{ t *testing.T }

func (n neverReader) Read([]byte) (int, error) {
	n.t.Helper()
	n.t.Error("--yes read from stdin")
	return 0, io.EOF
}

func installEnvDirs(t *testing.T) (home, config, cache string) {
	t.Helper()
	home, config, cache = t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", config)
	t.Setenv("XDG_CACHE_HOME", cache)
	return
}

func TestInstallYesAnswersEverything(t *testing.T) {
	home, config, cache := installEnvDirs(t)

	var out strings.Builder
	if code := RunInstall(neverReader{t}, &out, true); code != 0 {
		t.Fatalf("code %d\n%s", code, out.String())
	}
	for _, p := range []string{
		filepath.Join(home, ".local", "bin", "ts6tray"),
		filepath.Join(config, "autostart", "ts6tray.desktop"),
		filepath.Join(cache, "ts6tray", "ts6tray.svg"),
	} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s missing: %v", p, err)
		}
	}
	if !strings.Contains(out.String(), "Choice [1-3]: 1 (--yes)\n") {
		t.Errorf("output lacks the logged answer:\n%s", out.String())
	}

	// a second run (an update) over the existing install succeeds as well
	out.Reset()
	if code := RunInstall(neverReader{t}, &out, true); code != 0 {
		t.Fatalf("re-run code %d\n%s", code, out.String())
	}
}

// --yes over an existing install (what every ritz-get update is) keeps the
// autorun state: no desktop file stays none, an existing one is refreshed.
func TestInstallYesKeepsAutorunState(t *testing.T) {
	_, config, _ := installEnvDirs(t)
	desktop := filepath.Join(config, "autostart", "ts6tray.desktop")
	var out strings.Builder
	if code := RunInstall(neverReader{t}, &out, true); code != 0 {
		t.Fatalf("install %d\n%s", code, out.String())
	}
	if err := os.Remove(desktop); err != nil {
		t.Fatal(err)
	}

	out.Reset()
	if code := RunInstall(neverReader{t}, &out, true); code != 0 {
		t.Fatalf("update %d\n%s", code, out.String())
	}
	if _, err := os.Stat(desktop); err == nil {
		t.Error("update re-created the autostart entry")
	}
	if !strings.Contains(out.String(), "autorun left off as before") {
		t.Errorf("no note about autorun:\n%s", out.String())
	}

	// with an entry present, the update refreshes it
	if err := os.MkdirAll(filepath.Dir(desktop), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(desktop, []byte("[Desktop Entry]\nName=ts6tray\nExec=/old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if code := RunInstall(neverReader{t}, &out, true); code != 0 {
		t.Fatalf("update %d\n%s", code, out.String())
	}
	if got := readTestFile(t, desktop); strings.Contains(got, "/old") {
		t.Errorf("entry not refreshed:\n%s", got)
	}
}

func readTestFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestInstallWithoutYesStillAsks(t *testing.T) {
	home, _, _ := installEnvDirs(t)
	var out strings.Builder
	if code := RunInstall(strings.NewReader(""), &out, false); code != 1 { // EOF: abort
		t.Errorf("code %d, want 1", code)
	}
	if _, err := os.Stat(filepath.Join(home, ".local", "bin", "ts6tray")); err == nil {
		t.Error("installed without an answer")
	}
}

func TestUninstallReversesInstallAndKeepsConfig(t *testing.T) {
	home, config, cache := installEnvDirs(t)
	var out strings.Builder
	if code := RunInstall(neverReader{t}, &out, true); code != 0 {
		t.Fatalf("install %d\n%s", code, out.String())
	}
	if err := os.MkdirAll(filepath.Join(config, "ts6tray"), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(config, "ts6tray", "config")
	if err := os.WriteFile(cfg, []byte("notify.poke=on\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	out.Reset()
	if code := RunUninstall(neverReader{t}, &out, true); code != 0 {
		t.Fatalf("uninstall %d\n%s", code, out.String())
	}
	for _, p := range []string{
		filepath.Join(home, ".local", "bin", "ts6tray"),
		filepath.Join(config, "autostart", "ts6tray.desktop"),
		filepath.Join(cache, "ts6tray"),
	} {
		if _, err := os.Lstat(p); err == nil {
			t.Errorf("%s still exists", p)
		}
	}
	if _, err := os.Stat(cfg); err != nil {
		t.Errorf("config was removed: %v", err)
	}
	if !strings.Contains(out.String(), "Proceed? [y/N] yes (--yes)\n") {
		t.Errorf("output lacks the logged answer:\n%s", out.String())
	}

	// nothing left to do the second time
	out.Reset()
	if code := RunUninstall(neverReader{t}, &out, true); code != 0 || !strings.Contains(out.String(), "nothing to remove") {
		t.Errorf("second uninstall: code %d\n%s", code, out.String())
	}
}

func TestUninstallAsksWithoutYes(t *testing.T) {
	home, _, _ := installEnvDirs(t)
	var out strings.Builder
	if code := RunInstall(neverReader{t}, &out, true); code != 0 {
		t.Fatalf("install %d", code)
	}
	bin := filepath.Join(home, ".local", "bin", "ts6tray")

	if code := RunUninstall(strings.NewReader("n\n"), &out, false); code != 1 {
		t.Errorf("answer n: code %d, want 1", code)
	}
	if _, err := os.Stat(bin); err != nil {
		t.Error("binary removed after answering no")
	}
	if code := RunUninstall(strings.NewReader("y\n"), &out, false); code != 0 {
		t.Errorf("answer y: code %d, want 0", code)
	}
	if _, err := os.Stat(bin); err == nil {
		t.Error("binary kept after answering yes")
	}
}

// A desktop file that is not ours stays.
func TestUninstallLeavesForeignDesktopFile(t *testing.T) {
	_, config, _ := installEnvDirs(t)
	p := filepath.Join(config, "autostart", "ts6tray.desktop")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("[Desktop Entry]\nName=other\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	RunUninstall(neverReader{t}, &out, true)
	if _, err := os.Stat(p); err != nil {
		t.Errorf("foreign desktop file removed: %v", err)
	}
}

func TestInstallParseArgs(t *testing.T) {
	for _, c := range []struct {
		args    []string
		yes, ok bool
	}{
		{nil, false, true},
		{[]string{"--yes"}, true, true},
		{[]string{"-y"}, true, true},
		{[]string{"--no"}, false, false},
		{[]string{"--yes", "extra"}, false, false},
	} {
		yes, ok := installParseArgs(c.args)
		if yes != c.yes || ok != c.ok {
			t.Errorf("installParseArgs(%q) = %v, %v; want %v, %v", c.args, yes, ok, c.yes, c.ok)
		}
	}
}
