package main

// settings.go is `ts6tray settings`: a small full-screen terminal UI over the
// config file (config.go). It is where every setting lives; the tray menu has
// only the toggles, the server list and Quit.
//
// It is a separate, short-lived process on purpose. The daemon holds a
// WebSocket, a D-Bus connection and the rendered icons; a settings screen has
// no business growing that resident set. Every change is written through
// immediately and the running daemon is told to re-read the file.
//
// The key-binding helper is here too: it counts down out loud while the user
// switches to TeamSpeak's hotkey assignment, then asks the daemon over IPC to
// press the virtual key once so TeamSpeak records it.
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
	"time"

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
	settingsBindHint   = "Esc or q cancels"
	settingsNoDaemon   = "daemon not running — changes apply on next start"
	settingsReloadedOK = "daemon: reloaded ✓"
	settingsNotATTY    = "ts6tray settings needs a terminal; run it in one, or edit ~/.config/ts6tray/config"
	settingsPressedOK  = "Key pressed — TeamSpeak should have recorded it."
	settingsCancelled  = "Key binding cancelled."
)

// settingsBindSeconds is how long the helper counts down before pressing, so
// the user can get to TeamSpeak's assignment dialog. settingsTick is how often
// the countdown advances. Both are variables so tests can shorten them.
var (
	settingsBindSeconds = 10
	settingsTick        = time.Second
)

// settingsBindRows are the two key-binding rows, in screen order. The key is
// also the press target the IPC request carries.
var settingsBindRows = []struct{ target, label string }{
	{"mic", "Bind microphone key"},
	{"speaker", "Bind speaker key"},
}

// settingsState is the whole UI. click and notif mirror the config file; cursor
// is the selected row, counted over settingsRowKeys.
type settingsState struct {
	click   string
	notif   map[string]bool
	timeout string // one of trayNotifyTimeouts
	cursor  int
	status  string

	// bind is the target of a running key-binding countdown ("" for none) and
	// left the seconds still to go. They are state, not a goroutine, so the
	// whole helper stays inside the pure state machine.
	bind string
	left int
}

// settingsRowClick is the index of the left-click row: straight after the bind
// rows, which are the top of the screen.
var settingsRowClick = len(settingsBindRows)

// settingsRowKeys names the selectable rows in screen order. Only the switch
// rows have a config key of their own; the rest are named after what they do.
func settingsRowKeys() []string {
	keys := make([]string, 0, len(settingsBindRows)+2+len(trayNotifySwitches))
	for _, b := range settingsBindRows {
		keys = append(keys, "bind."+b.target)
	}
	keys = append(keys, "click")
	for _, g := range trayNotifySwitches {
		keys = append(keys, g.key)
	}
	return append(keys, trayOptTimeout)
}

// settingsLoad reads the current config into a fresh state.
func settingsLoad(path string) settingsState {
	return settingsState{
		click:   trayReadClick(path),
		notif:   trayReadNotify(path),
		timeout: trayReadTimeout(path),
	}
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

	hint := settingsHint
	if s.bind != "" {
		hint = settingsBindHint
	}
	fmt.Fprintf(&b, "%-37s%s\n\n", "ts6tray settings", hint)

	b.WriteString("Key binding (one-time)\n")
	for _, r := range settingsBindRows {
		line("  ▸ " + r.label)
	}

	what := "microphone"
	if s.click == "speaker" {
		what = "speaker"
	}
	b.WriteString("\n")
	line("Left-click on the tray icon   ‹ " + what + " ›")

	b.WriteString("\nNotifications\n")
	for _, g := range trayNotifyGroups {
		line("  " + settingsBox(s.notif[g.key]) + " " + g.label)
	}
	b.WriteString("\nDelivery\n")
	for _, o := range trayNotifyOptions {
		line("  " + settingsBox(s.notif[o.key]) + " " + o.label)
	}
	line("  Show notifications for   ‹ " + trayNotifyTimeoutLabel(s.timeout) + " ›")

	status := s.status
	if s.bind != "" {
		status = settingsCountdown(s.bind, s.left)
	}
	b.WriteString("\n" + status + "\n")
	return b.String()
}

// settingsCountdown is the live instruction line under a running countdown. It
// spells out the whole path through TeamSpeak's settings, because the user has
// ten seconds to walk it and no time to go looking.
func settingsCountdown(target string, left int) string {
	what := "Microphone"
	if target == "speaker" {
		what = "Speaker"
	}
	return fmt.Sprintf("Switch to TeamSpeak → Settings → Key Bindings → %s: Toggle → Choose … pressing in %d s",
		what, left)
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
	// The countdown reached zero: press that target's virtual key.
	settingsActPressMic
	settingsActPressSpeaker
)

// settingsPress applies one event to the state. key is one of the names
// settingsReadKey produces — up, down, left, right, toggle, quit, none — or
// "tick", which the main loop sends once a second while a countdown runs.
func settingsPress(s settingsState, key string) (settingsState, settingsAct) {
	rows := len(settingsRowKeys())
	switch key {
	case "tick":
		if s.bind == "" {
			return s, settingsActNone
		}
		s.left--
		if s.left > 0 {
			return s, settingsActNone
		}
		target := s.bind
		s.bind, s.left = "", 0
		if target == "speaker" {
			return s, settingsActPressSpeaker
		}
		return s, settingsActPressMic
	case "quit":
		// Esc and q both arrive here. While a countdown runs they stop it;
		// pressing q again then quits, as it would have the first time.
		if s.bind != "" {
			s.bind, s.left, s.status = "", 0, settingsCancelled
			return s, settingsActNone
		}
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
		// Only the two cycling rows have sides to move between; elsewhere the
		// key does nothing rather than surprising anyone.
		if s.cursor != settingsRowClick && settingsRowKeys()[s.cursor] != trayOptTimeout {
			return s, settingsActNone
		}
		return settingsToggle(s), settingsActWrite
	case "toggle":
		if s.cursor < len(settingsBindRows) {
			// Activating a bind row starts the countdown, or restarts it when
			// one is already running for another target.
			s.bind = settingsBindRows[s.cursor].target
			s.left = settingsBindSeconds
			s.status = ""
			return s, settingsActNone
		}
		return settingsToggle(s), settingsActWrite
	}
	return s, settingsActNone
}

// settingsToggle flips or cycles whatever the cursor is on: the click target,
// the display time, or a switch. The map is copied, so the returned state never
// shares storage with the one that went in.
func settingsToggle(s settingsState) settingsState {
	notif := make(map[string]bool, len(s.notif))
	for k, v := range s.notif {
		notif[k] = v
	}
	s.notif = notif
	if s.cursor == settingsRowClick {
		s.click = trayOther(s.click)
		if s.click != "speaker" {
			s.click = "mic"
		}
		return s
	}
	key := settingsRowKeys()[s.cursor]
	if key == trayOptTimeout {
		s.timeout = trayNextTimeout(s.timeout)
		return s
	}
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
	if err := trayWriteTimeout(path, s.timeout); err != nil {
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

// settingsPressKey asks the daemon to press one virtual key, which is what the
// countdown was counting down to. press is a parameter for the same reason
// reload is: tests hand it a fake.
func settingsPressKey(s settingsState, target string, press func(string) error) settingsState {
	if press == nil {
		s.status = settingsNoDaemon
		return s
	}
	if err := press(target); err != nil {
		s.status = err.Error()
		return s
	}
	s.status = settingsPressedOK
	return s
}

// settingsPressIPC sends one `press mic|speaker` to the daemon, reusing the
// CLI's wire code so there is only one implementation of it.
func settingsPressIPC(target string) error {
	var buf strings.Builder
	if code := RunIPCClient(SocketPath(), []string{"press", target}, &buf); code != 0 {
		return errors.New(strings.TrimSpace(buf.String()))
	}
	return nil
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

	// The countdown needs the loop to hear from a clock as well as the
	// keyboard, and settingsReadKey blocks, so reading moves to a goroutine and
	// the loop selects. It ends when stdin does; the process is on its way out
	// by then, so nothing has to join it.
	keys := make(chan string)
	go func() {
		defer close(keys)
		r := bufio.NewReader(f)
		for {
			key, err := settingsReadKey(r)
			if err != nil {
				return
			}
			keys <- key
		}
	}()

	tick := time.NewTicker(settingsTick)
	defer tick.Stop()

	for {
		fmt.Fprint(out, settingsClear+settingsRender(s))

		// Only listen to the clock while something is counting down, so an
		// idle screen is not redrawn once a second.
		var ticks <-chan time.Time
		if s.bind != "" {
			ticks = tick.C
		}
		var key string
		select {
		case k, ok := <-keys:
			if !ok {
				return 0 // stdin closed: leave as if quit
			}
			key = k
		case <-ticks:
			key = "tick"
		}

		was := s.bind
		var act settingsAct
		s, act = settingsPress(s, key)
		if s.bind != "" && s.bind != was {
			// A countdown just started or was retargeted: drop whatever the
			// ticker buffered while nobody was listening, so the first second
			// is a whole one.
			select {
			case <-tick.C:
			default:
			}
			tick.Reset(settingsTick)
		}
		switch act {
		case settingsActQuit:
			return 0
		case settingsActWrite:
			s = settingsSave(path, s, settingsReload)
		case settingsActPressMic:
			s = settingsPressKey(s, "mic", settingsPressIPC)
		case settingsActPressSpeaker:
			s = settingsPressKey(s, "speaker", settingsPressIPC)
		}
	}
}
