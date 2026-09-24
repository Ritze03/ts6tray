package main

// ipc.go is the Unix-socket link between the ts6tray daemon and its CLI (D13).
// The daemon owns the single TS6 connection and its approval, so every
// `ts6tray mic|speaker toggle|mute|unmute` and `ts6tray status` invocation is a
// short-lived client that asks the running daemon to act.
//
// Wire format, deliberately dumb: the client sends one request line of
// space-separated words ("mic toggle\n", "status\n") and the server answers
// with text lines — the first is "ok" or "err", the rest is the
// human-readable body — then closes. One request per connection.

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ipcBackend is what the IPC server needs from the TS6 client. *TSClient
// satisfies it; tests use a fake.
type ipcBackend interface {
	Snapshot() (conns []Conn, icon Icon, up bool)
	SetMute(target, mode string) (changed bool, err error)
}

const (
	// ipcTimeout bounds one request/response exchange. SetMute can wait ~1.5 s
	// for TeamSpeak to confirm the keypress, so leave room.
	ipcTimeout = 5 * time.Second
	// ipcProbeTimeout bounds the "is a daemon already there?" dial.
	ipcProbeTimeout = 500 * time.Millisecond
	// ipcMaxRequest caps one request line. The socket is a trust boundary even
	// at 0600, so nothing unbounded is read.
	ipcMaxRequest = 4096
)

const ipcUsage = "usage:\n  ts6tray mic|speaker toggle|mute|unmute\n  ts6tray status"

// ipcNotRunning is what the CLI prints when nothing answers the socket.
const ipcNotRunning = "ts6tray daemon not running — start it with `ts6tray --daemon`"

// ipcClosedEarly is what the CLI prints when the daemon accepted the
// connection but closed it without a reply (it was stopped mid-request).
const ipcClosedEarly = "ts6tray daemon closed the connection without replying (was it stopped?)"

// SocketPath is where the daemon listens: $XDG_RUNTIME_DIR/ts6tray.sock, or
// <tmp>/ts6tray-<uid>.sock when XDG_RUNTIME_DIR is unset.
func SocketPath() string {
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		return filepath.Join(dir, "ts6tray.sock")
	}
	return filepath.Join(os.TempDir(), "ts6tray-"+strconv.Itoa(os.Getuid())+".sock")
}

// --- server ---------------------------------------------------------------

// ServeIPC listens on path and serves requests until ctx is done, then removes
// the socket. It returns an error if another daemon is already listening.
func ServeIPC(ctx context.Context, b ipcBackend, path string) error {
	ln, err := ipcListen(path)
	if err != nil {
		return err
	}

	var wg sync.WaitGroup
	done := make(chan struct{})
	closed := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
		case <-done:
		}
		// Close unlinks the socket: *net.UnixListener does that itself, so
		// ServeIPC must never os.Remove(path) — by the time it returns, the
		// path may already belong to a successor daemon.
		ln.Close()
		close(closed)
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			close(done)
			<-closed // the unlink is part of shutdown, so wait for it
			wg.Wait()
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			ipcHandle(b, conn)
		}()
	}
}

// ipcListen binds path, clearing a stale socket but refusing to steal a live
// one.
func ipcListen(path string) (net.Listener, error) {
	if _, err := os.Stat(path); err == nil {
		if c, derr := net.DialTimeout("unix", path, ipcProbeTimeout); derr == nil {
			c.Close()
			return nil, fmt.Errorf("ts6tray already running on %s", path)
		}
		// Nobody home: the socket file outlived its daemon.
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("removing stale socket %s: %w", path, err)
		}
	}
	// Create the socket 0600 in the first place: the chmod below closes the
	// window only after the fact, which matters on the world-writable /tmp
	// fallback path.
	// ponytail: umask is process-wide, but ipcListen runs once at startup
	// before any other goroutine creates files, so a save/restore is enough.
	oldMask := syscall.Umask(0o177)
	ln, err := net.Listen("unix", path)
	syscall.Umask(oldMask)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		os.Remove(path)
		return nil, err
	}
	return ln, nil
}

// ipcHandle serves exactly one request on conn and closes it.
func ipcHandle(b ipcBackend, conn net.Conn) {
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(ipcTimeout))

	// The cap is the reader, not just its buffer: bufio.ReadString keeps
	// growing its own string past the buffer size, so only a LimitReader
	// actually bounds what a client can make the daemon hold.
	r := bufio.NewReaderSize(io.LimitReader(conn, ipcMaxRequest), ipcMaxRequest)
	line, err := r.ReadString('\n')
	if err != nil {
		// No newline within the cap, or the peer vanished mid-line. Either
		// way the request is malformed and nothing reaches the backend.
		ipcReply(conn, false, "malformed request: no newline within "+strconv.Itoa(ipcMaxRequest)+" bytes")
		// Half-close so the peer sees the reply and EOF at once, then swallow
		// the rest of its line: closing with data still unread would reset the
		// connection and take the reply with it. The tail is discarded, never
		// buffered, and bounded by the deadline set above.
		if cw, ok := conn.(interface{ CloseWrite() error }); ok {
			cw.CloseWrite()
		}
		io.Copy(io.Discard, conn)
		return
	}
	ok, body := ipcDispatch(b, strings.Fields(line))
	ipcReply(conn, ok, body)
}

// ipcReply writes one "ok"/"err" status line plus an optional body.
func ipcReply(conn net.Conn, ok bool, body string) {
	status := "err"
	if ok {
		status = "ok"
	}
	fmt.Fprintf(conn, "%s\n", status)
	if body != "" {
		fmt.Fprintf(conn, "%s\n", body)
	}
}

// ipcDispatch validates the request words and runs the command. The socket is
// a trust boundary, so nothing here trusts the client's spelling.
func ipcDispatch(b ipcBackend, words []string) (ok bool, body string) {
	if len(words) == 0 {
		return false, "empty request\n" + ipcUsage
	}
	switch words[0] {
	case "status":
		if len(words) != 1 {
			return false, "status takes no arguments"
		}
		return true, ipcStatusText(b)
	case "mic", "speaker":
		if len(words) != 2 {
			return false, "bad request: " + strings.Join(words, " ") + "\n" + ipcUsage
		}
		switch words[1] {
		case "toggle", "mute", "unmute":
		default:
			return false, "unknown mode " + strconv.Quote(words[1]) + "\n" + ipcUsage
		}
		return ipcSetMute(b, words[0], words[1])
	}
	return false, "unknown command " + strconv.Quote(words[0]) + "\n" + ipcUsage
}

// ipcSetMute forwards to the backend (D12: the press-only-if-different logic
// lives there) and reports the resulting state.
func ipcSetMute(b ipcBackend, target, mode string) (bool, string) {
	changed, err := b.SetMute(target, mode)
	if err != nil {
		// D12: when the key is unbound, the error text carries the binding
		// steps. Pass it through verbatim.
		return false, err.Error()
	}
	state := ipcMuteState(b, target, mode)
	if changed {
		return true, target + " " + state
	}
	return true, target + " already " + state
}

// ipcMuteState names the state the target ended up in. "mute"/"unmute" say it
// outright; for "toggle" the backend's fresh snapshot does.
func ipcMuteState(b ipcBackend, target, mode string) string {
	switch mode {
	case "mute":
		return "muted"
	case "unmute":
		return "unmuted"
	}
	conns, _, _ := b.Snapshot()
	for _, c := range conns {
		if !c.InputHardware || c.InputDeactivated {
			continue
		}
		if ipcMuted(c, target) {
			return "muted"
		}
		return "unmuted"
	}
	if len(conns) > 0 {
		if ipcMuted(conns[0], target) {
			return "muted"
		}
		return "unmuted"
	}
	return "toggled"
}

func ipcMuted(c Conn, target string) bool {
	if target == "speaker" {
		return c.OutputMuted
	}
	return c.InputMuted
}

// ipcStatusText renders the human-readable status body.
func ipcStatusText(b ipcBackend) string {
	conns, icon, up := b.Snapshot()
	var sb strings.Builder
	if up {
		sb.WriteString("TeamSpeak: connected\n")
	} else {
		sb.WriteString("TeamSpeak: not reachable\n")
	}
	sb.WriteString("icon: " + icon.String())
	for _, c := range conns {
		sb.WriteString("\n" + ipcConnLine(c))
	}
	return sb.String()
}

// ipcConnLine is one server line. "*" marks the connection holding the mic.
func ipcConnLine(c Conn) string {
	mark := " "
	if c.InputHardware && !c.InputDeactivated {
		mark = "*"
	}
	name := c.ServerName
	if name == "" {
		name = "server " + strconv.Itoa(c.ID)
	}
	mic := "on"
	switch {
	case c.MicDisabled():
		mic = "disabled"
	case c.InputMuted:
		mic = "muted"
	}
	speaker := "on"
	if c.OutputMuted {
		speaker = "muted"
	}
	line := mark + " " + name + ": mic " + mic + ", speaker " + speaker
	if c.Talking {
		line += ", talking"
	}
	return line
}

// --- client ---------------------------------------------------------------

// RunIPCClient sends one command to the daemon and prints the response body.
// It returns the process exit code: 0 on ok, 1 on err or no daemon, 2 on bad
// arguments (which are rejected without dialing).
func RunIPCClient(path string, args []string, out io.Writer) int {
	if !ipcValidArgs(args) {
		fmt.Fprintln(out, ipcUsage)
		return 2
	}

	conn, err := net.DialTimeout("unix", path, ipcTimeout)
	if err != nil {
		fmt.Fprintln(out, ipcNotRunning)
		return 1
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(ipcTimeout))

	if _, err := io.WriteString(conn, strings.Join(args, " ")+"\n"); err != nil {
		fmt.Fprintln(out, ipcNotRunning)
		return 1
	}
	resp, _ := io.ReadAll(conn)
	if len(resp) == 0 {
		// Nothing came back at all: the daemon accepted and then went away
		// (clean EOF or a reset). That is not an unexpected reply of "".
		fmt.Fprintln(out, ipcClosedEarly)
		return 1
	}
	status, body, _ := strings.Cut(strings.TrimRight(string(resp), "\n"), "\n")
	if body != "" {
		fmt.Fprintln(out, body)
	}
	if status == "ok" {
		return 0
	}
	if status != "err" {
		fmt.Fprintln(out, "unexpected reply from daemon: "+strconv.Quote(status))
	}
	return 1
}

// ipcValidArgs mirrors the server's validation so a typo never reaches the
// daemon.
func ipcValidArgs(args []string) bool {
	switch len(args) {
	case 1:
		return args[0] == "status"
	case 2:
		switch args[0] {
		case "mic", "speaker":
		default:
			return false
		}
		switch args[1] {
		case "toggle", "mute", "unmute":
			return true
		}
	}
	return false
}
