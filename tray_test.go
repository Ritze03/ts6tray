package main

// tray_test.go deliberately never touches the session bus: connecting would
// claim a name and make a real icon appear in the user's panel. Everything here
// is pure Go, including the reproduction of what godbus/prop does to a property
// value, which needs no connection at all.

import (
	"bytes"
	"context"
	"image"
	"image/color"
	_ "image/png"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
)

// TestTrayPixmapsForCopies is the direct half of the regression: what callers
// get back must not share memory with trayPixmapCache.
func TestTrayPixmapsForCopies(t *testing.T) {
	got := trayPixmapsFor(IconQuiet)
	if len(got) == 0 || len(got[0].Data) == 0 {
		t.Fatalf("trayPixmapsFor(IconQuiet) = %d pixmaps, want artwork", len(got))
	}
	want := append([]byte(nil), trayPixmapCache[IconQuiet][0].Data...)

	got[0].Data[0] ^= 0xff

	if !bytes.Equal(trayPixmapCache[IconQuiet][0].Data, want) {
		t.Error("mutating the returned pixmap changed trayPixmapCache: " +
			"trayPixmapsFor hands out the cache itself")
	}
}

// TestTrayPixmapsSurviveAPropStore reproduces the actual bug without a bus.
//
// prop.Export keeps *[]trayPixmap pointing at the value it was given, and
// prop.Properties.SetMust stores each new value through that pointer with
// dbus.Store, which writes into the existing backing arrays. The two lines
// below are exactly copyProps + set. If trayPixmapsFor returned the cache, the
// talking artwork would land inside trayPixmapCache[IconQuiet].
func TestTrayPixmapsSurviveAPropStore(t *testing.T) {
	quietBefore := clonePixmaps(trayPixmapsFor(IconQuiet))

	// prop.copyProps: a pointer to the exported value (the quiet pixmaps).
	exported := reflect.New(reflect.TypeOf([]trayPixmap(nil)))
	exported.Elem().Set(reflect.ValueOf(trayPixmapsFor(IconQuiet)))

	// prop.Properties.set: store the talking pixmaps through that pointer.
	if err := dbus.Store([]any{trayPixmapsFor(IconTalking)}, exported.Interface()); err != nil {
		t.Fatalf("dbus.Store: %v", err)
	}

	if !pixmapsEqual(trayPixmapsFor(IconQuiet), quietBefore) {
		t.Error("publishing the talking icon corrupted the quiet icon: " +
			"the exported IconPixmap aliases trayPixmapCache")
	}
	if pixmapsEqual(trayPixmapsFor(IconQuiet), trayPixmapsFor(IconTalking)) {
		t.Error("quiet and talking pixmaps are now identical")
	}
}

// TestTrayIconsAreDistinct is cheap insurance for the generated artwork: six
// states must not render to the same 22 px image.
func TestTrayIconsAreDistinct(t *testing.T) {
	icons := []Icon{IconNone, IconQuiet, IconTalking, IconMicMuted, IconSpeakerMuted, IconMicDisabled}
	for i, a := range icons {
		for _, b := range icons[i+1:] {
			pa, pb := pixmapAt(t, a, 22), pixmapAt(t, b, 22)
			if bytes.Equal(pa, pb) {
				t.Errorf("icons %v and %v render identically at 22 px", a, b)
			}
		}
	}
}

func pixmapAt(t *testing.T, ic Icon, size int32) []byte {
	t.Helper()
	for _, p := range trayPixmapsFor(ic) {
		if p.Width == size && p.Height == size {
			if len(p.Data) != int(size*size*4) {
				t.Fatalf("icon %v at %d px: %d bytes, want %d", ic, size, len(p.Data), size*size*4)
			}
			return p.Data
		}
	}
	t.Fatalf("icon %v has no %d px pixmap", ic, size)
	return nil
}

func clonePixmaps(ps []trayPixmap) []trayPixmap {
	out := make([]trayPixmap, len(ps))
	for i, p := range ps {
		out[i] = trayPixmap{Width: p.Width, Height: p.Height, Data: append([]byte(nil), p.Data...)}
	}
	return out
}

func pixmapsEqual(a, b []trayPixmap) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Width != b[i].Width || a[i].Height != b[i].Height || !bytes.Equal(a[i].Data, b[i].Data) {
			return false
		}
	}
	return true
}

// --- the Settings submenu ---------------------------------------------------

// newTestTray is a tray with no bus and no TSClient: enough for the menu model,
// the layout methods and the event dispatch, all of which are pure Go.
func newTestTray(t *testing.T) *tray {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	tr := &tray{click: trayReadClick(trayConfigPath())}
	tr.refresh()
	return tr
}

func nodeChildren(n trayMenuNode) []trayMenuNode {
	out := make([]trayMenuNode, 0, len(n.Children))
	for _, c := range n.Children {
		out = append(out, c.Value().(trayMenuNode))
	}
	return out
}

func findNode(ns []trayMenuNode, id int32) (trayMenuNode, bool) {
	for _, n := range ns {
		if n.ID == id {
			return n, true
		}
	}
	return trayMenuNode{}, false
}

func propOf(t *testing.T, n trayMenuNode, name string) any {
	t.Helper()
	v, ok := n.Props[name]
	if !ok {
		t.Fatalf("item %d has no %q property (props: %v)", n.ID, name, n.Props)
	}
	return v.Value()
}

// TestTraySettingsLayout pins the shape the user asked for: a Settings submenu
// sitting directly above Quit, holding the two bind helpers, a separator and
// the two left-click radios with microphone selected by default.
func TestTraySettingsLayout(t *testing.T) {
	tr := newTestTray(t)

	_, root, derr := tr.GetLayout(trayIDRoot, -1, nil)
	if derr != nil {
		t.Fatalf("GetLayout: %v", derr)
	}
	top := nodeChildren(root)
	var ids []int32
	for _, n := range top {
		ids = append(ids, n.ID)
	}
	if len(ids) < 2 || ids[len(ids)-1] != trayIDQuit || ids[len(ids)-2] != trayIDSettings {
		t.Fatalf("root children %v: want Settings (%d) directly before Quit (%d)",
			ids, trayIDSettings, trayIDQuit)
	}
	// The flat toggles stay where they were.
	if _, ok := findNode(top, trayIDMic); !ok {
		t.Error("the flat \"Toggle microphone mute\" item disappeared")
	}
	if _, ok := findNode(top, trayIDSpeaker); !ok {
		t.Error("the flat \"Toggle speaker mute\" item disappeared")
	}

	settings, _ := findNode(top, trayIDSettings)
	if got := propOf(t, settings, "label"); got != "Settings" {
		t.Errorf("Settings label = %q, want %q", got, "Settings")
	}
	if got := propOf(t, settings, "children-display"); got != "submenu" {
		t.Errorf("Settings children-display = %v, want submenu", got)
	}

	kids := nodeChildren(settings)
	var kidIDs []int32
	for _, n := range kids {
		kidIDs = append(kidIDs, n.ID)
	}
	want := []int32{trayIDBindMic, trayIDBindSpeaker, trayIDSep3, trayIDClickMic, trayIDClickSpeaker,
		trayIDSep4, trayIDNotify}
	if !reflect.DeepEqual(kidIDs, want) {
		t.Fatalf("Settings children = %v, want %v", kidIDs, want)
	}
	if got := propOf(t, kids[0], "label"); got != "Bind microphone key…" {
		t.Errorf("bind mic label = %q", got)
	}
	if got := propOf(t, kids[1], "label"); got != "Bind speaker key…" {
		t.Errorf("bind speaker label = %q", got)
	}
	if got := propOf(t, kids[2], "type"); got != "separator" {
		t.Errorf("Settings child 3 type = %v, want separator", got)
	}
	for _, n := range kids[3:5] {
		if got := propOf(t, n, "toggle-type"); got != "radio" {
			t.Errorf("item %d toggle-type = %v, want radio", n.ID, got)
		}
	}
	if got := propOf(t, kids[3], "toggle-state"); got != int32(1) {
		t.Errorf("microphone radio toggle-state = %v, want 1 (the default)", got)
	}
	if got := propOf(t, kids[4], "toggle-state"); got != int32(0) {
		t.Errorf("speaker radio toggle-state = %v, want 0", got)
	}

	// Depth 1 must stop at the submenu row itself, as dbusmenu prescribes.
	_, shallow, derr := tr.GetLayout(trayIDRoot, 1, nil)
	if derr != nil {
		t.Fatalf("GetLayout depth 1: %v", derr)
	}
	s1, _ := findNode(nodeChildren(shallow), trayIDSettings)
	if len(s1.Children) != 0 {
		t.Errorf("GetLayout depth 1 returned %d Settings children, want 0", len(s1.Children))
	}

	// Asking for the submenu directly must hand back its children.
	_, sub, derr := tr.GetLayout(trayIDSettings, -1, nil)
	if derr != nil {
		t.Fatalf("GetLayout(Settings): %v", derr)
	}
	if len(sub.Children) != len(want) {
		t.Errorf("GetLayout(Settings) returned %d children, want %d", len(sub.Children), len(want))
	}
}

// TestTraySetClickFlipsRadios covers the click on the speaker radio: the marks
// move, the revision moves with them (which is what makes the host redraw), and
// the choice is on disk.
func TestTraySetClickFlipsRadios(t *testing.T) {
	tr := newTestTray(t)
	before := tr.revision

	tr.setClick("speaker")

	if tr.revision <= before {
		t.Errorf("revision %d after selecting speaker, want more than %d", tr.revision, before)
	}
	if got := tr.clickTarget(); got != "speaker" {
		t.Errorf("clickTarget = %q, want speaker", got)
	}
	_, sub, _ := tr.GetLayout(trayIDSettings, -1, nil)
	kids := nodeChildren(sub)
	mic, _ := findNode(kids, trayIDClickMic)
	spk, _ := findNode(kids, trayIDClickSpeaker)
	if propOf(t, mic, "toggle-state") != int32(0) || propOf(t, spk, "toggle-state") != int32(1) {
		t.Errorf("after selecting speaker: mic=%v speaker=%v, want 0 and 1",
			propOf(t, mic, "toggle-state"), propOf(t, spk, "toggle-state"))
	}
	if got := trayReadClick(trayConfigPath()); got != "speaker" {
		t.Errorf("config on disk says %q, want speaker", got)
	}

	// Selecting the same target again is a no-op, not a revision bump.
	rev := tr.revision
	tr.setClick("speaker")
	if tr.revision != rev {
		t.Errorf("re-selecting speaker bumped the revision to %d", tr.revision)
	}
}

// TestTrayClickConfigRoundTrip checks the one-line config file, including the
// modes of the file and its directory and the fallback for a garbage file.
func TestTrayClickConfigRoundTrip(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	path := trayConfigPath()

	if got := trayReadClick(path); got != "mic" {
		t.Errorf("missing config: %q, want mic", got)
	}
	for _, want := range []string{"speaker", "mic"} {
		if err := trayWriteClick(path, want); err != nil {
			t.Fatalf("trayWriteClick(%q): %v", want, err)
		}
		if got := trayReadClick(path); got != want {
			t.Errorf("round trip: wrote %q, read %q", want, got)
		}
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat config: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("config mode %v, want 0600", fi.Mode().Perm())
	}
	di, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("stat config dir: %v", err)
	}
	if di.Mode().Perm() != 0o700 {
		t.Errorf("config dir mode %v, want 0700", di.Mode().Perm())
	}

	for _, junk := range []string{"", "click=", "click=trumpet", "nonsense\n", "click speaker"} {
		if err := os.WriteFile(path, []byte(junk), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := trayReadClick(path); got != "mic" {
			t.Errorf("garbage config %q read as %q, want mic", junk, got)
		}
	}
}

// TestTrayActivateUsesConfiguredTarget: left click follows the setting, middle
// click takes the other one. No real SetMute runs; tr.mute is the hook.
func TestTrayActivateUsesConfiguredTarget(t *testing.T) {
	tr := newTestTray(t)
	got := make(chan string, 4)
	tr.mute = func(target, mode string) (bool, error) {
		if mode != "toggle" {
			t.Errorf("SetMute mode = %q, want toggle", mode)
		}
		got <- target
		return false, nil
	}

	next := func(what string) string {
		t.Helper()
		select {
		case v := <-got:
			return v
		case <-time.After(2 * time.Second):
			t.Fatalf("%s never reached SetMute", what)
			return ""
		}
	}

	tr.Activate(0, 0)
	if v := next("Activate with mic selected"); v != "mic" {
		t.Errorf("Activate toggled %q, want mic", v)
	}
	tr.SecondaryActivate(0, 0)
	if v := next("SecondaryActivate with mic selected"); v != "speaker" {
		t.Errorf("SecondaryActivate toggled %q, want speaker", v)
	}

	tr.setClick("speaker")
	tr.Activate(0, 0)
	if v := next("Activate with speaker selected"); v != "speaker" {
		t.Errorf("Activate toggled %q, want speaker", v)
	}
	tr.SecondaryActivate(0, 0)
	if v := next("SecondaryActivate with speaker selected"); v != "mic" {
		t.Errorf("SecondaryActivate toggled %q, want mic", v)
	}
}

// TestTrayBindHelperPresses walks the helper end to end with the delay turned
// down: it presses the right button once, and while it is counting down the row
// says so and a second click is dropped instead of queueing a second press.
func TestTrayBindHelperPresses(t *testing.T) {
	tr := newTestTray(t)
	pressed := make(chan string, 4)
	started := make(chan struct{})
	tr.press = func(button string) error {
		pressed <- button
		return nil
	}

	// Hold the first bind inside its countdown while a second click arrives.
	tr.bindDelay = 250 * time.Millisecond
	go func() {
		close(started)
		tr.bindKey("speaker")
	}()
	<-started

	deadline := time.Now().Add(2 * time.Second)
	for {
		tr.mu.RLock()
		pending := tr.binding
		tr.mu.RUnlock()
		if pending == "speaker" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the bind countdown never started")
		}
		time.Sleep(time.Millisecond)
	}

	// The row reports the countdown and is disabled.
	row, ok := tr.rowByID(trayIDBindSpeaker)
	if !ok {
		t.Fatal("no bind-speaker row")
	}
	if row.label != "Pressing speaker key in 10 s…" || row.enabled {
		t.Errorf("counting down: label %q enabled %v, want the pending label, disabled", row.label, row.enabled)
	}

	// A second click while one is pending is ignored.
	tr.bindKey("mic")

	select {
	case b := <-pressed:
		if b != ButtonSpeaker {
			t.Errorf("pressed %q, want %q", b, ButtonSpeaker)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the bind helper never pressed the button")
	}
	select {
	case b := <-pressed:
		t.Fatalf("a second press happened (%q); the countdown is not single-flight", b)
	case <-time.After(100 * time.Millisecond):
	}

	// Once it is over the row is back to normal and a new bind runs.
	row, _ = tr.rowByID(trayIDBindSpeaker)
	if row.label != "Bind speaker key…" || !row.enabled {
		t.Errorf("after the countdown: label %q enabled %v", row.label, row.enabled)
	}
	tr.bindDelay = time.Millisecond
	tr.bindKey("mic")
	select {
	case b := <-pressed:
		if b != ButtonMic {
			t.Errorf("second bind pressed %q, want %q", b, ButtonMic)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the second bind never pressed anything")
	}
}

// TestTrayBindHelperStopsOnShutdown: a pending countdown must not fire a button
// press after the tray's context is cancelled.
func TestTrayBindHelperStopsOnShutdown(t *testing.T) {
	tr := newTestTray(t)
	ctx, cancel := context.WithCancel(context.Background())
	tr.ctx = ctx
	tr.bindDelay = time.Minute
	pressed := make(chan string, 1)
	tr.press = func(button string) error { pressed <- button; return nil }

	done := make(chan struct{})
	go func() { tr.bindKey("mic"); close(done) }()
	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("bindKey did not return after the context was cancelled")
	}
	select {
	case b := <-pressed:
		t.Fatalf("pressed %q after shutdown", b)
	default:
	}
}

// TestTrayBindEventsDispatch is the wiring check: the ids the host clicks reach
// the right handlers.
func TestTrayBindEventsDispatch(t *testing.T) {
	tr := newTestTray(t)
	tr.bindDelay = time.Millisecond
	pressed := make(chan string, 2)
	tr.press = func(button string) error { pressed <- button; return nil }

	tr.Event(trayIDBindMic, "clicked", dbus.Variant{}, 0)
	select {
	case b := <-pressed:
		if b != ButtonMic {
			t.Errorf("clicking the bind-mic item pressed %q", b)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("clicking the bind-mic item pressed nothing")
	}

	tr.Event(trayIDClickSpeaker, "clicked", dbus.Variant{}, 0)
	deadline := time.Now().Add(2 * time.Second)
	for tr.clickTarget() != "speaker" {
		if time.Now().After(deadline) {
			t.Fatal("clicking the speaker radio did not change the click target")
		}
		time.Sleep(time.Millisecond)
	}
}

// --- the Notifications submenu ----------------------------------------------

// TestTrayNotificationsLayout pins the new submenu: it sits inside Settings
// after the left-click radios with a separator in front, its children are
// checkmarks in the declared order, and the two noisy kinds start off.
func TestTrayNotificationsLayout(t *testing.T) {
	tr := newTestTray(t)

	_, sub, derr := tr.GetLayout(trayIDSettings, -1, nil)
	if derr != nil {
		t.Fatalf("GetLayout(Settings): %v", derr)
	}
	kids := nodeChildren(sub)
	var ids []int32
	for _, n := range kids {
		ids = append(ids, n.ID)
	}
	want := []int32{trayIDBindMic, trayIDBindSpeaker, trayIDSep3, trayIDClickMic, trayIDClickSpeaker,
		trayIDSep4, trayIDNotify}
	if !reflect.DeepEqual(ids, want) {
		t.Fatalf("Settings children = %v, want %v", ids, want)
	}
	if got := propOf(t, kids[5], "type"); got != "separator" {
		t.Errorf("item before Notifications is %v, want a separator", got)
	}

	notif := kids[6]
	if got := propOf(t, notif, "label"); got != "Notifications" {
		t.Errorf("Notifications label = %q", got)
	}
	if got := propOf(t, notif, "children-display"); got != "submenu" {
		t.Errorf("Notifications children-display = %v, want submenu", got)
	}

	items := nodeChildren(notif)
	// The eight kind checkmarks, a separator, then the two delivery options.
	if want := len(trayNotifyGroups) + 1 + len(trayNotifyOptions); len(items) != want {
		t.Fatalf("Notifications has %d children, want %d", len(items), want)
	}
	for i, g := range trayNotifyGroups {
		n := items[i]
		if n.ID != trayIDNotify0+int32(i) {
			t.Errorf("child %d id = %d, want %d", i, n.ID, trayIDNotify0+int32(i))
		}
		if got := propOf(t, n, "label"); got != g.label {
			t.Errorf("child %d label = %q, want %q", i, got, g.label)
		}
		if got := propOf(t, n, "toggle-type"); got != "checkmark" {
			t.Errorf("child %d toggle-type = %v, want checkmark", i, got)
		}
		wantState := int32(0)
		if g.def {
			wantState = 1
		}
		if got := propOf(t, n, "toggle-state"); got != wantState {
			t.Errorf("%s default toggle-state = %v, want %v", g.key, got, wantState)
		}
	}

	// The defaults the user asked for: joins/leaves, moves, kicks and lost
	// connections on; mute churn, channel messages and — because TeamSpeak
	// already pops those up itself — private messages and pokes off.
	off := map[string]bool{"mute": true, "channelMsg": true, "privateMsg": true, "poke": true}
	for _, g := range trayNotifyGroups {
		if g.def == off[g.key] {
			t.Errorf("%s defaults to %v", g.key, g.def)
		}
	}

	// The separator, then the two options, both on by default.
	sep := items[len(trayNotifyGroups)]
	if sep.ID != trayIDSep5 {
		t.Errorf("separator id = %d, want %d", sep.ID, trayIDSep5)
	}
	if got := propOf(t, sep, "type"); got != "separator" {
		t.Errorf("item after the kinds is %v, want a separator", got)
	}
	for i, o := range trayNotifyOptions {
		n := items[len(trayNotifyGroups)+1+i]
		if n.ID != trayIDNotifyOpt0+int32(i) {
			t.Errorf("option %d id = %d, want %d", i, n.ID, trayIDNotifyOpt0+int32(i))
		}
		if got := propOf(t, n, "label"); got != o.label {
			t.Errorf("option %d label = %q, want %q", i, got, o.label)
		}
		if got := propOf(t, n, "toggle-type"); got != "checkmark" {
			t.Errorf("option %d toggle-type = %v, want checkmark", i, got)
		}
		if got := propOf(t, n, "toggle-state"); got != int32(1) {
			t.Errorf("%s default toggle-state = %v, want 1 (on)", o.key, got)
		}
	}
	if got := []string{trayNotifyOptions[0].label, trayNotifyOptions[1].label}; !reflect.DeepEqual(got,
		[]string{"Group bursts (0.5 s)", "Replace previous notification"}) {
		t.Errorf("option labels = %q", got)
	}
}

// TestTrayNotifyToggleRoundTrip: clicking a checkmark flips it, bumps the
// revision so the host redraws, writes it to disk and survives a restart.
func TestTrayNotifyToggleRoundTrip(t *testing.T) {
	tr := newTestTray(t)
	before := tr.revision

	// Turn the mute notices on (they default off) and the moves off.
	tr.Event(trayIDNotify0+3, "clicked", dbus.Variant{}, 0)
	tr.Event(trayIDNotify0+1, "clicked", dbus.Variant{}, 0)
	waitForNotify(t, tr, "mute", true)
	waitForNotify(t, tr, "moved", false)

	if tr.revision <= before {
		t.Errorf("revision %d after toggling, want more than %d", tr.revision, before)
	}
	if !tr.notifyEnabled(noticeMute) {
		t.Error("noticeMute is still filtered out")
	}
	if tr.notifyEnabled(noticeMoved) {
		t.Error("noticeMoved is still enabled")
	}
	// join and leave share one switch, so both must still be on.
	if !tr.notifyEnabled(noticeJoin) || !tr.notifyEnabled(noticeLeave) {
		t.Error("the joins/leaves switch moved on its own")
	}

	_, sub, _ := tr.GetLayout(trayIDNotify, -1, nil)
	items := nodeChildren(sub)
	if got := propOf(t, items[3], "toggle-state"); got != int32(1) {
		t.Errorf("mute checkmark = %v, want 1", got)
	}
	if got := propOf(t, items[1], "toggle-state"); got != int32(0) {
		t.Errorf("moved checkmark = %v, want 0", got)
	}

	// A fresh tray reads the same choices back off disk.
	got := trayReadNotify(trayConfigPath())
	if !got["mute"] || got["moved"] {
		t.Errorf("config on disk: mute=%v moved=%v, want true and false", got["mute"], got["moved"])
	}
	// …and the left-click setting was not trampled by the rewrite.
	if err := trayWriteClick(trayConfigPath(), "speaker"); err != nil {
		t.Fatal(err)
	}
	tr.toggleNotify("moved")
	if trayReadClick(trayConfigPath()) != "speaker" {
		t.Error("writing the notification switches lost click=speaker")
	}
	if !trayReadNotify(trayConfigPath())["mute"] {
		t.Error("writing click= lost the notification switches")
	}
}

// waitForNotify waits for an Event-dispatched toggle, which runs in its own
// goroutine.
func waitForNotify(t *testing.T, tr *tray, key string, want bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		tr.mu.RLock()
		got, ok := tr.notif[key]
		tr.mu.RUnlock()
		if ok && got == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("notify.%s never became %v", key, want)
}

// TestTrayConfigFormat covers the generalised key=value file: an old one-line
// click-only config still loads, unknown and malformed lines are ignored, and
// the file and its directory keep their modes.
func TestTrayConfigFormat(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	path := trayConfigPath()

	// Nothing on disk: all defaults.
	if got := trayReadClick(path); got != "mic" {
		t.Errorf("missing config click = %q, want mic", got)
	}
	if got, want := trayReadNotify(path), trayNotifyDefaults(); !reflect.DeepEqual(got, want) {
		t.Errorf("missing config notify = %v, want %v", got, want)
	}

	// The old format, written by a previous version.
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("click=speaker\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := trayReadClick(path); got != "speaker" {
		t.Errorf("old-format config click = %q, want speaker", got)
	}
	if got, want := trayReadNotify(path), trayNotifyDefaults(); !reflect.DeepEqual(got, want) {
		t.Errorf("old-format config notify = %v, want the defaults %v", got, want)
	}

	// A full file, with junk and unknown keys mixed in.
	body := "# a comment\n\nclick=speaker\nnotify.mute=on\nnotify.poke=off\n" +
		"notify.channelMsg=maybe\nnotify.nosuchthing=on\nnonsense\n  notify.moved = off  \n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	got := trayReadNotify(path)
	if !got["mute"] || got["poke"] || got["moved"] {
		t.Errorf("notify = %v: want mute on, poke off, moved off", got)
	}
	if got["channelMsg"] { // "maybe" is not a value: fall back to the default
		t.Errorf("notify.channelMsg=maybe read as on")
	}
	if !got["joinleave"] || !got["kicked"] || !got["connLost"] {
		t.Errorf("notify = %v: untouched keys lost their defaults", got)
	}
	if got["privateMsg"] { // off by default: TeamSpeak notifies about those
		t.Errorf("notify = %v: privateMsg should default off", got)
	}
	if _, ok := got["nosuchthing"]; ok {
		t.Error("an unknown notify key leaked into the settings")
	}
	if got := trayReadClick(path); got != "speaker" {
		t.Errorf("click = %q, want speaker", got)
	}

	// Writing keeps keys we do not own and the modes stay tight.
	if err := trayWriteNotify(path, map[string]bool{"poke": true}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(readFile(t, path), "click=speaker") {
		t.Errorf("rewriting dropped click=speaker:\n%s", readFile(t, path))
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("config mode %v, want 0600", fi.Mode().Perm())
	}
	di, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if di.Mode().Perm() != 0o700 {
		t.Errorf("config dir mode %v, want 0700", di.Mode().Perm())
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestTrayOnNoticeRespectsTheSwitches: onNotice is the whole filter between the
// TSClient and the desktop. With no bus t.notify only logs, so this checks the
// decision, not the D-Bus call.
func TestTrayOnNoticeRespectsTheSwitches(t *testing.T) {
	tr := newTestTray(t)
	for _, k := range []noticeKind{noticeJoin, noticeLeave, noticeMoved, noticeKicked, noticeConnLost} {
		if !tr.notifyEnabled(k) {
			t.Errorf("%v is off by default, want on", k)
		}
	}
	// TeamSpeak pops up its own notification for messages and pokes, so ours
	// would only double them.
	for _, k := range []noticeKind{noticeMute, noticeChannelMsg, noticePrivateMsg, noticePoke} {
		if tr.notifyEnabled(k) {
			t.Errorf("%v is on by default, want off", k)
		}
	}
	// An unknown kind is never shown rather than shown unconditionally.
	if tr.notifyEnabled(noticeKind(99)) {
		t.Error("an unmapped kind is notified")
	}
}

// --- notification delivery: batching, replacement, the app icon ------------

// notifyCall is one captured Notify, replaces_id and icon path included.
type notifyCall struct {
	replaces    uint32
	title, body string
	icon        string
}

// newNotifyTray builds a bus-free tray whose notifications land in a slice
// instead of on the session bus. window and cap shorten the batcher so the
// tests do not have to wait half a second at a time.
func newNotifyTray(t *testing.T, window, hardCap time.Duration) (*tray, func() []notifyCall) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	tr := &tray{notif: trayReadNotify(trayConfigPath())}

	var mu sync.Mutex
	var calls []notifyCall
	var next uint32
	tr.notifyFn = func(replaces uint32, title, body, icon string) uint32 {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, notifyCall{replaces, title, body, icon})
		next++
		if replaces != 0 {
			return replaces // a server keeps the id it was told to replace
		}
		return next
	}
	tr.batch = &noticeBatcher{window: window, cap: hardCap, send: tr.notifyEvent}
	return tr, func() []notifyCall {
		mu.Lock()
		defer mu.Unlock()
		return append([]notifyCall(nil), calls...)
	}
}

// waitCalls waits for at least n captured notifications, then returns them.
func waitCalls(t *testing.T, got func() []notifyCall, n int) []notifyCall {
	t.Helper()
	for i := 0; i < 400; i++ {
		if c := got(); len(c) >= n {
			return c
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("only %d notifications, want %d", len(got()), n)
	return nil
}

// TestTrayBatchBurst: five notices inside the window become one notification
// with the counted title and one escaped line each. The nickname carries "<b>"
// because titles are plain text but the batched body is markup.
func TestTrayBatchBurst(t *testing.T) {
	tr, got := newNotifyTray(t, 40*time.Millisecond, time.Second)
	for i := 0; i < 5; i++ {
		tr.onNotice(notice{kind: noticeMoved, title: "Mo moved <b> into your channel"})
	}
	if c := got(); len(c) != 0 {
		t.Fatalf("sent %d notifications during the burst, want none yet", len(c))
	}
	calls := waitCalls(t, got, 1)
	if len(calls) != 1 {
		t.Fatalf("got %d notifications, want exactly one: %+v", len(calls), calls)
	}
	if calls[0].title != "5 TeamSpeak events" {
		t.Errorf("title = %q, want %q", calls[0].title, "5 TeamSpeak events")
	}
	lines := strings.Split(calls[0].body, "\n")
	if len(lines) != 5 {
		t.Fatalf("body has %d lines, want 5:\n%s", len(lines), calls[0].body)
	}
	for i, l := range lines {
		want := strconv.Itoa(i+1) + ". Mo moved &lt;b&gt; into your channel"
		if l != want {
			t.Errorf("line %d = %q, want %q", i, l, want)
		}
	}
}

// TestTrayBatchSingle: one notice passes through with its own title and body,
// unescaped title and all.
func TestTrayBatchSingle(t *testing.T) {
	tr, got := newNotifyTray(t, 20*time.Millisecond, time.Second)
	tr.onNotice(notice{kind: noticeJoin, title: "Mo & co joined your channel", body: "hi"})
	calls := waitCalls(t, got, 1)
	if calls[0].title != "Mo & co joined your channel" || calls[0].body != "hi" {
		t.Errorf("passthrough = %+v", calls[0])
	}
}

// TestTrayBatchSlidingWindow: a notice inside the window pushes the flush out,
// so both land in one notification rather than the first going out alone.
func TestTrayBatchSlidingWindow(t *testing.T) {
	tr, got := newNotifyTray(t, 60*time.Millisecond, 5*time.Second)
	tr.onNotice(notice{kind: noticeJoin, title: "A joined your channel"})
	time.Sleep(30 * time.Millisecond)
	if c := got(); len(c) != 0 {
		t.Fatalf("flushed after half the window: %+v", c)
	}
	tr.onNotice(notice{kind: noticeJoin, title: "B joined your channel"})
	time.Sleep(40 * time.Millisecond) // past the first deadline, inside the second
	if c := got(); len(c) != 0 {
		t.Fatalf("the second notice did not slide the window: %+v", c)
	}
	calls := waitCalls(t, got, 1)
	if calls[0].title != "2 TeamSpeak events" {
		t.Errorf("title = %q, want the two notices merged", calls[0].title)
	}
	if calls[0].body != "1. B joined your channel\n2. A joined your channel" {
		t.Errorf("body = %q, want B (the newest) first", calls[0].body)
	}
}

// TestTrayBatchHardCap: a stream that never stops still gets flushed, because
// the cap is measured from the first notice of the batch.
func TestTrayBatchHardCap(t *testing.T) {
	tr, got := newNotifyTray(t, 50*time.Millisecond, 120*time.Millisecond)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			tr.onNotice(notice{kind: noticeJoin, title: "A joined your channel"})
			time.Sleep(10 * time.Millisecond)
		}
	}()
	calls := waitCalls(t, got, 1) // would never arrive without the cap
	close(stop)
	<-done
	if !strings.HasSuffix(calls[0].title, "TeamSpeak events") {
		t.Errorf("title = %q, want a batch", calls[0].title)
	}
}

// TestTrayBatchOff: with "Group bursts" off every notice goes out at once.
func TestTrayBatchOff(t *testing.T) {
	tr, got := newNotifyTray(t, time.Hour, time.Hour)
	tr.notif[trayOptBatch] = false
	tr.onNotice(notice{kind: noticeJoin, title: "A joined your channel"})
	tr.onNotice(notice{kind: noticeJoin, title: "B joined your channel"})
	calls := got()
	if len(calls) != 2 {
		t.Fatalf("got %d notifications, want 2 immediate ones: %+v", len(calls), calls)
	}
	if calls[0].title != "A joined your channel" || calls[1].title != "B joined your channel" {
		t.Errorf("calls = %+v", calls)
	}
}

// TestTrayNotifyReplace: with the option on the second notification carries the
// first one's id, so only the newest is on screen; with it off it carries 0 and
// they stack.
func TestTrayNotifyReplace(t *testing.T) {
	tr, got := newNotifyTray(t, time.Hour, time.Hour)
	tr.notif[trayOptBatch] = false

	tr.onNotice(notice{kind: noticeJoin, title: "A joined your channel"})
	tr.onNotice(notice{kind: noticeJoin, title: "B joined your channel"})
	calls := got()
	if calls[0].replaces != 0 {
		t.Errorf("first replaces_id = %d, want 0", calls[0].replaces)
	}
	if calls[1].replaces != 1 {
		t.Errorf("second replaces_id = %d, want the first id 1", calls[1].replaces)
	}

	tr.notif[trayOptReplace] = false
	tr.onNotice(notice{kind: noticeJoin, title: "C joined your channel"})
	if c := got(); c[2].replaces != 0 {
		t.Errorf("replaces_id with the option off = %d, want 0", c[2].replaces)
	}
}

// TestTrayNotifyOptionsRoundTrip: the two options persist as notify.batch and
// notify.replace and read back.
func TestTrayNotifyOptionsRoundTrip(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	path := trayConfigPath()

	if got := trayReadNotify(path); !got[trayOptBatch] || !got[trayOptReplace] {
		t.Errorf("defaults: batch=%v replace=%v, want both on", got[trayOptBatch], got[trayOptReplace])
	}

	m := trayNotifyDefaults()
	m[trayOptBatch] = false
	if err := trayWriteNotify(path, m); err != nil {
		t.Fatal(err)
	}
	raw := readFile(t, path)
	if !strings.Contains(raw, "notify.batch=off") || !strings.Contains(raw, "notify.replace=on") {
		t.Errorf("config file:\n%s", raw)
	}
	got := trayReadNotify(path)
	if got[trayOptBatch] || !got[trayOptReplace] {
		t.Errorf("read back: batch=%v replace=%v", got[trayOptBatch], got[trayOptReplace])
	}

	// Clicking the menu items flips them and saves.
	tr := &tray{notif: trayReadNotify(path)}
	tr.Event(trayIDNotifyOpt0+1, "clicked", dbus.Variant{}, 0)
	waitForNotify(t, tr, trayOptReplace, false)
	if trayReadNotify(path)[trayOptReplace] {
		t.Error("notify.replace was not persisted")
	}
	// batch was written off above and replace has just been clicked off.
	if tr.notifyOptOn(trayOptBatch) || tr.notifyOptOn(trayOptReplace) {
		t.Error("notifyOptOn disagrees with the switches")
	}
}

// TestTrayIconFile: the embedded icon lands in XDG_CACHE_HOME with the right
// modes, and an unchanged file is left alone rather than rewritten.
func TestTrayIconFile(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache)

	path, err := trayIconFile()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(cache, "ts6tray", "ts6tray.svg")
	if path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b, trayIconSVG) {
		t.Errorf("the file is not the embedded icon (%d vs %d bytes)", len(b), len(trayIconSVG))
	}
	if len(trayIconSVG) == 0 {
		t.Error("the embedded icon is empty")
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o644 {
		t.Errorf("icon mode %v, want 0644", fi.Mode().Perm())
	}
	di, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if di.Mode().Perm() != 0o700 {
		t.Errorf("icon dir mode %v, want 0700", di.Mode().Perm())
	}

	// Unchanged content: the file must not be touched.
	old := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	if _, err := trayIconFile(); err != nil {
		t.Fatal(err)
	}
	fi2, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !fi2.ModTime().Equal(old) {
		t.Errorf("the icon was rewritten though it had not changed (mtime %v, want %v)", fi2.ModTime(), old)
	}

	// Stale content: rewritten.
	if err := os.WriteFile(path, []byte("<svg/>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := trayIconFile(); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); !bytes.Equal(b, trayIconSVG) {
		t.Error("a stale icon file was not replaced")
	}
}

// --- the per-state notification icons --------------------------------------

// TestTrayStateIconFiles: the three PNGs land in XDG_CACHE_HOME, decode as
// PNGs of the expected size, and an unchanged file is left alone rather than
// rewritten — the same contract trayIconFile has.
func TestTrayStateIconFiles(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache)

	icons, err := trayStateIconFiles()
	if err != nil {
		t.Fatal(err)
	}
	want := map[Icon]string{
		IconMicMuted:     filepath.Join(cache, "ts6tray", "state-mic-muted.png"),
		IconSpeakerMuted: filepath.Join(cache, "ts6tray", "state-speaker-muted.png"),
		IconQuiet:        filepath.Join(cache, "ts6tray", "state-quiet.png"),
	}
	if !reflect.DeepEqual(icons, want) {
		t.Fatalf("paths = %v, want %v", icons, want)
	}
	// No icon is rendered for a state a mute notice can never report.
	for _, ic := range []Icon{IconNone, IconTalking, IconMicDisabled} {
		if p, ok := icons[ic]; ok {
			t.Errorf("%v has a state icon at %q, want none", ic, p)
		}
	}

	mods := map[Icon]time.Time{}
	for ic, p := range icons {
		fi, serr := os.Stat(p)
		if serr != nil {
			t.Fatal(serr)
		}
		mods[ic] = fi.ModTime()
		if fi.Mode().Perm() != 0o644 {
			t.Errorf("%v: mode %v, want 0644", ic, fi.Mode().Perm())
		}
		img := decodePNG(t, p)
		if b := img.Bounds(); b.Dx() != trayStateIconSize || b.Dy() != trayStateIconSize {
			t.Errorf("%v: %dx%d, want %dx%d", ic, b.Dx(), b.Dy(),
				trayStateIconSize, trayStateIconSize)
		}
	}

	// Written once: a second call must not touch the files.
	time.Sleep(10 * time.Millisecond)
	again, err := trayStateIconFiles()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(again, icons) {
		t.Errorf("second call = %v, want %v", again, icons)
	}
	for ic, p := range icons {
		fi, serr := os.Stat(p)
		if serr != nil {
			t.Fatal(serr)
		}
		if !fi.ModTime().Equal(mods[ic]) {
			t.Errorf("%v was rewritten although its content is unchanged", ic)
		}
	}
}

// decodePNG reads a PNG file, failing the test if it is not one.
func decodePNG(t *testing.T, path string) image.Image {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	img, format, err := image.Decode(f)
	if err != nil {
		t.Fatalf("decoding %s: %v", path, err)
	}
	if format != "png" {
		t.Fatalf("%s decoded as %q, want png", path, format)
	}
	return img
}

// TestTrayStateIconColours: the rendered PNGs are the tray's own artwork and
// not, say, an ARGB-vs-RGBA byte shuffle away from it. The quiet ring has to
// carry the reference blue #0353f4, and both muted glyphs opaque white.
func TestTrayStateIconColours(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache)
	icons, err := trayStateIconFiles()
	if err != nil {
		t.Fatal(err)
	}

	// near reports whether a colour is within tol of want on every channel; the
	// canvas antialiases, so an exact hit is only sure away from the edges.
	near := func(r, g, b uint32, want color.RGBA, tol int) bool {
		d := func(a uint32, w byte) int {
			v := int(a>>8) - int(w)
			if v < 0 {
				return -v
			}
			return v
		}
		return d(r, want.R) <= tol && d(g, want.G) <= tol && d(b, want.B) <= tol
	}

	for _, tc := range []struct {
		ic   Icon
		want color.RGBA
		name string
	}{
		{IconQuiet, trayBlue, "the ring blue #0353f4"},
		{IconMicMuted, trayWhite, "white"},
		{IconSpeakerMuted, trayWhite, "white"},
	} {
		img := decodePNG(t, icons[tc.ic])
		bounds := img.Bounds()
		hits, opaque := 0, 0
		for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
			for x := bounds.Min.X; x < bounds.Max.X; x++ {
				r, g, b, a := img.At(x, y).RGBA()
				if a < 0xf000 {
					continue // antialiased edge or transparent background
				}
				opaque++
				if near(r, g, b, tc.want, 2) {
					hits++
				}
			}
		}
		if opaque == 0 {
			t.Errorf("%v: every pixel is transparent", tc.ic)
			continue
		}
		if hits < 50 {
			t.Errorf("%v: only %d of %d opaque pixels are %s — check the ARGB byte order",
				tc.ic, hits, opaque, tc.name)
		}
	}

	// The two muted icons must not be the same picture: the mic and the speaker
	// say different things.
	mic, err := trayStatePNG(IconMicMuted)
	if err != nil {
		t.Fatal(err)
	}
	spk, err := trayStatePNG(IconSpeakerMuted)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(mic, spk) {
		t.Error("the mic and speaker state icons are byte-identical")
	}
}

// TestTrayNoticeIconPath: a mute notice is sent with its state PNG as the icon,
// everything else with the app icon, and a batch takes its newest notice's.
func TestTrayNoticeIconPath(t *testing.T) {
	tr, got := newNotifyTray(t, 30*time.Millisecond, time.Second)
	tr.iconPath = "/cache/ts6tray.svg"
	tr.stateIcons = map[Icon]string{
		IconMicMuted:     "/cache/state-mic-muted.png",
		IconSpeakerMuted: "/cache/state-speaker-muted.png",
		IconQuiet:        "/cache/state-quiet.png",
	}
	tr.notif[trayOptBatch] = false
	tr.notif[trayNotifyKindGroup[noticeMute]] = true

	tr.onNotice(notice{kind: noticeMute, title: "A muted their microphone", icon: IconMicMuted})
	tr.onNotice(notice{kind: noticeJoin, title: "B joined your channel"})
	calls := got()
	if len(calls) != 2 {
		t.Fatalf("got %d notifications, want 2: %+v", len(calls), calls)
	}
	if calls[0].icon != "/cache/state-mic-muted.png" {
		t.Errorf("mute notice icon = %q, want the mic PNG", calls[0].icon)
	}
	if calls[1].icon != "/cache/ts6tray.svg" {
		t.Errorf("join notice icon = %q, want the app icon", calls[1].icon)
	}

	// A batch: the newest notice is line 1 and supplies the picture.
	tr.notif[trayOptBatch] = true
	tr.onNotice(notice{kind: noticeMute, title: "A muted their microphone", icon: IconMicMuted})
	tr.onNotice(notice{kind: noticeMute, title: "A muted their speakers", icon: IconSpeakerMuted})
	tr.onNotice(notice{kind: noticeMute, title: "A unmuted everything", icon: IconQuiet})
	calls = waitCalls(t, got, 3)
	last := calls[len(calls)-1]
	if last.icon != "/cache/state-quiet.png" {
		t.Errorf("batch icon = %q, want the newest notice's quiet PNG", last.icon)
	}
	if !strings.HasPrefix(last.body, "1. A unmuted everything") {
		t.Errorf("batch body = %q, want the quiet notice on line 1", last.body)
	}

	// An icon we never rendered falls back to the app icon rather than "".
	tr.mu.Lock()
	tr.stateIcons = nil
	tr.mu.Unlock()
	tr.notif[trayOptBatch] = false
	tr.onNotice(notice{kind: noticeMute, title: "A muted their speakers", icon: IconSpeakerMuted})
	calls = got()
	if c := calls[len(calls)-1]; c.icon != "/cache/ts6tray.svg" {
		t.Errorf("unrendered state icon = %q, want the app icon", c.icon)
	}
}

// --- menu updates: properties versus layout --------------------------------

// emitCall is one captured D-Bus signal.
type emitCall struct {
	path dbus.ObjectPath
	name string
	args []any
}

// newEmitTray is a bus-free tray that looks exported, so refresh() actually
// emits, and records the signals instead of sending them.
func newEmitTray(t *testing.T) (*tray, func() []emitCall) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	tr := &tray{
		click: trayReadClick(trayConfigPath()),
		notif: trayReadNotify(trayConfigPath()),
	}
	var mu sync.Mutex
	var calls []emitCall
	tr.emitFn = func(path dbus.ObjectPath, name string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, emitCall{path, name, args})
	}
	tr.refresh() // build the rows before anything is watching
	tr.exported = true
	return tr, func() []emitCall {
		mu.Lock()
		defer mu.Unlock()
		return append([]emitCall(nil), calls...)
	}
}

// propsOfEmit pulls the a(ia{sv}) argument out of an ItemsPropertiesUpdated,
// checking the removed-properties array is there with the right type.
func propsOfEmit(t *testing.T, c emitCall) []trayMenuProps {
	t.Helper()
	if len(c.args) != 2 {
		t.Fatalf("%s has %d arguments, want 2", c.name, len(c.args))
	}
	upd, ok := c.args[0].([]trayMenuProps)
	if !ok {
		t.Fatalf("first argument is %T, want []trayMenuProps", c.args[0])
	}
	if _, ok := c.args[1].([]trayMenuRemovedProps); !ok {
		t.Fatalf("second argument is %T, want []trayMenuRemovedProps", c.args[1])
	}
	return upd
}

// TestTrayToggleEmitsPropertiesOnly: flipping a notification checkmark or a
// left-click radio changes nothing structural, so the host is told about the
// one property rather than asked to re-read the whole layout.
func TestTrayToggleEmitsPropertiesOnly(t *testing.T) {
	t.Run("notification checkmark", func(t *testing.T) {
		tr, got := newEmitTray(t)
		rev := tr.revision

		tr.toggleNotify(trayNotifyGroups[0].key)

		calls := got()
		if len(calls) != 1 {
			t.Fatalf("emitted %d signals, want 1: %+v", len(calls), calls)
		}
		if calls[0].name != trayMenuIface+".ItemsPropertiesUpdated" {
			t.Fatalf("emitted %s, want ItemsPropertiesUpdated", calls[0].name)
		}
		if calls[0].path != trayMenuPath {
			t.Errorf("path = %v, want %v", calls[0].path, trayMenuPath)
		}
		upd := propsOfEmit(t, calls[0])
		if len(upd) != 1 {
			t.Fatalf("updated %d items, want 1: %+v", len(upd), upd)
		}
		if upd[0].ID != trayIDNotify0 {
			t.Errorf("updated id %d, want %d", upd[0].ID, trayIDNotify0)
		}
		if len(upd[0].Props) != 1 {
			t.Errorf("sent %d properties, want only toggle-state: %v", len(upd[0].Props), upd[0].Props)
		}
		// The first group defaults to on, so switching it flips it to 0.
		if v, ok := upd[0].Props["toggle-state"]; !ok || v.Value() != int32(0) {
			t.Errorf("toggle-state = %v (present: %v), want 0", v.Value(), ok)
		}
		if tr.revision <= rev {
			t.Errorf("revision %d, want it bumped past %d", tr.revision, rev)
		}
	})

	t.Run("left-click radios", func(t *testing.T) {
		tr, got := newEmitTray(t)

		tr.setClick("speaker")

		calls := got()
		if len(calls) != 1 || calls[0].name != trayMenuIface+".ItemsPropertiesUpdated" {
			t.Fatalf("emitted %+v, want one ItemsPropertiesUpdated", calls)
		}
		// Both radios moved: one off, one on.
		upd := propsOfEmit(t, calls[0])
		want := map[int32]int32{trayIDClickMic: 0, trayIDClickSpeaker: 1}
		if len(upd) != len(want) {
			t.Fatalf("updated %d items, want %d: %+v", len(upd), len(want), upd)
		}
		for _, u := range upd {
			w, ok := want[u.ID]
			if !ok {
				t.Errorf("unexpected item %d in the update", u.ID)
				continue
			}
			if v := u.Props["toggle-state"].Value(); v != w {
				t.Errorf("item %d toggle-state = %v, want %v", u.ID, v, w)
			}
		}
	})
}

// TestTrayStructuralChangeEmitsLayoutUpdated: anything that adds, removes or
// relabels a row still has to be a LayoutUpdated, because a host cannot learn
// about it from toggle-state alone.
func TestTrayStructuralChangeEmitsLayoutUpdated(t *testing.T) {
	t.Run("the bind countdown relabels and disables a row", func(t *testing.T) {
		tr, got := newEmitTray(t)
		tr.mu.Lock()
		tr.binding = "mic"
		tr.mu.Unlock()

		tr.refresh()

		calls := got()
		if len(calls) != 1 || calls[0].name != trayMenuIface+".LayoutUpdated" {
			t.Fatalf("emitted %+v, want one LayoutUpdated", calls)
		}
		if len(calls[0].args) != 2 || calls[0].args[1] != trayIDRoot {
			t.Errorf("args = %v, want (revision, %d)", calls[0].args, trayIDRoot)
		}
		if rev, ok := calls[0].args[0].(uint32); !ok || rev != tr.revision {
			t.Errorf("revision argument = %v, want %d", calls[0].args[0], tr.revision)
		}
	})

	t.Run("a server row appearing", func(t *testing.T) {
		tr, got := newEmitTray(t)
		tr.mu.Lock()
		tr.conns = []Conn{{ID: 1, ServerName: "Home", InputHardware: true}}
		tr.mu.Unlock()

		tr.refresh()

		calls := got()
		if len(calls) != 1 || calls[0].name != trayMenuIface+".LayoutUpdated" {
			t.Fatalf("emitted %+v, want one LayoutUpdated", calls)
		}
	})

	t.Run("nothing changed: nothing is emitted", func(t *testing.T) {
		tr, got := newEmitTray(t)
		tr.refresh()
		if calls := got(); len(calls) != 0 {
			t.Fatalf("emitted %+v on an unchanged menu, want nothing", calls)
		}
	})
}

// TestTrayToggleOnlyDiff covers the decision itself, away from the plumbing.
func TestTrayToggleOnlyDiff(t *testing.T) {
	base := []trayRow{
		{id: 24, parent: trayIDSettings, label: "Microphone", enabled: true, radio: true, checked: true},
		{id: 25, parent: trayIDSettings, label: "Speaker", enabled: true, radio: true},
	}
	clone := func() []trayRow { return append([]trayRow(nil), base...) }

	if _, ok := trayToggleOnlyDiff(base, clone()); ok {
		t.Error("an identical pair reports a toggle diff, want none")
	}

	flipped := clone()
	flipped[0].checked, flipped[1].checked = false, true
	upd, ok := trayToggleOnlyDiff(base, flipped)
	if !ok || len(upd) != 2 {
		t.Fatalf("flipped radios: ok=%v upd=%+v, want two updates", ok, upd)
	}

	relabelled := clone()
	relabelled[0].label = "Mic"
	if _, ok := trayToggleOnlyDiff(base, relabelled); ok {
		t.Error("a relabelled row reports a toggle-only diff")
	}

	disabled := clone()
	disabled[0].checked, disabled[0].enabled = false, false
	if _, ok := trayToggleOnlyDiff(base, disabled); ok {
		t.Error("a row that also changed enabled reports a toggle-only diff")
	}

	if _, ok := trayToggleOnlyDiff(base, base[:1]); ok {
		t.Error("a removed row reports a toggle-only diff")
	}
	if _, ok := trayToggleOnlyDiff(nil, base); ok {
		t.Error("building the rows from nothing reports a toggle-only diff")
	}
}
