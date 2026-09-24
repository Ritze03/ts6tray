package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// --- fake TeamSpeak -------------------------------------------------------

// fakeTS is an httptest server speaking the TeamSpeak Remote Apps protocol.
// Each accepted connection runs handler in its own goroutine; handler drives
// the conversation and everything the client sent is recorded.
type fakeTS struct {
	t    *testing.T
	srv  *httptest.Server
	addr string

	mu    sync.Mutex
	sent  []map[string]any
	conns int32
}

type fakeSession struct {
	t   *testing.T
	f   *fakeTS
	c   *websocket.Conn
	ctx context.Context
	n   int32 // 1 for the first connection, 2 for the second, ...
}

func newFakeTS(t *testing.T, handler func(s *fakeSession)) *fakeTS {
	t.Helper()
	f := &fakeTS{t: t}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		c.SetReadLimit(1 << 20)
		n := atomic.AddInt32(&f.conns, 1)
		handler(&fakeSession{t: t, f: f, c: c, ctx: r.Context(), n: n})
	}))
	t.Cleanup(f.srv.Close)
	f.addr = strings.TrimPrefix(f.srv.URL, "http://")
	return f
}

// read returns the next message from the client, recording it.
func (s *fakeSession) read() map[string]any {
	s.t.Helper()
	_, data, err := s.c.Read(s.ctx)
	if err != nil {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		s.t.Errorf("fake server: bad JSON from client: %v (%s)", err, data)
		return nil
	}
	s.f.mu.Lock()
	s.f.sent = append(s.f.sent, m)
	s.f.mu.Unlock()
	return m
}

func (s *fakeSession) write(v any) {
	s.t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		s.t.Errorf("fake server: marshal: %v", err)
		return
	}
	_ = s.c.Write(s.ctx, websocket.MessageText, b)
}

func (s *fakeSession) writeRaw(b []byte) {
	_ = s.c.Write(s.ctx, websocket.MessageText, b)
}

// messages returns a copy of everything the client has sent so far.
func (f *fakeTS) messages() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]map[string]any, len(f.sent))
	copy(out, f.sent)
	return out
}

func (f *fakeTS) countType(typ string) int {
	n := 0
	for _, m := range f.messages() {
		if m["type"] == typ {
			n++
		}
	}
	return n
}

// authReply loads testdata/auth_full.json (never modified on disk), sets the
// api key it hands back and optionally patches our own client's properties on
// connection 1.
func authReply(t *testing.T, key string, patch map[string]any) []byte {
	t.Helper()
	raw := fixture(t, "auth_full.json")
	var env map[string]any
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("parsing fixture: %v", err)
	}
	payload := env["payload"].(map[string]any)
	payload["apiKey"] = key
	if len(patch) > 0 {
		conns := payload["connections"].([]any)
		conn := conns[0].(map[string]any)
		clientID := conn["clientId"].(float64)
		for _, ci := range conn["clientInfos"].([]any) {
			info := ci.(map[string]any)
			if info["id"].(float64) != clientID {
				continue
			}
			props := info["properties"].(map[string]any)
			for k, v := range patch {
				props[k] = v
			}
		}
	}
	out, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("re-marshal fixture: %v", err)
	}
	return out
}

// propsUpdate is a clientPropertiesUpdated for conn 1 / client 18, the identity
// our fixture uses (docs/protocol.md, "the authoritative state feed").
func propsUpdate(props map[string]any) map[string]any {
	return map[string]any{
		"type": "clientPropertiesUpdated",
		"payload": map[string]any{
			"connectionId": 1,
			"clientId":     18,
			"properties":   props,
		},
	}
}

func buttonAck(button string, state bool) map[string]any {
	return map[string]any{
		"type":       "buttonPress",
		"payload":    map[string]any{"button": button, "state": state},
		"returnCode": "",
		"status":     map[string]any{"code": 0, "message": "ok"},
	}
}

// startClient runs c against f until the test ends.
func startClient(t *testing.T, c *TSClient) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); c.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("Run did not return after cancel")
		}
	})
}

func waitUp(t *testing.T, c *TSClient, want bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, _, up := c.Snapshot(); up == want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for up==%v", want)
}

// waitCount waits (bounded) for the fake server to have read n messages of the
// given type before asserting the count is exactly n.
//
// The fake records a message only when its read() returns, which can lag the
// client's write. In the toggle tests the fake flips the muted flag on the
// *press*, so SetMute can observe the flip and return while the fake has not
// yet read the release — counting right after SetMute returns then sees 1 of 2
// presses. This is a test-side race only; ts.go always writes both halves.
func waitCount(t *testing.T, f *fakeTS, typ string, n int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && f.countType(typ) < n {
		time.Sleep(2 * time.Millisecond)
	}
	if got := f.countType(typ); got != n {
		t.Errorf("sent %d %s messages, want %d", got, typ, n)
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// requireAuthFixture skips the test up front when the auth fixture is absent.
// authReply itself runs inside the fake server's goroutine, where skipping is
// not possible, so tests that use it call this first.
func requireAuthFixture(t *testing.T) {
	t.Helper()
	fixture(t, "auth_full.json")
}

// --- tests ----------------------------------------------------------------

func TestAuthSendsExactIdentityWithStoredKey(t *testing.T) {
	requireAuthFixture(t)
	const key = "stored-key-1234"
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "apikey")
	if err := os.WriteFile(keyPath, []byte(key+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	f := newFakeTS(t, func(s *fakeSession) {
		if m := s.read(); m != nil {
			s.writeRaw(authReply(t, key, nil))
		}
		<-s.ctx.Done()
	})

	c := NewTSClient(f.addr, keyPath)
	startClient(t, c)
	waitUp(t, c, true)

	msgs := f.messages()
	if len(msgs) == 0 {
		t.Fatal("client sent nothing")
	}
	got := msgs[0]
	want := map[string]any{
		"type": "auth",
		"payload": map[string]any{
			"identifier":  "ts6tray",
			"version":     "0.1.0",
			"name":        "ts6tray",
			"description": "Tray icon and mute control for TeamSpeak 6",
			"content":     map[string]any{"apiKey": key},
		},
	}
	gotJSON, _ := json.Marshal(got)
	wantJSON, _ := json.Marshal(want)
	if string(gotJSON) != string(wantJSON) {
		t.Errorf("auth payload mismatch\n got: %s\nwant: %s", gotJSON, wantJSON)
	}

	conns, icon, up := c.Snapshot()
	if !up || len(conns) != 1 || conns[0].ID != 1 || conns[0].ClientID != 18 {
		t.Fatalf("snapshot = %+v, icon %v, up %v", conns, icon, up)
	}
	if icon != IconQuiet {
		t.Errorf("icon = %v, want quiet", icon)
	}
	// The key came back unchanged, so the file must be untouched.
	if b, _ := os.ReadFile(keyPath); strings.TrimSpace(string(b)) != key {
		t.Errorf("key file = %q, want %q", b, key)
	}
}

func TestEmptyKeyIsSavedWith0600(t *testing.T) {
	requireAuthFixture(t)
	const newKey = "fresh-key-abcdef"
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "ts6tray", "apikey")

	f := newFakeTS(t, func(s *fakeSession) {
		m := s.read()
		if m == nil {
			return
		}
		payload := m["payload"].(map[string]any)
		content := payload["content"].(map[string]any)
		if content["apiKey"] != "" {
			t.Errorf("apiKey = %v, want empty on first auth", content["apiKey"])
		}
		s.writeRaw(authReply(t, newKey, nil))
		<-s.ctx.Done()
	})

	c := NewTSClient(f.addr, keyPath)
	startClient(t, c)
	waitUp(t, c, true)

	waitFor(t, "key file", func() bool { _, err := os.Stat(keyPath); return err == nil })
	b, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(b)) != newKey {
		t.Errorf("saved key = %q, want %q", b, newKey)
	}
	fi, err := os.Stat(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("key file mode = %v, want 0600", fi.Mode().Perm())
	}
	di, err := os.Stat(filepath.Dir(keyPath))
	if err != nil {
		t.Fatal(err)
	}
	if di.Mode().Perm() != 0o700 {
		t.Errorf("key dir mode = %v, want 0700", di.Mode().Perm())
	}
}

func TestPushedPropertiesUpdateSnapshotAndUpdates(t *testing.T) {
	requireAuthFixture(t)
	push := make(chan struct{})
	f := newFakeTS(t, func(s *fakeSession) {
		if s.read() == nil {
			return
		}
		s.writeRaw(authReply(t, "k", nil))
		<-push
		s.write(propsUpdate(map[string]any{"inputMuted": true}))
		<-s.ctx.Done()
	})

	c := NewTSClient(f.addr, filepath.Join(t.TempDir(), "apikey"))
	startClient(t, c)
	waitUp(t, c, true)

	// Drain the up-notification so the next one is the property change.
	select {
	case <-c.Updates():
	case <-time.After(2 * time.Second):
		t.Fatal("no update for connect")
	}

	close(push)
	select {
	case <-c.Updates():
	case <-time.After(5 * time.Second):
		t.Fatal("no update for clientPropertiesUpdated")
	}
	conns, icon, up := c.Snapshot()
	if !up || len(conns) != 1 || !conns[0].InputMuted {
		t.Fatalf("snapshot = %+v, up %v", conns, up)
	}
	if icon != IconMicMuted {
		t.Errorf("icon = %v, want mic-muted", icon)
	}
}

func TestSetMuteMuteWhenAlreadyMutedSendsNothing(t *testing.T) {
	requireAuthFixture(t)
	f := newFakeTS(t, func(s *fakeSession) {
		if s.read() == nil {
			return
		}
		s.writeRaw(authReply(t, "k", map[string]any{"inputMuted": true}))
		for s.read() != nil {
		}
	})

	c := NewTSClient(f.addr, filepath.Join(t.TempDir(), "apikey"))
	startClient(t, c)
	waitUp(t, c, true)

	changed, err := c.SetMute("mic", "mute")
	if err != nil || changed {
		t.Fatalf("SetMute(mic,mute) = (%v, %v), want (false, nil)", changed, err)
	}
	time.Sleep(50 * time.Millisecond)
	if n := f.countType("buttonPress"); n != 0 {
		t.Errorf("sent %d buttonPress messages, want 0", n)
	}
}

func TestSetMuteToggleWithFlipSucceeds(t *testing.T) {
	requireAuthFixture(t)
	f := newFakeTS(t, func(s *fakeSession) {
		if s.read() == nil {
			return
		}
		s.writeRaw(authReply(t, "k", nil))
		muted := false
		for {
			m := s.read()
			if m == nil {
				return
			}
			if m["type"] != "buttonPress" {
				continue
			}
			p := m["payload"].(map[string]any)
			s.write(buttonAck(p["button"].(string), p["state"].(bool)))
			if p["state"].(bool) { // act on the press, like a bound hotkey
				muted = !muted
				s.write(propsUpdate(map[string]any{"inputMuted": muted}))
			}
		}
	})

	c := NewTSClient(f.addr, filepath.Join(t.TempDir(), "apikey"))
	startClient(t, c)
	waitUp(t, c, true)

	changed, err := c.SetMute("mic", "toggle")
	if err != nil || !changed {
		t.Fatalf("SetMute(mic,toggle) = (%v, %v), want (true, nil)", changed, err)
	}
	if conns, icon, _ := c.Snapshot(); len(conns) != 1 || !conns[0].InputMuted || icon != IconMicMuted {
		t.Errorf("after toggle: conns %+v icon %v", conns, icon)
	}
	waitCount(t, f, "buttonPress", 2) // down and up
}

func TestSetMuteAckOnlyIsNotBound(t *testing.T) {
	requireAuthFixture(t)
	f := newFakeTS(t, func(s *fakeSession) {
		if s.read() == nil {
			return
		}
		s.writeRaw(authReply(t, "k", nil))
		for {
			m := s.read()
			if m == nil {
				return
			}
			if m["type"] != "buttonPress" {
				continue
			}
			p := m["payload"].(map[string]any)
			s.write(buttonAck(p["button"].(string), p["state"].(bool))) // ack only
		}
	})

	c := NewTSClient(f.addr, filepath.Join(t.TempDir(), "apikey"))
	c.flipWait = 50 * time.Millisecond
	startClient(t, c)
	waitUp(t, c, true)

	changed, err := c.SetMute("speaker", "mute")
	if changed {
		t.Errorf("changed = true, want false")
	}
	if !errors.Is(err, ErrNotBound) {
		t.Fatalf("err = %v, want ErrNotBound", err)
	}
	// The steps have to point at the terminal UI, which is where the bind
	// helper lives now — the tray menu no longer has a Settings submenu.
	for _, want := range []string{"ts6tray settings", "Key Bindings", "Toggle speaker mute",
		"Bind speaker key", "5 seconds", ButtonSpeaker} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error message missing %q: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "tray menu") {
		t.Errorf("the error still sends the user to the tray menu: %v", err)
	}
	waitCount(t, f, "buttonPress", 2)
}

func TestSetMuteWithoutConnection(t *testing.T) {
	c := NewTSClient("127.0.0.1:1", filepath.Join(t.TempDir(), "apikey"))
	if _, err := c.SetMute("mic", "toggle"); !errors.Is(err, ErrNotConnected) {
		t.Errorf("err = %v, want ErrNotConnected", err)
	}
	if _, err := c.SetMute("nose", "toggle"); err == nil {
		t.Error("bad target accepted")
	}
	if _, err := c.SetMute("mic", "sideways"); err == nil {
		t.Error("bad mode accepted")
	}
}

func TestReconnectAfterServerClose(t *testing.T) {
	requireAuthFixture(t)
	second := make(chan struct{})
	var once sync.Once
	f := newFakeTS(t, func(s *fakeSession) {
		if s.read() == nil {
			return
		}
		s.writeRaw(authReply(t, "k", nil))
		if s.n == 1 {
			// Let the client see the connection come up, then drop it.
			time.Sleep(20 * time.Millisecond)
			_ = s.c.Close(websocket.StatusNormalClosure, "bye")
			return
		}
		once.Do(func() { close(second) })
		<-s.ctx.Done()
	})

	c := NewTSClient(f.addr, filepath.Join(t.TempDir(), "apikey"))
	c.backoffBase = 10 * time.Millisecond
	startClient(t, c)

	waitUp(t, c, true)
	waitUp(t, c, false)
	if conns, icon, up := c.Snapshot(); up || conns != nil || icon != IconNone {
		t.Errorf("while down: conns %+v icon %v up %v", conns, icon, up)
	}
	select {
	case <-second:
	case <-time.After(5 * time.Second):
		t.Fatal("client did not reconnect")
	}
	waitUp(t, c, true)
	if got := atomic.LoadInt32(&f.conns); got < 2 {
		t.Errorf("accepted %d connections, want >= 2", got)
	}
}

func TestRunExitsOnContextCancel(t *testing.T) {
	requireAuthFixture(t)
	f := newFakeTS(t, func(s *fakeSession) {
		if s.read() == nil {
			return
		}
		s.writeRaw(authReply(t, "k", nil))
		<-s.ctx.Done()
	})

	c := NewTSClient(f.addr, filepath.Join(t.TempDir(), "apikey"))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); c.Run(ctx) }()
	waitUp(t, c, true)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
	if _, _, up := c.Snapshot(); up {
		t.Error("still up after Run returned")
	}
}

func TestRunExitsWhileServerUnreachable(t *testing.T) {
	c := NewTSClient("127.0.0.1:1", filepath.Join(t.TempDir(), "apikey"))
	c.backoffBase = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); c.Run(ctx) }()
	time.Sleep(50 * time.Millisecond)
	if _, _, up := c.Snapshot(); up {
		t.Error("up although nothing is listening")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func TestDefaultKeyPath(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/tmp/xdg")
	if got, want := DefaultKeyPath(), "/tmp/xdg/ts6tray/apikey"; got != want {
		t.Errorf("DefaultKeyPath() = %q, want %q", got, want)
	}
	t.Setenv("XDG_CONFIG_HOME", "")
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}
	if got, want := DefaultKeyPath(), filepath.Join(home, ".config/ts6tray/apikey"); got != want {
		t.Errorf("DefaultKeyPath() = %q, want %q", got, want)
	}
}

// --- regression tests -----------------------------------------------------

// saveKey must tighten a pre-existing loose dir/file, not just a fresh one:
// MkdirAll and WriteFile apply their mode only to what they create, so a
// rotated key used to land in an existing 0644 file.
func TestSaveKeyTightensExistingLoosePermissions(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "ts6tray")
	if err := os.MkdirAll(cfg, 0o755); err != nil { // pre-existing loose config dir
		t.Fatal(err)
	}
	keyPath := filepath.Join(cfg, "apikey")
	if err := os.WriteFile(keyPath, []byte("old-key\n"), 0o644); err != nil { // loose key file
		t.Fatal(err)
	}

	c := NewTSClient("127.0.0.1:1", keyPath)
	if err := c.saveKey("rotated-secret"); err != nil {
		t.Fatalf("saveKey: %v", err)
	}

	if b, _ := os.ReadFile(keyPath); strings.TrimSpace(string(b)) != "rotated-secret" {
		t.Errorf("key file = %q, want %q", b, "rotated-secret")
	}
	fi, err := os.Stat(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("key file mode = %v, want 0600", fi.Mode().Perm())
	}
	di, err := os.Stat(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if di.Mode().Perm() != 0o700 {
		t.Errorf("config dir mode = %v, want 0700", di.Mode().Perm())
	}
}

// Rapid clicks must not queue: while one SetMute sits in flipWait, every other
// call returns ErrBusy immediately and sends no press of its own.
func TestSetMuteWhileBusyReturnsErrBusy(t *testing.T) {
	requireAuthFixture(t)
	f := newFakeTS(t, func(s *fakeSession) {
		if s.read() == nil {
			return
		}
		s.writeRaw(authReply(t, "k", nil))
		for {
			m := s.read()
			if m == nil {
				return
			}
			if m["type"] != "buttonPress" {
				continue
			}
			p := m["payload"].(map[string]any)
			s.write(buttonAck(p["button"].(string), p["state"].(bool))) // ack only: unbound
		}
	})

	c := NewTSClient(f.addr, filepath.Join(t.TempDir(), "apikey"))
	c.flipWait = 400 * time.Millisecond
	startClient(t, c)
	waitUp(t, c, true)

	firstDone := make(chan error, 1)
	go func() {
		_, err := c.SetMute("mic", "toggle")
		firstDone <- err
	}()

	// Wait until the first call is actually in flipWait, i.e. has sent both
	// halves of its press.
	waitFor(t, "first press pair", func() bool { return f.countType("buttonPress") == 2 })

	// Every further click while the first one waits is dropped.
	const clicks = 7
	var busy int
	for i := 0; i < clicks; i++ {
		changed, err := c.SetMute("mic", "toggle")
		if changed {
			t.Errorf("click %d reported a change", i)
		}
		if errors.Is(err, ErrBusy) {
			busy++
			continue
		}
		t.Errorf("click %d: err = %v, want ErrBusy", i, err)
	}
	if busy != clicks {
		t.Errorf("%d of %d rapid clicks returned ErrBusy, want all", busy, clicks)
	}

	select {
	case err := <-firstDone:
		if !errors.Is(err, ErrNotBound) {
			t.Fatalf("first SetMute err = %v, want ErrNotBound", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("first SetMute never returned")
	}

	// Nothing was queued behind it: still exactly one press pair.
	time.Sleep(50 * time.Millisecond)
	waitCount(t, f, "buttonPress", 2) // still one down/up pair: nothing queued
	if err := ErrBusy.Error(); err != "a mute command is already in progress" {
		t.Errorf("ErrBusy text = %q", err)
	}
}

// The "already in that state" shortcut still has to work, and must not leave
// the press lock held.
func TestSetMuteNoopStillReleasesPressLock(t *testing.T) {
	requireAuthFixture(t)
	f := newFakeTS(t, func(s *fakeSession) {
		if s.read() == nil {
			return
		}
		s.writeRaw(authReply(t, "k", map[string]any{"inputMuted": true}))
		for s.read() != nil {
		}
	})

	c := NewTSClient(f.addr, filepath.Join(t.TempDir(), "apikey"))
	startClient(t, c)
	waitUp(t, c, true)

	for i := 0; i < 5; i++ {
		changed, err := c.SetMute("mic", "mute")
		if changed || err != nil {
			t.Fatalf("call %d: SetMute(mic,mute) = (%v, %v), want (false, nil)", i, changed, err)
		}
	}
	if n := f.countType("buttonPress"); n != 0 {
		t.Errorf("sent %d buttonPress messages, want 0", n)
	}
}

// --- Press (bind helper) --------------------------------------------------

// Press sends exactly one down/up pair for the button it was given and waits
// for nothing: the fake never reports a flag change here.
func TestPressSendsOneDownUpPair(t *testing.T) {
	requireAuthFixture(t)
	f := newFakeTS(t, func(s *fakeSession) {
		if s.read() == nil {
			return
		}
		s.writeRaw(authReply(t, "k", nil))
		for {
			m := s.read()
			if m == nil {
				return
			}
			if m["type"] != "buttonPress" {
				continue
			}
			p := m["payload"].(map[string]any)
			s.write(buttonAck(p["button"].(string), p["state"].(bool))) // ack only
		}
	})

	c := NewTSClient(f.addr, filepath.Join(t.TempDir(), "apikey"))
	c.flipWait = 5 * time.Second // must not be waited on
	startClient(t, c)
	waitUp(t, c, true)

	start := time.Now()
	if err := c.Press(ButtonSpeaker); err != nil {
		t.Fatalf("Press = %v, want nil", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("Press took %s: it must not wait for a flag change", d)
	}

	waitCount(t, f, "buttonPress", 2)
	var states []bool
	for _, m := range f.messages() {
		if m["type"] != "buttonPress" {
			continue
		}
		p := m["payload"].(map[string]any)
		if p["button"] != ButtonSpeaker {
			t.Errorf("pressed button %v, want %v", p["button"], ButtonSpeaker)
		}
		states = append(states, p["state"].(bool))
	}
	if len(states) != 2 || !states[0] || states[1] {
		t.Errorf("button states = %v, want [true false]", states)
	}
}

func TestPressWithoutConnection(t *testing.T) {
	c := NewTSClient("127.0.0.1:1", filepath.Join(t.TempDir(), "apikey"))
	if err := c.Press(ButtonMic); !errors.Is(err, ErrNotConnected) {
		t.Errorf("err = %v, want ErrNotConnected", err)
	}
}

// A bind press must not race a mute: while a SetMute waits for its flag to
// flip, Press takes the same lock and is refused instead of pressing.
func TestPressWhileSetMuteInFlightReturnsErrBusy(t *testing.T) {
	requireAuthFixture(t)
	f := newFakeTS(t, func(s *fakeSession) {
		if s.read() == nil {
			return
		}
		s.writeRaw(authReply(t, "k", nil))
		for {
			m := s.read()
			if m == nil {
				return
			}
			if m["type"] != "buttonPress" {
				continue
			}
			p := m["payload"].(map[string]any)
			s.write(buttonAck(p["button"].(string), p["state"].(bool))) // ack only: unbound
		}
	})

	c := NewTSClient(f.addr, filepath.Join(t.TempDir(), "apikey"))
	c.flipWait = 400 * time.Millisecond
	startClient(t, c)
	waitUp(t, c, true)

	muteDone := make(chan error, 1)
	go func() {
		_, err := c.SetMute("mic", "toggle")
		muteDone <- err
	}()
	waitFor(t, "SetMute press pair", func() bool { return f.countType("buttonPress") == 2 })

	if err := c.Press(ButtonMic); !errors.Is(err, ErrBusy) {
		t.Errorf("Press during SetMute = %v, want ErrBusy", err)
	}

	select {
	case err := <-muteDone:
		if !errors.Is(err, ErrNotBound) {
			t.Fatalf("SetMute err = %v, want ErrNotBound", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("SetMute never returned")
	}

	// The refused Press sent nothing: still just SetMute's one pair.
	time.Sleep(50 * time.Millisecond)
	waitCount(t, f, "buttonPress", 2)

	// Once SetMute is done the lock is free again.
	if err := c.Press(ButtonMic); err != nil {
		t.Fatalf("Press after SetMute = %v, want nil", err)
	}
	waitCount(t, f, "buttonPress", 4)
}

// A revoked key: TeamSpeak answers the auth with status.code != 0. The client
// must forget that key, re-auth immediately with an empty key (which is what
// makes TeamSpeak show the approval prompt) and save the key it gets back.
func TestRejectedKeyReAsksForApproval(t *testing.T) {
	requireAuthFixture(t)
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "apikey")
	if err := os.WriteFile(keyPath, []byte("old\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	f := newFakeTS(t, func(s *fakeSession) {
		m := s.read()
		if m == nil {
			return
		}
		key := m["payload"].(map[string]any)["content"].(map[string]any)["apiKey"]
		if s.n == 1 {
			if key != "old" {
				t.Errorf("first auth apiKey = %v, want %q", key, "old")
			}
			s.write(map[string]any{
				"type":    "auth",
				"status":  map[string]any{"code": 1, "message": "invalid api key"},
				"payload": map[string]any{},
			})
			<-s.ctx.Done()
			return
		}
		if key != "" {
			t.Errorf("auth %d apiKey = %v, want empty after rejection", s.n, key)
		}
		s.writeRaw(authReply(t, "new", nil))
		<-s.ctx.Done()
	})

	c := NewTSClient(f.addr, keyPath)
	c.backoffBase = time.Hour // the retry after a rejection must not wait
	startClient(t, c)
	waitUp(t, c, true)

	waitFor(t, "new key file", func() bool {
		b, _ := os.ReadFile(keyPath)
		return strings.TrimSpace(string(b)) == "new"
	})
	waitCount(t, f, "auth", 2)
	msgs := f.messages()
	var keys []any
	for _, m := range msgs {
		if m["type"] != "auth" {
			continue
		}
		keys = append(keys, m["payload"].(map[string]any)["content"].(map[string]any)["apiKey"])
	}
	if len(keys) != 2 || keys[0] != "old" || keys[1] != "" {
		t.Errorf("auth keys = %#v, want [old \"\"]", keys)
	}
}

// A connection that drops without an auth reply (TeamSpeak quitting, a crash)
// is not a rejection: the client keeps using the stored key and never asks for
// approval again, so restarting TeamSpeak cannot pop an approval prompt.
func TestConnectionCloseKeepsStoredKey(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "apikey")
	if err := os.WriteFile(keyPath, []byte("old\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	f := newFakeTS(t, func(s *fakeSession) {
		if s.read() == nil {
			return
		}
		_ = s.c.Close(websocket.StatusNormalClosure, "bye")
	})

	c := NewTSClient(f.addr, keyPath)
	c.backoffBase = 10 * time.Millisecond
	startClient(t, c)

	waitFor(t, "three auth attempts", func() bool { return f.countType("auth") >= 3 })
	for i, m := range f.messages() {
		if m["type"] != "auth" {
			continue
		}
		key := m["payload"].(map[string]any)["content"].(map[string]any)["apiKey"]
		if key != "old" {
			t.Fatalf("auth %d apiKey = %v, want %q on every retry", i, key, "old")
		}
	}
	if b, _ := os.ReadFile(keyPath); strings.TrimSpace(string(b)) != "old" {
		t.Errorf("key file = %q, want %q", b, "old")
	}
	if _, _, up := c.Snapshot(); up {
		t.Error("up although the server never replied")
	}
}

// TestPushedTextMessageProducesANotice is the seam between ts.go and notify.go:
// the roster is seeded from the same auth reply the State gets, every later
// message goes through it too, and what it produces comes out of Notices().
// auth_full.json is conn 1 / client 18, so the private message is addressed
// there and sent by someone else.
func TestPushedTextMessageProducesANotice(t *testing.T) {
	requireAuthFixture(t)
	push := make(chan struct{})
	f := newFakeTS(t, func(s *fakeSession) {
		if s.read() == nil {
			return
		}
		s.writeRaw(authReply(t, "k", nil))
		<-push
		s.write(map[string]any{
			"type": "textMessage",
			"payload": map[string]any{
				"connectionId": 1,
				"invoker":      map[string]any{"id": 11, "nickname": "UserC"},
				"message":      "hi",
				"targetId":     18,
				"targetMode":   targetPrivate,
			},
		})
		<-s.ctx.Done()
	})

	c := NewTSClient(f.addr, filepath.Join(t.TempDir(), "apikey"))
	startClient(t, c)
	waitUp(t, c, true)

	// The auth snapshot is the baseline, so it must not have produced anything.
	select {
	case n := <-c.Notices():
		t.Fatalf("the auth snapshot produced a notice: %+v", n)
	default:
	}

	close(push)
	select {
	case n := <-c.Notices():
		if n.kind != noticePrivateMsg || n.title != "Message from UserC" || n.body != "hi" {
			t.Errorf("notice = %+v, want a private message from UserC saying hi", n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no notice for the private message")
	}
}

// TestNoticesAreDroppedWhenNobodyListens: the channel is buffered and written
// non-blockingly, so a tray that stopped draining can never stall the reader.
func TestNoticesAreDroppedWhenNobodyListens(t *testing.T) {
	c := NewTSClient("127.0.0.1:1", filepath.Join(t.TempDir(), "apikey"))
	many := make([]notice, cap(c.notices)+10)
	done := make(chan struct{})
	go func() { c.pushNotices(many); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pushNotices blocked on a full buffer")
	}
	if got := len(c.notices); got != cap(c.notices) {
		t.Errorf("buffered %d notices, want the cap %d", got, cap(c.notices))
	}
}
