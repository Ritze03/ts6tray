package main

import (
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
	if code := RunInstall(strings.NewReader("2\n"), &out); code != 0 {
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
