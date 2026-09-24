package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// --- fake backend ---------------------------------------------------------

type fakeBackend struct {
	mu      sync.Mutex
	conns   []Conn
	icon    Icon
	up      bool
	changed bool
	err     error

	calls []string // "target mode", in order
}

func (f *fakeBackend) Snapshot() ([]Conn, Icon, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Conn(nil), f.conns...), f.icon, f.up
}

func (f *fakeBackend) SetMute(target, mode string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, target+" "+mode)
	return f.changed, f.err
}

func (f *fakeBackend) callLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// --- helpers --------------------------------------------------------------

// serve starts ServeIPC on a socket in t.TempDir() and waits until it answers.
// It returns the socket path and a stop func that cancels and waits.
func serve(t *testing.T, b ipcBackend) (string, func()) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "s.sock")
	return path, serveAt(t, b, path)
}

// reload is optional; without one the daemon under test has no tray to reload.
func serveAt(t *testing.T, b ipcBackend, path string, reload ...func() error) func() {
	t.Helper()
	var rl func() error
	if len(reload) == 1 {
		rl = reload[0]
	}
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- ServeIPC(ctx, b, path, rl) }()
	waitReady(t, path)
	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		select {
		case err := <-errc:
			if err != nil {
				t.Errorf("ServeIPC returned %v, want nil", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("ServeIPC did not return after ctx cancel")
		}
	}
	t.Cleanup(stop)
	return stop
}

func waitReady(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := net.Dial("unix", path); err == nil {
			c.Close()
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("socket %s never became ready", path)
}

func run(t *testing.T, path string, args ...string) (int, string) {
	t.Helper()
	var buf bytes.Buffer
	code := RunIPCClient(path, args, &buf)
	return code, buf.String()
}

// --- tests ----------------------------------------------------------------

func TestSocketPath(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/1000")
	if got, want := SocketPath(), "/run/user/1000/ts6tray.sock"; got != want {
		t.Errorf("SocketPath() = %q, want %q", got, want)
	}
	t.Setenv("XDG_RUNTIME_DIR", "")
	got := SocketPath()
	if !strings.HasPrefix(got, os.TempDir()) || !strings.HasSuffix(got, ".sock") ||
		!strings.Contains(got, "ts6tray-") {
		t.Errorf("SocketPath() fallback = %q, want <tmp>/ts6tray-<uid>.sock", got)
	}
}

func TestStatusTwoServers(t *testing.T) {
	b := &fakeBackend{
		up:   true,
		icon: IconMicMuted,
		conns: []Conn{
			{ID: 1, ServerName: "Alpha", Status: StatusConnectionEstablished,
				InputHardware: true, InputMuted: true},
			{ID: 2, ServerName: "Beta", Status: StatusConnectionEstablished,
				OutputMuted: true, Talking: true},
		},
	}
	path, _ := serve(t, b)

	code, out := run(t, path, "status")
	if code != 0 {
		t.Fatalf("exit %d, want 0; output:\n%s", code, out)
	}
	want := "TeamSpeak: connected\n" +
		"icon: mic-muted\n" +
		"* Alpha: mic muted, speaker on\n" +
		"  Beta: mic disabled, speaker muted, talking\n"
	if out != want {
		t.Errorf("status output:\n%q\nwant:\n%q", out, want)
	}
}

func TestStatusNotReachableAndUnnamedServer(t *testing.T) {
	b := &fakeBackend{
		up:    false,
		icon:  IconNone,
		conns: []Conn{{ID: 7, Status: StatusConnectionEstablished, InputHardware: true}},
	}
	path, _ := serve(t, b)

	code, out := run(t, path, "status")
	if code != 0 {
		t.Fatalf("exit %d, want 0; output:\n%s", code, out)
	}
	want := "TeamSpeak: not reachable\nicon: none\n* server 7: mic on, speaker on\n"
	if out != want {
		t.Errorf("status output:\n%q\nwant:\n%q", out, want)
	}
}

func TestMicMuteAlreadyMuted(t *testing.T) {
	b := &fakeBackend{changed: false, up: true}
	path, _ := serve(t, b)

	code, out := run(t, path, "mic", "mute")
	if code != 0 {
		t.Fatalf("exit %d, want 0; output:\n%s", code, out)
	}
	if out != "mic already muted\n" {
		t.Errorf("output %q, want %q", out, "mic already muted\n")
	}
	if got := b.callLog(); len(got) != 1 || got[0] != "mic mute" {
		t.Errorf("backend calls = %v, want [\"mic mute\"]", got)
	}
}

func TestSpeakerUnmuteChanged(t *testing.T) {
	b := &fakeBackend{changed: true, up: true}
	path, _ := serve(t, b)

	code, out := run(t, path, "speaker", "unmute")
	if code != 0 || out != "speaker unmuted\n" {
		t.Errorf("exit %d output %q, want 0 and %q", code, out, "speaker unmuted\n")
	}
}

func TestMicToggleReportsSnapshotState(t *testing.T) {
	b := &fakeBackend{
		changed: true, up: true,
		conns: []Conn{
			{ID: 1, ServerName: "Alpha", Status: StatusConnectionEstablished},
			{ID: 2, ServerName: "Beta", Status: StatusConnectionEstablished,
				InputHardware: true, InputMuted: true},
		},
	}
	path, _ := serve(t, b)

	code, out := run(t, path, "mic", "toggle")
	if code != 0 || out != "mic muted\n" {
		t.Errorf("exit %d output %q, want 0 and %q", code, out, "mic muted\n")
	}
}

func TestSetMuteErrorPassesThroughVerbatim(t *testing.T) {
	const msg = "mic mute hotkey is not bound in TeamSpeak.\n" +
		"Open Settings -> Keybinds, bind \"Mute Microphone\" to F13, then retry."
	b := &fakeBackend{err: errors.New(msg)}
	path, _ := serve(t, b)

	code, out := run(t, path, "mic", "toggle")
	if code != 1 {
		t.Fatalf("exit %d, want 1; output:\n%s", code, out)
	}
	if out != msg+"\n" {
		t.Errorf("output:\n%q\nwant verbatim:\n%q", out, msg+"\n")
	}
}

func TestBadArgsExit2WithoutDialing(t *testing.T) {
	// A real listener that counts accepts: the client must not touch it.
	path := filepath.Join(t.TempDir(), "s.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var accepts atomic.Int64
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			accepts.Add(1)
			c.Close()
		}
	}()

	for _, args := range [][]string{
		nil,
		{},
		{"status", "extra"},
		{"mic"},
		{"mic", "loud"},
		{"nose", "toggle"},
		{"mic", "toggle", "please"},
		{"--help"},
	} {
		code, out := run(t, path, args...)
		if code != 2 {
			t.Errorf("args %v: exit %d, want 2", args, code)
		}
		if !strings.Contains(out, "ts6tray mic|speaker toggle|mute|unmute") ||
			!strings.Contains(out, "ts6tray status") {
			t.Errorf("args %v: output %q lacks usage", args, out)
		}
	}
	if n := accepts.Load(); n != 0 {
		t.Errorf("client dialed %d times on bad args, want 0", n)
	}
}

func TestDaemonNotRunning(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent.sock")
	code, out := run(t, path, "status")
	if code != 1 {
		t.Fatalf("exit %d, want 1; output: %q", code, out)
	}
	if out != ipcNotRunning+"\n" {
		t.Errorf("output %q, want %q", out, ipcNotRunning+"\n")
	}
	if !strings.Contains(out, "ts6tray --daemon") {
		t.Errorf("output %q lacks the start hint", out)
	}
}

func TestStaleSocketIsReplaced(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.sock")
	// Bind and close without unlinking: a socket file with nobody behind it.
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	ln.Close()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("stale socket not on disk: %v", err)
	}

	b := &fakeBackend{up: true, icon: IconQuiet}
	serveAt(t, b, path)

	code, out := run(t, path, "status")
	if code != 0 || !strings.HasPrefix(out, "TeamSpeak: connected\n") {
		t.Errorf("exit %d output %q after replacing stale socket", code, out)
	}
}

func TestSecondServeIPCOnLiveSocketErrors(t *testing.T) {
	b := &fakeBackend{up: true}
	path, _ := serve(t, b)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := ServeIPC(ctx, b, path, nil)
	if err == nil {
		t.Fatal("second ServeIPC returned nil, want an already-running error")
	}
	if !strings.Contains(err.Error(), "already running") {
		t.Errorf("error %q, want it to say already running", err)
	}
	// The live daemon must still work.
	if code, out := run(t, path, "status"); code != 0 {
		t.Errorf("first daemon broken after the failed second: exit %d, %q", code, out)
	}
}

func TestSocketModeIs0600(t *testing.T) {
	path, _ := serve(t, &fakeBackend{})
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("socket mode %o, want 600", perm)
	}
}

func TestCtxCancelRemovesSocket(t *testing.T) {
	path, stop := serve(t, &fakeBackend{})
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("socket missing while serving: %v", err)
	}
	stop()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("socket still present after ctx cancel: stat err = %v", err)
	}
}

func TestGarbageRequestLine(t *testing.T) {
	b := &fakeBackend{up: true}
	path, _ := serve(t, b)

	for _, req := range []string{
		"\n",
		"   \n",
		"mic\n",
		"mic loud\n",
		"status now\n",
		"hello world\n",
		"mic toggle extra\n",
		strings.Repeat("A", 200) + "\n",
		"\x00\x01\x02 junk\n",
	} {
		status, body := rawRequest(t, path, req)
		if status != "err" {
			t.Errorf("request %q: first line %q, want \"err\" (body %q)", req, status, body)
		}
	}
	if got := b.callLog(); len(got) != 0 {
		t.Errorf("backend was called for garbage requests: %v", got)
	}

	// The daemon survives garbage and still answers a good request.
	if code, out := run(t, path, "status"); code != 0 {
		t.Errorf("daemon broken after garbage: exit %d, %q", code, out)
	}
}

// rawRequest speaks the wire protocol directly, bypassing the client's own
// argument validation.
func rawRequest(t *testing.T, path, req string) (status, body string) {
	t.Helper()
	c, err := net.Dial("unix", path)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Write([]byte(req)); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := new(bytes.Buffer)
	if _, err := buf.ReadFrom(c); err != nil {
		t.Fatalf("read: %v", err)
	}
	s, rest, _ := strings.Cut(strings.TrimRight(buf.String(), "\n"), "\n")
	return s, rest
}

func TestConcurrentRequests(t *testing.T) {
	b := &fakeBackend{up: true, icon: IconQuiet, changed: true}
	path, _ := serve(t, b)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			args := []string{"status"}
			if i%2 == 1 {
				args = []string{"mic", "mute"}
			}
			var buf bytes.Buffer
			if code := RunIPCClient(path, args, &buf); code != 0 {
				t.Errorf("goroutine %d: exit %d, output %q", i, code, buf.String())
			}
		}(i)
	}
	wg.Wait()
}

// --- regression tests -----------------------------------------------------

// A client that never sends a newline must be rejected without the daemon
// buffering what it sent. On the old bufio.ReadString path 1 MiB of headerless
// junk was accumulated in full; now the read is capped at ipcMaxRequest.
func TestOverlongRequestLineIsCappedAndRejected(t *testing.T) {
	b := &fakeBackend{up: true}
	path, _ := serve(t, b)

	c, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(10 * time.Second))

	// 1 MiB, no newline anywhere. The daemon must answer before it has read
	// all of it, so the write may well block; do it in the background.
	werr := make(chan error, 1)
	go func() {
		_, err := c.Write(bytes.Repeat([]byte("A"), 1<<20))
		werr <- err
	}()

	replied := make(chan string, 1)
	go func() {
		buf := new(bytes.Buffer)
		buf.ReadFrom(c)
		s, _, _ := strings.Cut(strings.TrimRight(buf.String(), "\n"), "\n")
		replied <- s
	}()

	select {
	case status := <-replied:
		if status != "err" {
			t.Errorf("first reply line %q, want \"err\"", status)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("daemon never answered an unterminated request line")
	}
	<-werr // whatever the write did, it must not wedge the test

	if got := b.callLog(); len(got) != 0 {
		t.Errorf("backend was called for an unterminated request: %v", got)
	}
	// The daemon survives it and still answers a good request.
	if code, out := run(t, path, "status"); code != 0 {
		t.Errorf("daemon broken after overlong request: exit %d, %q", code, out)
	}
}

// A request line just under the cap is still served; the cap rejects, it does
// not truncate a valid line into a different command.
func TestRequestLineAtCapBoundary(t *testing.T) {
	b := &fakeBackend{up: true, icon: IconQuiet}
	path, _ := serve(t, b)

	// "status" plus padding spaces, newline as the very last allowed byte.
	req := "status" + strings.Repeat(" ", ipcMaxRequest-7) + "\n"
	if len(req) != ipcMaxRequest {
		t.Fatalf("test built a %d-byte request, want %d", len(req), ipcMaxRequest)
	}
	if status, body := rawRequest(t, path, req); status != "ok" {
		t.Errorf("exactly-at-cap request: status %q body %q, want ok", status, body)
	}

	// One byte over: the newline no longer fits inside the cap.
	if status, _ := rawRequest(t, path, "status"+strings.Repeat(" ", ipcMaxRequest-6)+"\n"); status != "err" {
		t.Errorf("over-cap request: status %q, want err", status)
	}
}

// A daemon that accepts and then closes without replying must not surface as
// `unexpected reply from daemon: ""`.
func TestDaemonClosesWithoutReplying(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	// drain says whether the fake daemon reads the request before closing:
	// draining gives the client a clean EOF (io.ReadAll -> "", nil, the
	// reported case), not draining resets the connection.
	var drain atomic.Bool
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			if drain.Load() {
				bufio.NewReader(io.LimitReader(c, 4096)).ReadString('\n')
			}
			c.Close() // accepted, then gone: no reply at all
		}
	}()

	for _, d := range []bool{true, false} {
		drain.Store(d)
		code, out := run(t, path, "status")
		if code != 1 {
			t.Errorf("drain=%v: exit %d, want 1; output %q", d, code, out)
		}
		if strings.Contains(out, "unexpected reply") || strings.Contains(out, `""`) {
			t.Errorf("drain=%v: output %q still reports an empty reply as unexpected", d, out)
		}
		// Undrained, the request write itself may lose the race and fail, and
		// that path legitimately prints ipcNotRunning; drained, the read
		// always ends in the clean ("", nil) that was reported.
		if d && out != ipcClosedEarly+"\n" {
			t.Errorf("drain=%v: output %q, want %q", d, out, ipcClosedEarly+"\n")
		}
		if !d && out != ipcClosedEarly+"\n" && out != ipcNotRunning+"\n" {
			t.Errorf("drain=%v: output %q, want the closed-early or not-running message", d, out)
		}
	}
}

// The socket must never exist with looser-than-0600 permissions, not even for
// the instant between bind and chmod: it is created under a 0177 umask.
func TestSocketIsCreated0600UnderLooseUmask(t *testing.T) {
	old := syscall.Umask(0o000) // as permissive as a process can be
	defer syscall.Umask(old)

	path := filepath.Join(t.TempDir(), "s.sock")
	ln, err := ipcListen(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("socket mode %o under umask 000, want 600", perm)
	}
	// ipcListen restored the umask it found.
	if got := syscall.Umask(0o000); got != 0o000 {
		t.Errorf("umask after ipcListen is %o, want the 000 it was called with", got)
	}
}

// Shutdown must unlink only the socket this daemon bound. A successor that
// rebinds the same path after the first one is done must keep its own socket.
func TestShutdownDoesNotUnlinkSuccessorSocket(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.sock")
	stop := serveAt(t, &fakeBackend{}, path)
	stop() // first daemon fully returned; its socket is gone
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("first daemon left %s behind: stat err = %v", path, err)
	}

	b := &fakeBackend{up: true, icon: IconQuiet}
	serveAt(t, b, path)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("successor socket missing: %v", err)
	}
	if code, out := run(t, path, "status"); code != 0 {
		t.Errorf("successor daemon not answering: exit %d, %q", code, out)
	}
}

// --- reload ----------------------------------------------------------------

// TestIPCReloadCallsTheTray: `ts6tray reload` is what the settings TUI sends
// after every write, so the running daemon picks the change up.
func TestIPCReloadCallsTheTray(t *testing.T) {
	var calls atomic.Int32
	b := &fakeBackend{up: true}
	path := filepath.Join(t.TempDir(), "s.sock")
	stop := serveAt(t, b, path, func() error { calls.Add(1); return nil })
	defer stop()

	code, out := run(t, path, "reload")
	if code != 0 || !strings.Contains(out, "settings reloaded") {
		t.Errorf("reload: exit %d, output %q", code, out)
	}
	if calls.Load() != 1 {
		t.Errorf("reload func called %d times, want 1", calls.Load())
	}

	// A bad request is still an error, and still does not reach the tray.
	if code, out := run(t, path, "reload", "now"); code == 0 {
		t.Errorf("`reload now` succeeded: %q", out)
	}
	if calls.Load() != 1 {
		t.Errorf("a malformed reload reached the tray (%d calls)", calls.Load())
	}
}

// TestIPCReloadReportsFailures: no tray, or a tray that could not reload, is an
// "err" reply with the reason, not a silent success.
func TestIPCReloadReportsFailures(t *testing.T) {
	t.Run("no reload func", func(t *testing.T) {
		b := &fakeBackend{up: true}
		path, stop := serve(t, b)
		defer stop()
		code, out := run(t, path, "reload")
		if code == 0 || !strings.Contains(out, "cannot reload") {
			t.Errorf("exit %d, output %q", code, out)
		}
	})
	t.Run("the reload failed", func(t *testing.T) {
		b := &fakeBackend{up: true}
		path := filepath.Join(t.TempDir(), "s.sock")
		stop := serveAt(t, b, path, func() error { return errors.New("no tray icon") })
		defer stop()
		code, out := run(t, path, "reload")
		if code == 0 || !strings.Contains(out, "no tray icon") {
			t.Errorf("exit %d, output %q", code, out)
		}
	})
}

// TestTrayReloadHandle: the IPC server is started before the tray exists, so
// the handle it holds has to answer sensibly until RunTray fills it in.
func TestTrayReloadHandle(t *testing.T) {
	var rl trayReload
	if err := rl.Reload(); err == nil {
		t.Error("an unset trayReload reported success")
	}
	called := false
	rl.set(func() error { called = true; return nil })
	if err := rl.Reload(); err != nil || !called {
		t.Errorf("Reload() = %v, called = %v", err, called)
	}
}
