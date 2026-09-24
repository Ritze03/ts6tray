package main

// settings_test.go never opens a terminal: the whole TUI is a pure render and a
// pure key handler over settingsState, plus one write-and-reload step that
// takes its reload func as a parameter.

import (
	"bufio"
	"errors"
	"strings"
	"testing"
)

func bufioReader(s string) *bufio.Reader { return bufio.NewReader(strings.NewReader(s)) }

// plain is what the screen looks like with the escape sequences taken out, so a
// test can assert on the text the user reads.
func plain(s string) string {
	s = strings.ReplaceAll(s, settingsReverse, "")
	return strings.ReplaceAll(s, settingsReset, "")
}

func testState(t *testing.T) settingsState {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	return settingsLoad(trayConfigPath())
}

// TestSettingsRenderShowsEveryRow: every switch in the config appears exactly
// once, with a box that matches its state, under the right heading.
func TestSettingsRenderShowsEveryRow(t *testing.T) {
	s := testState(t)
	out := plain(settingsRender(s))

	for _, want := range []string{
		"ts6tray settings",
		settingsHint,
		"Left-click on the tray icon   ‹ microphone ›",
		"\nNotifications\n",
		"\nDelivery\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("render is missing %q:\n%s", want, out)
		}
	}
	for _, g := range trayNotifySwitches {
		want := "  " + settingsBox(g.def) + " " + g.label + "\n"
		if !strings.Contains(out, want) {
			t.Errorf("render is missing the row %q:\n%s", want, out)
		}
	}
	// The defaults the user asked for, read straight off the screen.
	if !strings.Contains(out, "[ ] My own changes") ||
		!strings.Contains(out, "[ ] Server messages") ||
		!strings.Contains(out, "[ ] Silence while my speakers are muted") {
		t.Errorf("the three new switches are not off by default:\n%s", out)
	}
}

// TestSettingsRenderHighlightsTheCursor: exactly the cursor's row is reverse
// video, and it moves with the cursor.
func TestSettingsRenderHighlightsTheCursor(t *testing.T) {
	s := testState(t)
	s.cursor = 1 // the first notification switch
	out := settingsRender(s)
	if n := strings.Count(out, settingsReverse); n != 1 {
		t.Fatalf("%d highlighted rows, want 1", n)
	}
	want := settingsReverse + "  [x] " + trayNotifyGroups[0].label + settingsReset
	if !strings.Contains(out, want) {
		t.Errorf("the highlight is not on row 1:\n%s", out)
	}
	if strings.Contains(settingsRender(settingsState{notif: s.notif}), want) {
		t.Error("row 1 is highlighted with the cursor on row 0")
	}
}

// TestSettingsRenderShowsTheStatus: the daemon line is whatever the last write
// put there.
func TestSettingsRenderShowsTheStatus(t *testing.T) {
	s := testState(t)
	s.status = settingsReloadedOK
	if !strings.Contains(plain(settingsRender(s)), settingsReloadedOK) {
		t.Error("the status line is not on screen")
	}
}

// TestSettingsPressMoves: j/k and the arrows walk the rows and stop at both
// ends rather than wrapping into nowhere.
func TestSettingsPressMoves(t *testing.T) {
	s := testState(t)
	last := len(settingsRowKeys()) - 1

	s, act := settingsPress(s, "up")
	if s.cursor != 0 || act != settingsActNone {
		t.Errorf("up at the top: cursor %d, act %v", s.cursor, act)
	}
	for i := 0; i < 3; i++ {
		s, _ = settingsPress(s, "down")
	}
	if s.cursor != 3 {
		t.Errorf("cursor = %d after three downs, want 3", s.cursor)
	}
	s, _ = settingsPress(s, "up")
	if s.cursor != 2 {
		t.Errorf("cursor = %d after an up, want 2", s.cursor)
	}
	for i := 0; i < len(settingsRowKeys())+5; i++ {
		s, _ = settingsPress(s, "down")
	}
	if s.cursor != last {
		t.Errorf("cursor = %d at the bottom, want %d", s.cursor, last)
	}
	// An unknown key changes nothing.
	before := s.cursor
	s, act = settingsPress(s, "none")
	if s.cursor != before || act != settingsActNone {
		t.Errorf("an unknown key moved the cursor: %d -> %d (%v)", before, s.cursor, act)
	}
}

// TestSettingsPressToggles: space flips the switch under the cursor and asks
// for a write, and nothing else moves.
func TestSettingsPressToggles(t *testing.T) {
	s := testState(t)
	s.cursor = 1 // "Someone joins or leaves my channel", on by default
	key := settingsRowKeys()[1]

	next, act := settingsPress(s, "toggle")
	if act != settingsActWrite {
		t.Errorf("act = %v, want a write", act)
	}
	if next.notif[key] {
		t.Errorf("%s is still on", key)
	}
	if !s.notif[key] {
		t.Error("the toggle mutated the state it was given instead of returning a new one")
	}
	for _, g := range trayNotifySwitches {
		if g.key != key && next.notif[g.key] != s.notif[g.key] {
			t.Errorf("%s changed too", g.key)
		}
	}
	// Enter does the same.
	if again, _ := settingsPress(next, "toggle"); !again.notif[key] {
		t.Error("a second toggle did not turn it back on")
	}
}

// TestSettingsPressFlipsTheClickTarget: row 0 is not a checkbox — space and the
// left/right arrows all swap microphone for speaker.
func TestSettingsPressFlipsTheClickTarget(t *testing.T) {
	for _, key := range []string{"toggle", "left", "right"} {
		s := testState(t)
		next, act := settingsPress(s, key)
		if act != settingsActWrite || next.click != "speaker" {
			t.Errorf("%s: click = %q, act = %v", key, next.click, act)
		}
		back, _ := settingsPress(next, key)
		if back.click != "mic" {
			t.Errorf("%s: click = %q after flipping back, want mic", key, back.click)
		}
	}
	// Left and right do nothing on a checkbox row.
	s := testState(t)
	s.cursor = 1
	if next, act := settingsPress(s, "right"); act != settingsActNone || next.cursor != 1 {
		t.Errorf("right on a switch row did something: act %v", act)
	}
}

func TestSettingsPressQuits(t *testing.T) {
	s := testState(t)
	if _, act := settingsPress(s, "quit"); act != settingsActQuit {
		t.Errorf("act = %v, want quit", act)
	}
}

// TestSettingsSaveWritesAndReloads: every toggle goes to disk at once and the
// daemon is told, with the outcome in the status line.
func TestSettingsSaveWritesAndReloads(t *testing.T) {
	s := testState(t)
	path := trayConfigPath()
	s.cursor = 1
	s, _ = settingsPress(s, "toggle")
	s, _ = settingsPress(s, "left") // and the click target, from row 0's neighbour

	reloads := 0
	s = settingsSave(path, s, func() error { reloads++; return nil })
	if reloads != 1 {
		t.Errorf("reload called %d times, want 1", reloads)
	}
	if s.status != settingsReloadedOK {
		t.Errorf("status = %q, want %q", s.status, settingsReloadedOK)
	}

	// On disk, and read back by the same helpers the daemon uses.
	if got := trayReadNotify(path)[trayNotifyGroups[0].key]; got {
		t.Error("the flipped switch did not reach the file")
	}
	if got := trayReadClick(path); got != "mic" {
		// "left" on row 1 is a no-op, so the click target is untouched.
		t.Errorf("click = %q, want mic", got)
	}
}

// TestSettingsSaveKeepsForeignKeys: the file is rewritten whole, so a key this
// build knows nothing about — or one the tray just wrote — has to survive.
func TestSettingsSaveKeepsForeignKeys(t *testing.T) {
	s := testState(t)
	path := trayConfigPath()
	kv := trayReadConfig(path)
	kv["something.else"] = "42"
	if err := trayWriteConfig(path, kv); err != nil {
		t.Fatal(err)
	}
	s, _ = settingsPress(s, "toggle") // flip the click target
	s = settingsSave(path, s, nil)
	if got := trayReadConfig(path)["something.else"]; got != "42" {
		t.Errorf("the foreign key is %q, want 42", got)
	}
	if got := trayReadClick(path); got != "speaker" {
		t.Errorf("click = %q, want speaker", got)
	}
}

// TestSettingsSaveStatusLines: no daemon is not an error the user has to act
// on, but any other failure is reported as it came.
func TestSettingsSaveStatusLines(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reload func() error
		want   string
	}{
		{"no daemon at all", nil, settingsNoDaemon},
		{"the socket is dead", func() error { return errors.New(ipcNotRunning) }, settingsNoDaemon},
		{"the daemon complained", func() error { return errors.New("no tray icon") }, "daemon: no tray icon"},
		{"it worked", func() error { return nil }, settingsReloadedOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := testState(t)
			if got := settingsSave(trayConfigPath(), s, tc.reload).status; got != tc.want {
				t.Errorf("status = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestSettingsReadKey: the byte a terminal sends becomes the name the pure
// handler understands.
func TestSettingsReadKey(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"j", "down"}, {"k", "up"}, {"h", "left"}, {"l", "right"},
		{" ", "toggle"}, {"\r", "toggle"}, {"\n", "toggle"},
		{"q", "quit"}, {"Q", "quit"}, {"\x03", "quit"}, {"\x04", "quit"},
		{"\x1b[A", "up"}, {"\x1b[B", "down"}, {"\x1b[C", "right"}, {"\x1b[D", "left"},
		{"\x1bOA", "up"},
		{"\x1b", "quit"}, // a bare Esc with nothing behind it
		{"z", "none"},
		{"\x1b[Z", "none"}, // shift-tab: known shape, no meaning here
	} {
		got, err := settingsReadKey(bufioReader(tc.in))
		if err != nil || got != tc.want {
			t.Errorf("%q -> %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
	if _, err := settingsReadKey(bufioReader("")); err == nil {
		t.Error("a closed stdin did not report an error")
	}
}
