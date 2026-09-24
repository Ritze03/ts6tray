package main

// settings.go is `ts6tray settings`: a small full-screen terminal UI for the
// same config file the tray menu writes (config.go).
//
// It is a separate, short-lived process on purpose. The daemon holds a
// WebSocket, a D-Bus connection and the rendered icons; a settings screen has
// no business growing that resident set, and the user wanted the main settings
// to be reachable from a terminal anyway. Every toggle is written through
// immediately and the running daemon is told to re-read the file, so the tray
// checkmarks follow along.
//
// Everything that decides *what is on screen* is a pure function of state —
// settingsRender and settingsPress — so the whole UI is testable without a
// terminal. Only RunSettings touches the tty.

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"
)

// ANSI: alternate screen in and out, cursor hidden and shown, home + erase.
const (
	settingsAltEnter   = "\x1b[?1049h"
	settingsAltLeave   = "\x1b[?1049l"
	settingsHideCursor = "\x1b[?25l"
	settingsShowCursor = "\x1b[?25h"
	settingsClear      = "\x1b[H\x1b[2J"
	settingsReverse    = "\x1b[7m"
	settingsReset      = "\x1b[0m"
)

const (
	settingsHint       = "↑↓/jk move · space/enter toggle · q quit"
	settingsNoDaemon   = "daemon not running — changes apply on next start"
	settingsReloadedOK = "daemon: reloaded ✓"
	settingsNotATTY    = "ts6tray settings needs a terminal; run it in one, or edit ~/.config/ts6tray/config"
)

// settingsState is the whole UI. click and notif mirror the config file; cursor
// is the selected row, counted over settingsRowKeys.
type settingsState struct {
	click  string
	notif  map[string]bool
	cursor int
	status string
}

// settingsRowKeys are the config keys of the selectable rows, in screen order.
// Row 0 is the click target, which is not a switch, so its key is "click".
func settingsRowKeys() []string {
	keys := make([]string, 0, 1+len(trayNotifySwitches))
	keys = append(keys, "click")
	for _, g := range trayNotifySwitches {
		keys = append(keys, g.key)
	}
	return keys
}

// settingsLoad reads the current config into a fresh state.
func settingsLoad(path string) settingsState {
	return settingsState{click: trayReadClick(path), notif: trayReadNotify(path)}
}

// --- pure rendering --------------------------------------------------------

// settingsRender draws the whole screen. The selected row is reverse video;
// nothing else is coloured, so it reads the same on every theme.
func settingsRender(s settingsState) string {
	var b strings.Builder
	row := 0
	// line writes one row of the list, highlighting it when it is the cursor's.
	line := func(text string) {
		if row == s.cursor {
			b.WriteString(settingsReverse + text + settingsReset)
		} else {
			b.WriteString(text)
		}
		b.WriteString("\n")
		row++
	}

	fmt.Fprintf(&b, "%-37s%s\n\n", "ts6tray settings", settingsHint)

	what := "microphone"
	if s.click == "speaker" {
		what = "speaker"
	}
	line("Left-click on the tray icon   ‹ " + what + " ›")

	b.WriteString("\nNotifications\n")
	for _, g := range trayNotifyGroups {
		line("  " + settingsBox(s.notif[g.key]) + " " + g.label)
	}
	b.WriteString("\nDelivery\n")
	for _, o := range trayNotifyOptions {
		line("  " + settingsBox(s.notif[o.key]) + " " + o.label)
	}
	b.WriteString("\n" + s.status + "\n")
	return b.String()
}

func settingsBox(on bool) string {
	if on {
		return "[x]"
	}
	return "[ ]"
}

// --- pure input ------------------------------------------------------------

// settingsAct is what the caller has to do after a keypress.
type settingsAct int

const (
	settingsActNone settingsAct = iota
	settingsActWrite
	settingsActQuit
)

// settingsPress applies one key to the state. key is one of the names
// settingsReadKey produces: up, down, left, right, toggle, quit, none.
func settingsPress(s settingsState, key string) (settingsState, settingsAct) {
	rows := len(settingsRowKeys())
	switch key {
	case "quit":
		return s, settingsActQuit
	case "up":
		if s.cursor > 0 {
			s.cursor--
		}
		return s, settingsActNone
	case "down":
		if s.cursor < rows-1 {
			s.cursor++
		}
		return s, settingsActNone
	case "left", "right":
		// Only the click row has two sides; elsewhere there is nothing to move
		// to, so the key does nothing rather than surprising anyone.
		if s.cursor != 0 {
			return s, settingsActNone
		}
		return settingsToggle(s), settingsActWrite
	case "toggle":
		return settingsToggle(s), settingsActWrite
	}
	return s, settingsActNone
}

// settingsToggle flips whatever the cursor is on: the click target on row 0, a
// switch anywhere else. The map is copied, so the returned state never shares
// storage with the one that went in.
func settingsToggle(s settingsState) settingsState {
	notif := make(map[string]bool, len(s.notif))
	for k, v := range s.notif {
		notif[k] = v
	}
	s.notif = notif
	if s.cursor == 0 {
		s.click = trayOther(s.click)
		if s.click != "speaker" {
			s.click = "mic"
		}
		return s
	}
	key := settingsRowKeys()[s.cursor]
	s.notif[key] = !s.notif[key]
	return s
}

// --- write and reload ------------------------------------------------------

// settingsSave persists the state and asks the daemon to re-read it, putting
// the outcome in the status line. reload is a parameter so tests can hand it a
// fake instead of a socket.
//
// ponytail: there is no cross-process lock on the config file. The tray writes
// it from menu clicks and this writes it too; both do a read-modify-write of
// the whole file, so the worst case is that the last writer wins on a key the
// other one had just changed — one setting, within the same second, which is
// not worth a lockfile and its stale-lock problems.
func settingsSave(path string, s settingsState, reload func() error) settingsState {
	if err := trayWriteClick(path, s.click); err != nil {
		s.status = "could not save: " + err.Error()
		return s
	}
	if err := trayWriteNotify(path, s.notif); err != nil {
		s.status = "could not save: " + err.Error()
		return s
	}
	if reload == nil {
		s.status = settingsNoDaemon
		return s
	}
	if err := reload(); err != nil {
		if strings.Contains(err.Error(), ipcNotRunning) {
			s.status = settingsNoDaemon
		} else {
			s.status = "daemon: " + err.Error()
		}
		return s
	}
	s.status = settingsReloadedOK
	return s
}

// settingsReload sends one `reload` request to the daemon, reusing the CLI's
// wire code so there is only one implementation of it.
func settingsReload() error {
	var buf strings.Builder
	if code := RunIPCClient(SocketPath(), []string{"reload"}, &buf); code != 0 {
		return errors.New(strings.TrimSpace(buf.String()))
	}
	return nil
}

// --- the terminal ----------------------------------------------------------

// settingsReadKey turns the next keypress into a name. Arrow keys arrive as a
// three-byte escape sequence; a lone Esc (nothing buffered behind it) quits.
func settingsReadKey(r *bufio.Reader) (string, error) {
	b, err := r.ReadByte()
	if err != nil {
		return "", err
	}
	switch b {
	case 'q', 'Q', 0x03, 0x04: // q, Ctrl-C, Ctrl-D
		return "quit", nil
	case 'j':
		return "down", nil
	case 'k':
		return "up", nil
	case 'h':
		return "left", nil
	case 'l':
		return "right", nil
	case ' ', '\r', '\n':
		return "toggle", nil
	case 0x1b:
		if r.Buffered() < 2 {
			return "quit", nil
		}
		intro, _ := r.ReadByte()
		final, _ := r.ReadByte()
		if intro != '[' && intro != 'O' {
			return "none", nil
		}
		switch final {
		case 'A':
			return "up", nil
		case 'B':
			return "down", nil
		case 'C':
			return "right", nil
		case 'D':
			return "left", nil
		}
	}
	return "none", nil
}

// RunSettings is the `ts6tray settings` subcommand. It returns the process exit
// code: 0 on a clean quit, 2 when stdin is not a terminal.
func RunSettings(in io.Reader, out io.Writer) int {
	f, ok := in.(*os.File)
	if !ok {
		fmt.Fprintln(out, settingsNotATTY)
		return 2
	}
	fd := int(f.Fd())
	old, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		fmt.Fprintln(out, settingsNotATTY)
		return 2
	}

	// One restore, however we leave: normal return, panic, or a signal. Under
	// raw mode ISIG is off, so Ctrl-C arrives as a byte and is handled as a
	// key; the signal handler is for the kill that comes from elsewhere.
	var once sync.Once
	restore := func() {
		unix.IoctlSetTermios(fd, unix.TCSETS, old)
		fmt.Fprint(out, settingsShowCursor+settingsAltLeave)
	}
	defer once.Do(restore)

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigs)
	go func() {
		if _, ok := <-sigs; !ok {
			return
		}
		once.Do(restore)
		os.Exit(1)
	}()

	raw := *old
	raw.Lflag &^= unix.ECHO | unix.ICANON | unix.ISIG
	raw.Iflag &^= unix.IXON | unix.ICRNL
	raw.Cc[unix.VMIN] = 1
	raw.Cc[unix.VTIME] = 0
	if err := unix.IoctlSetTermios(fd, unix.TCSETS, &raw); err != nil {
		fmt.Fprintln(out, "ts6tray settings: cannot set raw mode: "+err.Error())
		return 2
	}
	fmt.Fprint(out, settingsAltEnter+settingsHideCursor)

	path := trayConfigPath()
	s := settingsLoad(path)
	r := bufio.NewReader(f)
	for {
		fmt.Fprint(out, settingsClear+settingsRender(s))
		key, err := settingsReadKey(r)
		if err != nil {
			return 0 // stdin closed: leave as if quit
		}
		var act settingsAct
		s, act = settingsPress(s, key)
		switch act {
		case settingsActQuit:
			return 0
		case settingsActWrite:
			s = settingsSave(path, s, settingsReload)
		}
	}
}
