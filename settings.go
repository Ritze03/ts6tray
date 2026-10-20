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
	"unicode/utf8"

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
	settingsEditHint   = "type, then enter saves · Esc cancels"
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
	settingsBindSeconds = 5
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
	timeout string     // one of trayNotifyTimeouts
	alarm   alarmWatch // alarm.nick and alarm.channel, shown in the Alarms rows
	cursor  int
	status  string

	// bind is the target of a running key-binding countdown ("" for none) and
	// left the seconds still to go. They are state, not a goroutine, so the
	// whole helper stays inside the pure state machine.
	bind string
	left int

	// editing is the config key of the text row being typed into — the
	// alarm's name or channel — or "" for none, and edit is what has been
	// typed so far. Enter stores it; Esc throws it away.
	editing string
	edit    string
}

// settingsRowClick is the index of the left-click row: straight after the bind
// rows, which are the top of the screen.
var settingsRowClick = len(settingsBindRows)

// settingsRowKeys names the selectable rows in screen order. Only the switch
// rows and the alarm's name row have a config key of their own; the rest are
// named after what they do.
func settingsRowKeys() []string {
	keys := make([]string, 0, len(settingsBindRows)+3+len(trayNotifySwitches))
	for _, b := range settingsBindRows {
		keys = append(keys, "bind."+b.target)
	}
	keys = append(keys, "click")
	for _, g := range trayNotifyGroups {
		keys = append(keys, g.key)
	}
	for _, g := range trayAlarmGroups {
		keys = append(keys, g.key)
	}
	keys = append(keys, trayAlarmNickKey, trayAlarmChannelKey)
	for _, g := range trayNotifyOptions {
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
		alarm:   trayReadAlarm(path),
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
	switch {
	case s.editing != "":
		hint = settingsEditHint
	case s.bind != "":
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
	b.WriteString("\nAlarms\n")
	for _, g := range trayAlarmGroups {
		line("  " + settingsBox(s.notif[g.key]) + " " + trayAlarmLabel(s.alarm))
	}
	line("  " + settingsTextRow(s, trayAlarmNickKey))
	line("  " + settingsTextRow(s, trayAlarmChannelKey))
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

// settingsTextRow is one of the alarm's two text rows: what is being typed,
// with a cursor mark since the terminal's own is hidden, or the stored value
// and how to change it.
func settingsTextRow(s settingsState, key string) string {
	label, value := "Name:    ", s.alarm.nick
	if key == trayAlarmChannelKey {
		label, value = "Channel: ", s.alarm.channel
	}
	switch {
	case s.editing == key:
		return label + s.edit + "_"
	case value == "":
		return label + "not set   (enter to type one)"
	}
	return label + value + "   (enter to change)"
}

// settingsCountdown is the live instruction line under a running countdown. It
// spells out the whole path through TeamSpeak's settings, because the user has
// five seconds to walk it and no time to go looking.
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
// settingsReadKey produces — up, down, left, right, enter, esc, backspace,
// quit, none, or char:<c> for a typed character — one of the commands
// settingsKeyName turns them into, or "tick", which the main loop sends once a
// second while a countdown runs.
func settingsPress(s settingsState, key string) (settingsState, settingsAct) {
	if s.editing != "" && key != "tick" {
		return settingsEditKey(s, key)
	}
	key = settingsKeyName(key)
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
		// Esc, q and Ctrl-C all arrive here. While a countdown runs they stop
		// it; pressing q again then quits, as it would have the first time.
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
		step := 1
		if key == "left" {
			step = -1
		}
		return settingsToggle(s, step), settingsActWrite
	case "toggle":
		if s.cursor < len(settingsBindRows) {
			// Activating a bind row starts the countdown, or restarts it when
			// one is already running for another target.
			s.bind = settingsBindRows[s.cursor].target
			s.left = settingsBindSeconds
			s.status = ""
			return s, settingsActNone
		}
		switch key := settingsRowKeys()[s.cursor]; key {
		case trayAlarmNickKey, trayAlarmChannelKey:
			// Start from the stored value, so a typo is one backspace away.
			s.editing, s.status = key, ""
			s.edit = s.alarm.nick
			if key == trayAlarmChannelKey {
				s.edit = s.alarm.channel
			}
			return s, settingsActNone
		}
		return settingsToggle(s, 1), settingsActWrite
	}
	return s, settingsActNone
}

// settingsKeyName turns what the terminal sent into a command, outside the
// name editor: there the letters are text, here they are shortcuts.
func settingsKeyName(key string) string {
	switch key {
	case "char:j":
		return "down"
	case "char:k":
		return "up"
	case "char:h":
		return "left"
	case "char:l":
		return "right"
	case "char: ", "enter":
		return "toggle"
	case "char:q", "char:Q", "esc":
		return "quit"
	}
	return key
}

// settingsEditKey applies one key to the text being typed. Enter stores it,
// trimmed the way the config file would trim it, and asks for a write. An
// empty name watches for nobody; an empty channel goes back to the default,
// since "any channel at all" is not what an alarm for the lobby means. Esc or
// Ctrl-C leaves the stored value as it was. Every other key that is not text
// is ignored, so a stray arrow cannot drop what has been typed.
func settingsEditKey(s settingsState, key string) (settingsState, settingsAct) {
	switch {
	case key == "enter":
		v := strings.TrimSpace(s.edit)
		if s.editing == trayAlarmChannelKey {
			if v == "" {
				v = trayAlarmChannelDef
			}
			s.alarm.channel = v
		} else {
			s.alarm.nick = v
		}
		s.editing, s.edit = "", ""
		return s, settingsActWrite
	case key == "esc" || key == "quit":
		s.editing, s.edit = "", ""
	case key == "backspace":
		if r := []rune(s.edit); len(r) > 0 {
			s.edit = string(r[:len(r)-1])
		}
	case strings.HasPrefix(key, "char:"):
		if utf8.RuneCountInString(s.edit) < noticeMaxNick {
			s.edit += strings.TrimPrefix(key, "char:")
		}
	}
	return s, settingsActNone
}

// settingsToggle flips or cycles whatever the cursor is on: the click target,
// the display time, or a switch. step is which way the display time moves, +1
// forward and -1 back; the other rows have two values, so it does not matter
// there. The map is copied, so the returned state never shares storage with
// the one that went in.
func settingsToggle(s settingsState, step int) settingsState {
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
		s.timeout = trayNextTimeout(s.timeout, step)
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
	if err := trayWriteAlarm(path, s.alarm); err != nil {
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
// three-byte escape sequence; a lone Esc (nothing buffered behind it) is "esc".
// Every printable character, letters with shortcuts included, comes back as
// char:<c>: whether "j" means down or the letter j depends on whether the name
// is being typed, which only settingsPress knows.
func settingsReadKey(r *bufio.Reader) (string, error) {
	b, err := r.ReadByte()
	if err != nil {
		return "", err
	}
	switch b {
	case 0x03, 0x04: // Ctrl-C, Ctrl-D
		return "quit", nil
	case '\r', '\n':
		return "enter", nil
	case 0x7f, 0x08: // what Backspace sends, depending on the terminal
		return "backspace", nil
	case 0x1b:
		if r.Buffered() < 2 {
			return "esc", nil
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
		return "none", nil
	}
	if b < 0x20 {
		return "none", nil
	}
	if b >= utf8.RuneSelf {
		_ = r.UnreadByte()
		c, _, err := r.ReadRune()
		if err != nil || c == utf8.RuneError {
			return "none", nil
		}
		return "char:" + string(c), nil
	}
	return "char:" + string(rune(b)), nil
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
