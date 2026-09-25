package main

// tray_test.go deliberately never touches the session bus: connecting would
// claim a name and make a real icon appear in the user's panel. Everything here
// is pure Go, including the reproduction of what godbus/prop does to a property
// value, which needs no connection at all.

import (
	"bytes"
	"encoding/xml"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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

// TestTrayMenuIsMinimal pins the menu the user asked for: the server rows, a
// separator, the two toggles, a separator and Quit. Nothing else — every
// setting moved to `ts6tray settings`, so no id above Quit may come back.
func TestTrayMenuIsMinimal(t *testing.T) {
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
	want := []int32{trayIDServer0, trayIDSep1, trayIDMic, trayIDSpeaker, trayIDSep2, trayIDQuit}
	if !reflect.DeepEqual(ids, want) {
		t.Fatalf("root children = %v, want %v", ids, want)
	}
	for _, n := range top {
		if len(n.Children) != 0 {
			t.Errorf("item %d has %d children; the menu is supposed to be flat", n.ID, len(n.Children))
		}
		if _, ok := n.Props["children-display"]; ok {
			t.Errorf("item %d still claims a submenu", n.ID)
		}
		if _, ok := n.Props["toggle-type"]; ok {
			t.Errorf("item %d still has a toggle-type; the radios and checkmarks are gone", n.ID)
		}
	}
	// Nothing with a Settings-era id is reachable any more.
	for id := int32(15); id < trayIDServer0; id++ {
		if _, ok := tr.rowByID(id); ok {
			t.Errorf("row %d still exists; ids 15..99 belonged to the Settings submenu", id)
		}
	}
	if got := propOf(t, top[5], "label"); got != "Quit" {
		t.Errorf("last item label = %q, want Quit", got)
	}
	if got := propOf(t, top[2], "label"); got != "Toggle microphone mute" {
		t.Errorf("item 3 label = %q", got)
	}
	if got := propOf(t, top[3], "label"); got != "Toggle speaker mute" {
		t.Errorf("item 4 label = %q", got)
	}

	// Depth 1 is the whole menu now, because nothing nests.
	_, shallow, derr := tr.GetLayout(trayIDRoot, 1, nil)
	if derr != nil {
		t.Fatalf("GetLayout depth 1: %v", derr)
	}
	if len(nodeChildren(shallow)) != len(want) {
		t.Errorf("depth 1 returned %d children, want %d", len(nodeChildren(shallow)), len(want))
	}
}

// TestTrayQuitEvent: clicking Quit still calls the quit function.
func TestTrayQuitEvent(t *testing.T) {
	tr := newTestTray(t)
	quit := make(chan struct{})
	tr.quit = func() { close(quit) }

	tr.Event(trayIDQuit, "clicked", dbus.Variant{}, 0)

	select {
	case <-quit:
	case <-time.After(2 * time.Second):
		t.Fatal("clicking Quit did not quit")
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

	// The TUI writes the file and the daemon re-reads it; that is the only way
	// the target changes now.
	if err := trayWriteClick(trayConfigPath(), "speaker"); err != nil {
		t.Fatal(err)
	}
	if err := tr.reloadConfig(); err != nil {
		t.Fatalf("reloadConfig: %v", err)
	}
	if got := tr.clickTarget(); got != "speaker" {
		t.Fatalf("clickTarget = %q, want speaker", got)
	}
	tr.Activate(0, 0)
	if v := next("Activate with speaker selected"); v != "speaker" {
		t.Errorf("Activate toggled %q, want speaker", v)
	}
	tr.SecondaryActivate(0, 0)
	if v := next("SecondaryActivate with speaker selected"); v != "mic" {
		t.Errorf("SecondaryActivate toggled %q, want mic", v)
	}
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
	for _, k := range []noticeKind{noticeJoin, noticeLeave, noticeMoved, noticeKicked, noticeConnLost, noticeMute} {
		if !tr.notifyEnabled(k) {
			t.Errorf("%v is off by default, want on", k)
		}
	}
	// TeamSpeak pops up its own notification for messages and pokes, so ours
	// would only double them.
	for _, k := range []noticeKind{noticeChannelMsg, noticePrivateMsg, noticePoke} {
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
	tr := &tray{
		notif:   trayReadNotify(trayConfigPath()),
		timeout: trayReadTimeout(trayConfigPath()),
	}

	var mu sync.Mutex
	var calls []notifyCall
	var next uint32
	tr.notifyFn = func(replaces uint32, title, body, icon string) (uint32, error) {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, notifyCall{replaces, title, body, icon})
		next++
		if replaces != 0 {
			return replaces, nil // a server keeps the id it was told to replace
		}
		return next, nil
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
	tr.notif[trayOptBatch] = true
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
	tr.notif[trayOptBatch] = true
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
	tr.notif[trayOptBatch] = true
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

	if got := trayReadNotify(path); got[trayOptBatch] || !got[trayOptReplace] {
		t.Errorf("defaults: batch=%v replace=%v, want batch off and replace on", got[trayOptBatch], got[trayOptReplace])
	}

	m := trayNotifyDefaults()
	m[trayOptBatch] = true
	if err := trayWriteNotify(path, m); err != nil {
		t.Fatal(err)
	}
	raw := readFile(t, path)
	if !strings.Contains(raw, "notify.batch=on") || !strings.Contains(raw, "notify.replace=on") {
		t.Errorf("config file:\n%s", raw)
	}
	got := trayReadNotify(path)
	if !got[trayOptBatch] || !got[trayOptReplace] {
		t.Errorf("read back: batch=%v replace=%v", got[trayOptBatch], got[trayOptReplace])
	}

	// notifyOptOn reads whatever is in the state, switches and defaults alike.
	m[trayOptBatch] = false
	m[trayOptReplace] = false
	tr := &tray{notif: m}
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

// --- the notification icons ------------------------------------------------

// TestTrayNotifyIconFiles: the artwork lands in XDG_CACHE_HOME/ts6tray/notify,
// byte-identical to what is embedded, and an unchanged file is left alone
// rather than rewritten — the same contract trayIconFile has.
func TestTrayNotifyIconFiles(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache)

	icons, err := trayNotifyIconFiles()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(cache, "ts6tray", "notify")
	want := map[notifyIcon]string{
		iconMicMuted:     filepath.Join(dir, "mic-muted.svg"),
		iconSpeakerMuted: filepath.Join(dir, "speaker-muted.svg"),
		iconUnmuted:      filepath.Join(dir, "unmuted.svg"),
		iconJoin:         filepath.Join(dir, "join.svg"),
		iconLeave:        filepath.Join(dir, "leave.svg"),
	}
	if !reflect.DeepEqual(icons, want) {
		t.Fatalf("paths = %v, want %v", icons, want)
	}
	// The app icon is the sentinel for "our own icon" and has no file here.
	if p, ok := icons[iconApp]; ok {
		t.Errorf("iconApp has a notification icon at %q, want none", p)
	}

	mods := map[notifyIcon]time.Time{}
	for ic, p := range icons {
		fi, serr := os.Stat(p)
		if serr != nil {
			t.Fatal(serr)
		}
		mods[ic] = fi.ModTime()
		if fi.Mode().Perm() != 0o644 {
			t.Errorf("%v: mode %v, want 0644", ic, fi.Mode().Perm())
		}
		on, rerr := os.ReadFile(p)
		if rerr != nil {
			t.Fatal(rerr)
		}
		embedded, eerr := trayNotifySVGs.ReadFile("assets/notify/" + string(ic) + ".svg")
		if eerr != nil {
			t.Fatal(eerr)
		}
		if !bytes.Equal(on, embedded) {
			t.Errorf("%v: the written file is not the embedded artwork", ic)
		}
	}

	// Written once: a second call must not touch the files.
	time.Sleep(10 * time.Millisecond)
	again, err := trayNotifyIconFiles()
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

	// Stale content: rewritten.
	stale := icons[iconJoin]
	if werr := os.WriteFile(stale, []byte("<svg/>"), 0o644); werr != nil {
		t.Fatal(werr)
	}
	if _, err = trayNotifyIconFiles(); err != nil {
		t.Fatal(err)
	}
	embedded, _ := trayNotifySVGs.ReadFile("assets/notify/join.svg")
	if b, _ := os.ReadFile(stale); !bytes.Equal(b, embedded) {
		t.Error("a stale notification icon was not replaced")
	}
}

// TestTrayNotifySVGsParse: every embedded icon is well-formed XML and a picture
// of its own, and assets/notify/ holds nothing trayNotifyIconNames does not
// name — a file added there without a notifyIcon would never be unpacked.
func TestTrayNotifySVGsParse(t *testing.T) {
	entries, err := trayNotifySVGs.ReadDir("assets/notify")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(trayNotifyIconNames) {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("assets/notify holds %v, but only %v are mapped to an icon",
			names, trayNotifyIconNames)
	}
	seen := map[string]notifyIcon{}
	for _, ic := range trayNotifyIconNames {
		data, rerr := trayNotifySVGs.ReadFile("assets/notify/" + string(ic) + ".svg")
		if rerr != nil {
			t.Fatalf("%v: %v", ic, rerr)
		}
		dec := xml.NewDecoder(bytes.NewReader(data))
		for {
			_, terr := dec.Token()
			if terr == io.EOF {
				break
			}
			if terr != nil {
				t.Errorf("%v: not well-formed XML: %v", ic, terr)
				break
			}
		}
		if other, dup := seen[string(data)]; dup {
			t.Errorf("%v and %v are the same picture", ic, other)
		}
		seen[string(data)] = ic
	}
}

// TestTrayNoticeIconPath: a mute notice is sent with its state PNG as the icon,
// everything else with the app icon, and a batch takes its newest notice's.
func TestTrayNoticeIconPath(t *testing.T) {
	tr, got := newNotifyTray(t, 30*time.Millisecond, time.Second)
	tr.iconPath = "/cache/ts6tray.svg"
	tr.notifyIcons = map[notifyIcon]string{
		iconMicMuted:     "/cache/notify/mic-muted.svg",
		iconSpeakerMuted: "/cache/notify/speaker-muted.svg",
		iconJoin:         "/cache/notify/join.svg",
		iconLeave:        "/cache/notify/leave.svg",
		iconUnmuted:      "/cache/notify/unmuted.svg",
	}
	tr.notif[trayOptBatch] = false
	tr.notif[trayNotifyKindGroup[noticeMute]] = true

	tr.onNotice(notice{kind: noticeMute, title: "A muted their microphone", icon: iconMicMuted})
	tr.onNotice(notice{kind: noticeJoin, title: "B joined your channel"})
	calls := got()
	if len(calls) != 2 {
		t.Fatalf("got %d notifications, want 2: %+v", len(calls), calls)
	}
	if calls[0].icon != "/cache/notify/mic-muted.svg" {
		t.Errorf("mute notice icon = %q, want the mic PNG", calls[0].icon)
	}
	if calls[1].icon != "/cache/ts6tray.svg" {
		t.Errorf("join notice icon = %q, want the app icon", calls[1].icon)
	}

	// A batch: the newest notice is line 1 and supplies the picture.
	tr.notif[trayOptBatch] = true
	tr.onNotice(notice{kind: noticeMute, title: "A muted their microphone", icon: iconMicMuted})
	tr.onNotice(notice{kind: noticeMute, title: "A muted their speakers", icon: iconSpeakerMuted})
	tr.onNotice(notice{kind: noticeMute, title: "A unmuted everything", icon: iconUnmuted})
	calls = waitCalls(t, got, 3)
	last := calls[len(calls)-1]
	if last.icon != "/cache/notify/unmuted.svg" {
		t.Errorf("batch icon = %q, want the newest notice's quiet PNG", last.icon)
	}
	if !strings.HasPrefix(last.body, "1. A unmuted everything") {
		t.Errorf("batch body = %q, want the quiet notice on line 1", last.body)
	}

	// An icon we never rendered falls back to the app icon rather than "".
	tr.mu.Lock()
	tr.notifyIcons = nil
	tr.mu.Unlock()
	tr.notif[trayOptBatch] = false
	tr.onNotice(notice{kind: noticeMute, title: "A muted their speakers", icon: iconSpeakerMuted})
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

// TestTrayMenuChangeEmitsLayoutUpdated: the menu only ever changes shape now,
// so every change is a LayoutUpdated carrying the bumped revision.
func TestTrayMenuChangeEmitsLayoutUpdated(t *testing.T) {
	t.Run("the toggles becoming usable", func(t *testing.T) {
		tr, got := newEmitTray(t)
		tr.mu.Lock()
		tr.conns = []Conn{{ID: 1, ServerName: "Home", InputHardware: true}}
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

// --- "Silence while my speakers are muted" ---------------------------------

// deafTray is a notification tray whose speaker-mute state the test controls.
func deafTray(t *testing.T, deaf *atomic.Bool) (*tray, func() []notifyCall) {
	t.Helper()
	tr, got := newNotifyTray(t, time.Hour, time.Hour)
	tr.snapshot = func() ([]Conn, Icon, bool) {
		return []Conn{{ID: 1, Status: StatusConnectionEstablished, OutputMuted: deaf.Load()}}, IconQuiet, true
	}
	tr.notif[trayOptBatch] = false // deliver each notice as it arrives
	return tr, got
}

// TestTrayQuietWhenDeaf: with the option on and our speakers muted, event
// notices are dropped — except a lost connection, which still gets through.
func TestTrayQuietWhenDeaf(t *testing.T) {
	var deaf atomic.Bool
	tr, got := deafTray(t, &deaf)
	tr.notif[trayOptQuietWhenDeaf] = true

	deaf.Store(true)
	tr.onNotice(notice{kind: noticeJoin, title: "A joined your channel"})
	tr.onNotice(notice{kind: noticeKicked, title: "B was kicked from the channel"})
	if c := got(); len(c) != 0 {
		t.Errorf("notified while deaf: %v", c)
	}
	tr.onNotice(notice{kind: noticeConnLost, title: "Lost connection to X"})
	c := got()
	if len(c) != 1 || c[0].title != "Lost connection to X" {
		t.Fatalf("connLost did not get through: %v", c)
	}

	// Speakers back on: everything passes again.
	deaf.Store(false)
	tr.onNotice(notice{kind: noticeJoin, title: "A joined your channel"})
	if c := got(); len(c) != 2 || c[1].title != "A joined your channel" {
		t.Errorf("after unmuting: %v", c)
	}
}

// TestTrayQuietWhenDeafPassesOurOwnActions: silencing the world while our
// speakers are muted must not silence *us*. Muting the speakers is itself a
// "self" notice, and the moment the user flips that switch is exactly when the
// confirmation is wanted — so every self notice gets through, while other
// people's news stays held back.
func TestTrayQuietWhenDeafPassesOurOwnActions(t *testing.T) {
	var deaf atomic.Bool
	tr, got := deafTray(t, &deaf)
	tr.notif[trayOptQuietWhenDeaf] = true
	tr.notif[trayNotifyKindGroup[noticeSelf]] = true
	tr.notif[trayNotifyKindGroup[noticeMute]] = true

	deaf.Store(true)
	tr.onNotice(notice{kind: noticeSelf, title: "You muted your speakers", icon: iconSpeakerMuted})
	tr.onNotice(notice{kind: noticeSelf, title: "You were kicked from the channel", icon: iconLeave})
	c := got()
	if len(c) != 2 || c[0].title != "You muted your speakers" ||
		c[1].title != "You were kicked from the channel" {
		t.Fatalf("our own notices were silenced: %v", c)
	}

	// Everybody else is still held back.
	tr.onNotice(notice{kind: noticeMute, title: "A muted their speakers", icon: iconSpeakerMuted})
	tr.onNotice(notice{kind: noticeJoin, title: "A joined your channel", icon: iconJoin})
	if c := got(); len(c) != 2 {
		t.Errorf("someone else's notice got through while deaf: %v", c[2:])
	}
}

// TestTrayQuietWhenDeafOffPassesEverything: with the option off a muted
// speaker on its own changes nothing.
func TestTrayQuietWhenDeafOffPassesEverything(t *testing.T) {
	var deaf atomic.Bool
	deaf.Store(true)
	tr, got := deafTray(t, &deaf)
	tr.notif[trayOptQuietWhenDeaf] = false
	tr.onNotice(notice{kind: noticeJoin, title: "A joined your channel"})
	if c := got(); len(c) != 1 {
		t.Errorf("calls = %v, want the notice through", c)
	}
}

// TestTrayQuietWhenDeafDropsRatherThanQueues: a silenced notice must not sit in
// the batcher waiting to be flushed the moment the speakers come back.
func TestTrayQuietWhenDeafDropsRatherThanQueues(t *testing.T) {
	var deaf atomic.Bool
	deaf.Store(true)
	tr, got := deafTray(t, &deaf)
	tr.notif[trayOptBatch] = true
	tr.notif[trayOptQuietWhenDeaf] = true

	tr.onNotice(notice{kind: noticeJoin, title: "A joined your channel"})
	deaf.Store(false)
	tr.batch.flush()
	if c := got(); len(c) != 0 {
		t.Errorf("the dropped notice was queued after all: %v", c)
	}
}

// TestTrayReloadConfigPicksUpAnotherProcessesWrite: `ts6tray settings` writes
// the file and sends `reload`; the menu has to follow.
func TestTrayReloadConfigPicksUpAnotherProcessesWrite(t *testing.T) {
	tr := newTestTray(t)
	tr.notif = trayReadNotify(trayConfigPath())
	if tr.clickTarget() != "mic" || tr.notif["self"] || !tr.notif["mute"] {
		t.Fatal("unexpected starting point")
	}

	// Another process (the TUI) changes the file underneath us.
	path := trayConfigPath()
	if err := trayWriteClick(path, "speaker"); err != nil {
		t.Fatal(err)
	}
	n := trayReadNotify(path)
	n["mute"] = false
	n["self"] = true
	if err := trayWriteNotify(path, n); err != nil {
		t.Fatal(err)
	}

	before := tr.revision
	if err := tr.reloadConfig(); err != nil {
		t.Fatalf("reloadConfig: %v", err)
	}
	if tr.clickTarget() != "speaker" {
		t.Errorf("click = %q, want speaker", tr.clickTarget())
	}
	if tr.notifyEnabled(noticeMute) || !tr.notifyEnabled(noticeSelf) {
		t.Error("the reloaded switches did not take")
	}
	// The menu shows none of this any more, so it must not churn either.
	if tr.revision != before {
		t.Errorf("revision %d, want it left at %d: no setting is in the menu", tr.revision, before)
	}
	// …and the display time comes along with the rest.
	if err := trayWriteTimeout(path, "30"); err != nil {
		t.Fatal(err)
	}
	if err := tr.reloadConfig(); err != nil {
		t.Fatalf("reloadConfig: %v", err)
	}
	if got := tr.notifyTimeout(); got != 30000 {
		t.Errorf("notifyTimeout = %d, want 30000", got)
	}
}

// TestConfigNewKeysRoundTrip: the three switches added after the first release
// survive a write/read cycle under their documented keys, and a config file
// written before they existed still loads with them at their defaults.
func TestConfigNewKeysRoundTrip(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	path := trayConfigPath()

	// A file from the previous version: every old key present, none of the new.
	old := "click=speaker\nnotify.batch=off\nnotify.channelMsg=on\nnotify.connLost=off\n" +
		"notify.joinleave=on\nnotify.kicked=on\nnotify.moved=on\nnotify.mute=on\n" +
		"notify.poke=on\nnotify.privateMsg=on\nnotify.replace=off\n"
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	got := trayReadNotify(path)
	for _, key := range []string{"self", "serverMsg"} {
		if got[key] {
			t.Errorf("%s came out of an old config as on, want off", key)
		}
	}
	if !got[trayOptQuietWhenDeaf] {
		t.Errorf("%s came out of an old config as off, want on", trayOptQuietWhenDeaf)
	}
	if !got["mute"] || got[trayOptBatch] || got["connLost"] {
		t.Errorf("the old keys did not survive: %v", got)
	}

	// Flip the new ones away from their defaults and read them back.
	got["self"] = true
	got["serverMsg"] = true
	got[trayOptQuietWhenDeaf] = false
	if err := trayWriteNotify(path, got); err != nil {
		t.Fatal(err)
	}
	body := readFile(t, path)
	for _, want := range []string{"notify.self=on", "notify.serverMsg=on", "notify.quietWhenDeaf=off"} {
		if !strings.Contains(body, want+"\n") {
			t.Errorf("the file is missing %q:\n%s", want, body)
		}
	}
	if again := trayReadNotify(path); !reflect.DeepEqual(again, got) {
		t.Errorf("round trip: wrote %v, read %v", got, again)
	}
	if trayReadClick(path) != "speaker" {
		t.Error("the click target was lost")
	}

	// Every switch the UI shows has a key in the file, and no two share one.
	seen := map[string]bool{}
	for _, g := range trayNotifySwitches {
		if seen[g.key] {
			t.Errorf("duplicate config key %q", g.key)
		}
		seen[g.key] = true
		if !strings.Contains(body, "notify."+g.key+"=") {
			t.Errorf("%s was not written", g.key)
		}
	}
}

// --- the display time and the replace window --------------------------------

// TestTrayNotifyTimeout: the config value becomes Notify's expire_timeout. The
// spec's -1 is "server decides" and 0 is "never expires"; everything else is
// milliseconds, and an unset or nonsense file means three seconds.
func TestTrayNotifyTimeout(t *testing.T) {
	for _, c := range []struct {
		value string
		want  int32
	}{
		{"default", -1},
		{"never", 0},
		{"3", 3000},
		{"5", 5000},
		{"10", 10000},
		{"30", 30000},
		{"", 3000},
		{"trumpet", 3000},
	} {
		if got := trayTimeoutMillis(c.value); got != c.want {
			t.Errorf("trayTimeoutMillis(%q) = %d, want %d", c.value, got, c.want)
		}
	}
	// Only "never" may be sticky; every other value has to expire.
	for _, v := range trayNotifyTimeouts {
		ms := trayTimeoutMillis(v)
		if v == "never" && ms != 0 {
			t.Errorf("never = %d, want 0", ms)
		}
		if v != "never" && v != "default" && ms <= 0 {
			t.Errorf("%s = %d, want a positive timeout", v, ms)
		}
	}

	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	path := trayConfigPath()
	if got := trayReadTimeout(path); got != trayNotifyTimeoutDef {
		t.Errorf("a config with no notify.timeout reads %q, want %q", got, trayNotifyTimeoutDef)
	}
	if err := trayWriteTimeout(path, "never"); err != nil {
		t.Fatal(err)
	}
	if got := trayReadTimeout(path); got != "never" {
		t.Errorf("round trip: read %q, want never", got)
	}
	if !strings.Contains(readFile(t, path), "notify.timeout=never\n") {
		t.Errorf("the file is missing notify.timeout:\n%s", readFile(t, path))
	}
	// Writing the switches must not disturb it, and vice versa.
	if err := trayWriteNotify(path, trayNotifyDefaults()); err != nil {
		t.Fatal(err)
	}
	if got := trayReadTimeout(path); got != "never" {
		t.Errorf("trayWriteNotify trampled notify.timeout: %q", got)
	}

	// The cycle the TUI walks, in order and wrapping.
	v := trayNotifyTimeouts[0]
	var seen []string
	for range trayNotifyTimeouts {
		seen = append(seen, v)
		v = trayNextTimeout(v, 1)
	}
	if !reflect.DeepEqual(seen, trayNotifyTimeouts) || v != trayNotifyTimeouts[0] {
		t.Errorf("cycle = %v (then %q), want %v wrapping", seen, v, trayNotifyTimeouts)
	}
	if got := trayNextTimeout("trumpet", 1); got != trayNotifyTimeoutDef {
		t.Errorf("an unknown value cycles to %q, want the default", got)
	}

	// And backwards, wrapping the other way.
	for _, c := range []struct{ from, want string }{
		{"default", "never"},
		{"3", "default"},
		{"never", "30"},
	} {
		if got := trayNextTimeout(c.from, -1); got != c.want {
			t.Errorf("back from %q = %q, want %q", c.from, got, c.want)
		}
	}
	if got := trayNextTimeout("never", 1); got != "default" {
		t.Errorf("forward from \"never\" = %q, want the default value", got)
	}
}

// TestTrayNotifyAppName: the notifications say they come from TeamSpeak, which
// is what the user sees as the source.
func TestTrayNotifyAppName(t *testing.T) {
	if trayAppName != "TeamSpeak" {
		t.Errorf("app_name = %q, want TeamSpeak", trayAppName)
	}
}

// TestTrayReplaceWindowStops: a notification the server can no longer be
// showing must not be "replaced" — some servers apply that in place, silently
// editing something nobody can see. Past the display time we send a fresh one.
func TestTrayReplaceWindowStops(t *testing.T) {
	tr, got := newNotifyTray(t, time.Hour, time.Hour)
	tr.notif[trayOptBatch] = false
	tr.timeout = "5"
	now := time.Now()
	tr.now = func() time.Time { return now }

	tr.onNotice(notice{kind: noticeJoin, title: "A joined your channel"})
	now = now.Add(2 * time.Second) // still inside the five seconds
	tr.onNotice(notice{kind: noticeJoin, title: "B joined your channel"})
	now = now.Add(9 * time.Second) // long gone
	tr.onNotice(notice{kind: noticeJoin, title: "C joined your channel"})

	calls := got()
	if len(calls) != 3 {
		t.Fatalf("%d notifications, want 3", len(calls))
	}
	if calls[1].replaces != 1 {
		t.Errorf("inside the window: replaces_id = %d, want 1", calls[1].replaces)
	}
	if calls[2].replaces != 0 {
		t.Errorf("past the window: replaces_id = %d, want a fresh notification (0)", calls[2].replaces)
	}

	// "never" means it really does stay up, so replacing stays right forever.
	if w := trayReplaceWindow("never"); w != 0 {
		t.Errorf("trayReplaceWindow(never) = %v, want no limit", w)
	}
	if w := trayReplaceWindow("default"); w != trayDefaultExpiry {
		t.Errorf("trayReplaceWindow(default) = %v, want %v", w, trayDefaultExpiry)
	}
	if w := trayReplaceWindow("30"); w != 30*time.Second {
		t.Errorf("trayReplaceWindow(30) = %v, want 30s", w)
	}
}

// TestTrayReplaceWindowRunsFromTheFirstShow: a replace rides on the popup
// already up and does not restart its expiry, so a steady stream of events
// must not keep the window alive for ever. It is measured from the first show.
func TestTrayReplaceWindowRunsFromTheFirstShow(t *testing.T) {
	tr, got := newNotifyTray(t, time.Hour, time.Hour)
	tr.notif[trayOptBatch] = false
	tr.timeout = "5"
	now := time.Now()
	tr.now = func() time.Time { return now }

	tr.onNotice(notice{kind: noticeJoin, title: "A joined your channel"})
	now = now.Add(3 * time.Second) // 0.6 of the window: the popup is still up
	tr.onNotice(notice{kind: noticeJoin, title: "B joined your channel"})
	now = now.Add(3 * time.Second) // 1.2 windows after the first show: gone
	tr.onNotice(notice{kind: noticeJoin, title: "C joined your channel"})

	calls := got()
	if len(calls) != 3 {
		t.Fatalf("%d notifications, want 3", len(calls))
	}
	if calls[1].replaces != 1 {
		t.Errorf("inside the window: replaces_id = %d, want the first notification's id 1", calls[1].replaces)
	}
	if calls[2].replaces != 0 {
		t.Errorf("a replace extended the window: replaces_id = %d, want a fresh notification (0)", calls[2].replaces)
	}
}

// TestTrayNotificationClosedResetsReplace: once the server says our
// notification is gone, the next event opens a new one.
func TestTrayNotificationClosedResetsReplace(t *testing.T) {
	tr, got := newNotifyTray(t, time.Hour, time.Hour)
	tr.notif[trayOptBatch] = false

	tr.onNotice(notice{kind: noticeJoin, title: "A joined your channel"})
	tr.mu.RLock()
	id := tr.lastEventID
	tr.mu.RUnlock()
	if id == 0 {
		t.Fatal("no id was recorded")
	}

	tr.onNotificationClosed(id + 99) // somebody else's notification
	tr.onNotice(notice{kind: noticeJoin, title: "B joined your channel"})
	if c := got(); c[1].replaces != id {
		t.Errorf("an unrelated close reset our id: replaces_id = %d, want %d", c[1].replaces, id)
	}

	tr.mu.RLock()
	id = tr.lastEventID
	tr.mu.RUnlock()
	tr.onNotificationClosed(id)
	tr.onNotice(notice{kind: noticeJoin, title: "C joined your channel"})
	if c := got(); c[2].replaces != 0 {
		t.Errorf("after NotificationClosed: replaces_id = %d, want 0", c[2].replaces)
	}
}

// TestTrayNotifyRetriesAfterAnError: if a replacing Notify fails, the id it
// tried to replace is dropped and the notice is sent once more as a new one,
// so the news still reaches the screen.
func TestTrayNotifyRetriesAfterAnError(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	tr := &tray{
		notif:   trayReadNotify(trayConfigPath()),
		timeout: trayReadTimeout(trayConfigPath()),
	}
	tr.notif[trayOptBatch] = false
	tr.lastEventID, tr.lastEventAt = 7, time.Now()

	var mu sync.Mutex
	var seen []uint32
	tr.notifyFn = func(replaces uint32, title, body, icon string) (uint32, error) {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, replaces)
		if replaces != 0 {
			return 0, errors.New("org.freedesktop.DBus.Error.ServiceUnknown")
		}
		return 12, nil
	}

	tr.onNotice(notice{kind: noticeJoin, title: "A joined your channel"})

	mu.Lock()
	defer mu.Unlock()
	if !reflect.DeepEqual(seen, []uint32{7, 0}) {
		t.Fatalf("Notify replaces_id sequence = %v, want the retry [7 0]", seen)
	}
	if tr.lastEventID != 12 {
		t.Errorf("lastEventID = %d, want the retry's id 12", tr.lastEventID)
	}
}
