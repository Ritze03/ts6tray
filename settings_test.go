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
		"Key binding (one-time)\n",
		"  ▸ Bind microphone key\n",
		"  ▸ Bind speaker key\n",
		"Left-click on the tray icon   ‹ microphone ›",
		"\nNotifications\n",
		"\nDelivery\n",
		"  Show notifications for   ‹ 5 s ›",
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
	s.cursor = settingsRowClick + 1 // the first notification switch
	out := settingsRender(s)
	if n := strings.Count(out, settingsReverse); n != 1 {
		t.Fatalf("%d highlighted rows, want 1", n)
	}
	want := settingsReverse + "  [x] " + trayNotifyGroups[0].label + settingsReset
	if !strings.Contains(out, want) {
		t.Errorf("the highlight is not on the first switch row:\n%s", out)
	}
	if strings.Contains(settingsRender(settingsState{notif: s.notif}), want) {
		t.Error("that row is highlighted with the cursor on row 0")
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
	s.cursor = settingsRowClick + 1 // "Someone joins or leaves my channel", on by default
	key := settingsRowKeys()[s.cursor]

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
		s.cursor = settingsRowClick
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
	s.cursor = settingsRowClick + 1
	if next, act := settingsPress(s, "right"); act != settingsActNone || next.notif == nil {
		t.Errorf("right on a switch row did something: act %v", act)
	}
	// …nor on a bind row, where there is nothing to cycle through.
	s.cursor = 0
	if next, act := settingsPress(s, "left"); act != settingsActNone || next.bind != "" {
		t.Errorf("left on a bind row started something: act %v bind %q", act, next.bind)
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
	s.cursor = settingsRowClick + 1
	s, _ = settingsPress(s, "toggle")
	s, _ = settingsPress(s, "left") // a no-op on a switch row

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
	s.cursor = settingsRowClick
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

// --- the key-binding helper -------------------------------------------------

// TestSettingsBindStartsACountdown: activating a bind row puts the target and
// the full five seconds into the state, and writes nothing.
func TestSettingsBindStartsACountdown(t *testing.T) {
	for i, want := range []string{"mic", "speaker"} {
		s := testState(t)
		s.cursor = i
		s.status = "something old"

		next, act := settingsPress(s, "toggle")
		if act != settingsActNone {
			t.Errorf("row %d: act = %v, want none (the press comes at zero)", i, act)
		}
		if next.bind != want {
			t.Errorf("row %d: bind = %q, want %q", i, next.bind, want)
		}
		if next.left != settingsBindSeconds {
			t.Errorf("row %d: left = %d, want %d", i, next.left, settingsBindSeconds)
		}
		if next.status != "" {
			t.Errorf("row %d: the stale status %q survived", i, next.status)
		}
		if s.bind != "" {
			t.Error("the press mutated the state it was given")
		}
	}
}

// TestSettingsBindTicksDownAndPresses: a tick a second, then the press action
// for whichever target was counting down.
func TestSettingsBindTicksDownAndPresses(t *testing.T) {
	for _, c := range []struct {
		row  int
		want settingsAct
	}{
		{0, settingsActPressMic},
		{1, settingsActPressSpeaker},
	} {
		s := testState(t)
		s.cursor = c.row
		s, _ = settingsPress(s, "toggle")

		for i := 1; i < settingsBindSeconds; i++ {
			var act settingsAct
			s, act = settingsPress(s, "tick")
			if act != settingsActNone {
				t.Fatalf("row %d: tick %d already acted (%v)", c.row, i, act)
			}
			if want := settingsBindSeconds - i; s.left != want {
				t.Fatalf("row %d: after %d ticks left = %d, want %d", c.row, i, s.left, want)
			}
			if s.bind == "" {
				t.Fatalf("row %d: the countdown stopped at tick %d", c.row, i)
			}
		}

		var act settingsAct
		s, act = settingsPress(s, "tick")
		if act != c.want {
			t.Errorf("row %d: final act = %v, want %v", c.row, act, c.want)
		}
		if s.bind != "" || s.left != 0 {
			t.Errorf("row %d: the countdown did not clear: bind %q left %d", c.row, s.bind, s.left)
		}
		// Further ticks are harmless no-ops.
		if _, act := settingsPress(s, "tick"); act != settingsActNone {
			t.Errorf("row %d: a tick with nothing running acted (%v)", c.row, act)
		}
	}
}

// TestSettingsBindCancels: Esc and q both stop the countdown without quitting;
// a second q then quits as usual.
func TestSettingsBindCancels(t *testing.T) {
	s := testState(t)
	s, _ = settingsPress(s, "toggle")
	s, _ = settingsPress(s, "tick")

	next, act := settingsPress(s, "quit")
	if act != settingsActQuit && next.bind != "" {
		t.Fatalf("quit during a countdown: bind %q act %v", next.bind, act)
	}
	if act != settingsActNone {
		t.Errorf("the first quit returned %v, want none — it only cancels", act)
	}
	if next.bind != "" || next.left != 0 {
		t.Errorf("the countdown survived: bind %q left %d", next.bind, next.left)
	}
	if next.status != settingsCancelled {
		t.Errorf("status = %q, want %q", next.status, settingsCancelled)
	}
	if _, act := settingsPress(next, "quit"); act != settingsActQuit {
		t.Errorf("the second quit returned %v, want quit", act)
	}
	// A cancelled countdown never presses anything.
	if _, act := settingsPress(next, "tick"); act != settingsActNone {
		t.Errorf("a tick after cancelling acted (%v)", act)
	}
}

// TestSettingsBindRestartsOnASecondActivation: aiming at the other key while
// one is counting down retargets it and starts the five seconds over.
func TestSettingsBindRestartsOnASecondActivation(t *testing.T) {
	s := testState(t)
	s, _ = settingsPress(s, "toggle")
	for i := 0; i < 4; i++ {
		s, _ = settingsPress(s, "tick")
	}
	if s.left != settingsBindSeconds-4 {
		t.Fatalf("left = %d, want %d", s.left, settingsBindSeconds-4)
	}

	// Same row: restart.
	s, _ = settingsPress(s, "toggle")
	if s.bind != "mic" || s.left != settingsBindSeconds {
		t.Errorf("re-activating mic: bind %q left %d, want mic and %d", s.bind, s.left, settingsBindSeconds)
	}

	// Other row: retarget and restart.
	s, _ = settingsPress(s, "tick")
	s.cursor = 1
	s, _ = settingsPress(s, "toggle")
	if s.bind != "speaker" || s.left != settingsBindSeconds {
		t.Errorf("switching to speaker: bind %q left %d", s.bind, s.left)
	}
}

// TestSettingsRenderShowsTheCountdown: the status line turns into the
// instructions, counting down, and the hint says how to stop.
func TestSettingsRenderShowsTheCountdown(t *testing.T) {
	s := testState(t)
	s.status = "daemon: reloaded ✓"
	s, _ = settingsPress(s, "toggle")
	for i := 0; i < 3; i++ {
		s, _ = settingsPress(s, "tick")
	}

	out := plain(settingsRender(s))
	want := "Switch to TeamSpeak → Settings → Key Bindings → Microphone: Toggle → Choose … pressing in 2 s"
	if !strings.Contains(out, want) {
		t.Errorf("render is missing %q:\n%s", want, out)
	}
	if !strings.Contains(out, settingsBindHint) {
		t.Errorf("render does not say how to cancel:\n%s", out)
	}
	if strings.Contains(out, "reloaded") {
		t.Errorf("the old status is still on screen under the countdown:\n%s", out)
	}

	s.bind, s.left = "speaker", 1
	if !strings.Contains(plain(settingsRender(s)), "Speaker: Toggle → Choose … pressing in 1 s") {
		t.Errorf("the speaker countdown reads wrong:\n%s", plain(settingsRender(s)))
	}
}

// TestSettingsPressKeyStatus: what the user is told after the press, success
// or failure, with no daemon spelled out rather than shown as a raw error.
func TestSettingsPressKeyStatus(t *testing.T) {
	s := testState(t)

	var got string
	ok := settingsPressKey(s, "mic", func(target string) error { got = target; return nil })
	if got != "mic" {
		t.Errorf("pressed %q, want mic", got)
	}
	if ok.status != settingsPressedOK {
		t.Errorf("status = %q, want %q", ok.status, settingsPressedOK)
	}

	bad := settingsPressKey(s, "speaker", func(string) error { return errors.New(ipcNotRunning) })
	if bad.status != ipcNotRunning {
		t.Errorf("status = %q, want the daemon-not-running message", bad.status)
	}
	if none := settingsPressKey(s, "mic", nil); none.status != settingsNoDaemon {
		t.Errorf("status with no press func = %q", none.status)
	}
}

// --- the notification display time ------------------------------------------

// TestSettingsTimeoutRowCycles: the last Delivery row cycles through the
// values with space and the arrows, and saves like everything else.
func TestSettingsTimeoutRowCycles(t *testing.T) {
	s := testState(t)
	s.cursor = len(settingsRowKeys()) - 1
	if key := settingsRowKeys()[s.cursor]; key != trayOptTimeout {
		t.Fatalf("the last row is %q, want the display time", key)
	}
	if s.timeout != trayNotifyTimeoutDef {
		t.Fatalf("loaded timeout = %q, want %q", s.timeout, trayNotifyTimeoutDef)
	}

	for _, c := range []struct {
		key  string
		step int
	}{{"toggle", 1}, {"left", -1}, {"right", 1}} {
		next, act := settingsPress(s, c.key)
		if act != settingsActWrite {
			t.Errorf("%s: act = %v, want a write", c.key, act)
		}
		if want := trayNextTimeout(s.timeout, c.step); next.timeout != want {
			t.Errorf("%s: timeout = %q, want %q", c.key, next.timeout, want)
		}
	}

	// Walk the whole cycle and watch the label follow.
	path := trayConfigPath()
	for _, want := range append(append([]string{}, trayNotifyTimeouts[3:]...), trayNotifyTimeouts[:3]...) {
		s, _ = settingsPress(s, "toggle")
		if s.timeout != want {
			t.Fatalf("cycled to %q, want %q", s.timeout, want)
		}
		s = settingsSave(path, s, nil)
		if got := trayReadTimeout(path); got != want {
			t.Errorf("on disk: %q, want %q", got, want)
		}
		if label := "‹ " + trayNotifyTimeoutLabel(want) + " ›"; !strings.Contains(plain(settingsRender(s)), label) {
			t.Errorf("the row does not show %q", label)
		}
	}
}
