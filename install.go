package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const installPromptChoice = "Choice [1-3]: "

const installDesktopTemplate = `[Desktop Entry]
Type=Application
Name=ts6tray
Comment=TeamSpeak 6 tray daemon
Exec=%s --daemon
Icon=%s
NoDisplay=true
X-GNOME-Autostart-enabled=true
`

// RunInstall installs the running binary to ~/.local/bin/ts6tray and optionally
// sets up autorun. It asks before writing anything. Returns an exit code.
func RunInstall(in io.Reader, out io.Writer) int {
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(out, "cannot determine home directory: %v\n", err)
		return 1
	}
	binDir := filepath.Join(home, ".local", "bin")
	target := filepath.Join(binDir, "ts6tray")

	src, err := os.Executable()
	if err != nil {
		fmt.Fprintf(out, "cannot determine own executable: %v\n", err)
		return 1
	}
	if resolved, rerr := filepath.EvalSymlinks(src); rerr == nil {
		src = resolved
	}

	fmt.Fprintf(out, "Install ts6tray to %s.\n", target)
	fmt.Fprintln(out, "Start the tray automatically at login?")
	fmt.Fprintf(out, "  1) XDG autostart (%s) — GNOME, KDE, most desktops\n", filepath.Join(installConfigDir(), "autostart", "ts6tray.desktop"))
	fmt.Fprintln(out, "  2) Manual autostart — print the command for your own autostart setup")
	fmt.Fprintln(out, "  3) No autorun")

	choice, ok := installAsk(in, out)
	if !ok {
		fmt.Fprintln(out, "Aborted; nothing was written.")
		return 1
	}

	var written []string

	if tgt, terr := filepath.EvalSymlinks(target); terr == nil && tgt == src {
		fmt.Fprintf(out, "Already installed at %s; skipping copy.\n", target)
	} else {
		if err := installCopy(src, target); err != nil {
			fmt.Fprintf(out, "install failed: %v\n", err)
			return 1
		}
		written = append(written, target)
	}

	switch choice {
	case "1":
		path, err := installDesktop(target)
		if err != nil {
			fmt.Fprintf(out, "autostart entry failed: %v\n", err)
			return 1
		}
		written = append(written, path)
	case "2":
		fmt.Fprintln(out, "\nAdd this command to whatever starts programs at login on your system")
		fmt.Fprintln(out, "(compositor config, desktop session settings, a systemd user unit, …):")
		fmt.Fprintf(out, "  %s --daemon\n", target)
	}

	fmt.Fprintln(out)
	if len(written) == 0 {
		fmt.Fprintln(out, "Nothing written.")
	} else {
		fmt.Fprintln(out, "Written:")
		for _, w := range written {
			fmt.Fprintf(out, "  %s\n", w)
		}
	}

	if !installOnPath(binDir) {
		fmt.Fprintf(out, "\nNote: %s is not on your $PATH. Add it, e.g.:\n  export PATH=\"$HOME/.local/bin:$PATH\"\n", binDir)
	}
	fmt.Fprintln(out, "\nStart it now with: ts6tray --daemon")
	return 0
}

// installAsk reads a choice of 1-3, re-asking on invalid input.
// ok is false on EOF.
func installAsk(in io.Reader, out io.Writer) (string, bool) {
	r := bufio.NewReader(in)
	for {
		fmt.Fprint(out, installPromptChoice)
		line, err := r.ReadString('\n')
		choice := strings.TrimSpace(line)
		switch choice {
		case "1", "2", "3":
			return choice, true
		}
		if err != nil {
			fmt.Fprintln(out)
			return "", false
		}
		fmt.Fprintln(out, "Please enter 1, 2 or 3.")
	}
}

// installConfigDir returns the user config dir, falling back to ~/.config.
func installConfigDir() string {
	if dir, err := os.UserConfigDir(); err == nil {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config")
}

// installCopy copies src to target atomically with mode 0755.
func installCopy(src, target string) error {
	dir := filepath.Dir(target)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	tmp, err := os.CreateTemp(dir, ".ts6tray-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if _, err := io.Copy(tmp, in); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o755); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, target)
}

// installDesktop writes the XDG autostart entry and returns its path.
func installDesktop(target string) (string, error) {
	dir := filepath.Join(installConfigDir(), "autostart")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, "ts6tray.desktop")
	// The daemon writes the same file at start; doing it here too means the
	// entry has a real icon even before the tray has ever run.
	icon, err := trayIconFile()
	if err != nil {
		return "", err
	}
	content := fmt.Sprintf(installDesktopTemplate, target, icon)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// installOnPath reports whether dir is listed in $PATH.
func installOnPath(dir string) bool {
	for _, p := range filepath.SplitList(os.Getenv("PATH")) {
		if p == "" {
			continue
		}
		if p == dir {
			return true
		}
		if abs, err := filepath.Abs(p); err == nil && abs == dir {
			return true
		}
	}
	return false
}
