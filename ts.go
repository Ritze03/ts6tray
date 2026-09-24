package main

// ts.go is the TeamSpeak 6 Remote Apps client: one goroutine-safe TSClient that
// connects to the client's WebSocket, authenticates with the stored API key (or
// asks the user for approval once), feeds every message into a State, and sends
// mute key presses as buttonPress messages.
//
// See docs/protocol.md — everything here follows what was observed live:
//   - the auth identity must be sent byte-for-byte or the stored key is refused,
//   - the auth reply is always the full snapshot, even with a cached key,
//   - buttonPress works, keyPress is silently ignored,
//   - a buttonPress ack means "accepted", not "a hotkey fired": the only proof
//     is the flag changing in a following property event,
//   - cancelling the context passed to coder/websocket's Read closes the whole
//     connection, so there is exactly one reader goroutine using the
//     connection-lifetime context and no per-read deadlines.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// Button ids ts6tray presses. They are arbitrary strings; the user binds them
// once in TeamSpeak's Settings -> Key Bindings.
const (
	ButtonMic     = "ts6tray.mic"
	ButtonSpeaker = "ts6tray.speaker"
)

// The identity sent in the auth payload. Changing any of it invalidates the
// stored key (protocol.md, "The exact payload").
const (
	tsIdentifier  = "ts6tray"
	tsAppVersion  = "0.1.0"
	tsAppName     = "ts6tray"
	tsDescription = "Tray icon and mute control for TeamSpeak 6"
)

const (
	tsDefaultFlipWait    = 1500 * time.Millisecond
	tsDefaultBackoffBase = time.Second
	tsMaxBackoff         = 10 * time.Second
	tsDialTimeout        = 10 * time.Second
	tsWriteTimeout       = 5 * time.Second
	tsReadLimit          = 8 << 20 // the auth snapshot is ~80 KB; default is 32 KiB
)

// ErrNotConnected means TeamSpeak is unreachable, not authenticated, or has no
// established server connection to act on.
var ErrNotConnected = errors.New("not connected to TeamSpeak")

// ErrNotBound means the button press was accepted by TeamSpeak but no mute flag
// changed, which means the button is not bound to a key binding yet.
var ErrNotBound = errors.New("ts6tray button is not bound to a TeamSpeak key binding")

// errAuthRejected is an explicit "no" from TeamSpeak: an auth reply whose
// status.code is not 0. It is the only signal that makes ts6tray drop the
// stored key and ask for approval again — a plain close or network error never
// does, so a TeamSpeak restart cannot trigger a new approval prompt.
var errAuthRejected = errors.New("auth rejected by TeamSpeak")

// ErrBusy means another SetMute or Press is still running (a press can take up to
// flipWait to be confirmed). Rapid clicks are dropped instead of queued, so
// presses never keep firing after the user stopped clicking.
var ErrBusy = errors.New("a mute command is already in progress")

// DefaultKeyPath is where the API key is stored: $XDG_CONFIG_HOME/ts6tray/apikey,
// falling back to ~/.config/ts6tray/apikey.
func DefaultKeyPath() string {
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return filepath.Join(dir, "ts6tray", "apikey")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".config", "ts6tray", "apikey")
	}
	return filepath.Join(home, ".config", "ts6tray", "apikey")
}

// TSClient is the connection to the TeamSpeak client. It is safe for concurrent
// use. Run owns the connection and the State; every other method takes the lock.
type TSClient struct {
	addr    string
	keyPath string
	updates chan struct{}
	// notices carries desktop-notification events out to the tray. It is
	// buffered and written non-blockingly: a tray that is not draining it must
	// never stall the reader goroutine, so a full buffer drops the notice.
	notices chan notice
	// changed wakes a SetMute waiting for a flag to flip. At most one such
	// waiter exists at a time, because press is taken with TryLock.
	changed chan struct{}

	// Tunables, overridden by tests.
	flipWait    time.Duration
	backoffBase time.Duration

	mu    sync.Mutex
	st    State
	r     roster // the notification model, reset with st on disconnect
	up    bool
	conn  *websocket.Conn
	key   string // last key we authenticated with / were given
	press sync.Mutex
}

// NewTSClient returns a client for addr ("127.0.0.1:5899") storing its API key
// at keyPath. It does not connect; call Run.
func NewTSClient(addr, keyPath string) *TSClient {
	return &TSClient{
		addr:        addr,
		keyPath:     keyPath,
		updates:     make(chan struct{}, 1),
		notices:     make(chan notice, 32),
		changed:     make(chan struct{}, 1),
		flipWait:    tsDefaultFlipWait,
		backoffBase: tsDefaultBackoffBase,
	}
}

// Updates fires (coalesced, never blocking) whenever the state changed or the
// connection went up or down.
func (c *TSClient) Updates() <-chan struct{} { return c.updates }

// Notices carries what the user should be notified about (see notify.go). It is
// buffered; notices are dropped rather than queued when nobody drains it.
func (c *TSClient) Notices() <-chan notice { return c.notices }

// pushNotices queues notices without ever blocking the reader goroutine.
func (c *TSClient) pushNotices(ns []notice) {
	for _, n := range ns {
		select {
		case c.notices <- n:
		default:
			return // buffer full: drop the rest, they are only notifications
		}
	}
}

// Snapshot returns the live connections and the icon to show. up is false while
// the client is not connected and authenticated; conns is then nil.
func (c *TSClient) Snapshot() (conns []Conn, icon Icon, up bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.up {
		return nil, IconNone, false
	}
	return c.st.Conns(), c.st.Icon(), true
}

func (c *TSClient) notify() {
	select {
	case c.updates <- struct{}{}:
	default:
	}
	select {
	case c.changed <- struct{}{}:
	default:
	}
}

// --- run loop -------------------------------------------------------------

// Run connects, authenticates and reads until ctx is done, reconnecting with
// exponential backoff (1s doubling to 10s, reset after a successful auth).
func (c *TSClient) Run(ctx context.Context) {
	backoff := c.backoffBase
	if backoff <= 0 {
		backoff = tsDefaultBackoffBase
	}
	// askApproval makes the next auth use an empty key, which is what asks
	// TeamSpeak to show the approval prompt. It stays set until an auth is
	// accepted, so a declined prompt keeps asking (at most once per backoff,
	// capped at tsMaxBackoff) instead of falling back to the rejected key.
	askApproval := false
	for ctx.Err() == nil {
		key := c.storedKey()
		if askApproval {
			key = ""
		}
		authed, err := c.session(ctx, key)
		if ctx.Err() != nil {
			return
		}
		if !authed && key != "" && errors.Is(err, errAuthRejected) {
			// Explicit rejection of a real key: forget it and re-ask now.
			c.mu.Lock()
			c.key = ""
			c.mu.Unlock()
			askApproval = true
			log.Printf("ts6tray: TeamSpeak rejected the stored key; requesting approval again — accept %q in TeamSpeak", tsAppName)
			continue
		}
		if authed {
			askApproval = false
			backoff = c.backoffBase
			if backoff <= 0 {
				backoff = tsDefaultBackoffBase
			}
			log.Printf("ts6tray: TeamSpeak connection lost: %v", err)
		} else if err != nil {
			log.Printf("ts6tray: TeamSpeak unavailable: %v", err)
		}
		t := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		if backoff *= 2; backoff > tsMaxBackoff {
			backoff = tsMaxBackoff
		}
	}
}

// session runs one connection from dial to close. authed reports whether the
// auth reply was accepted, which is what resets the backoff. key is the API key
// to authenticate with; an empty key asks the user for approval.
func (c *TSClient) session(ctx context.Context, key string) (authed bool, err error) {
	dialCtx, cancel := context.WithTimeout(ctx, tsDialTimeout)
	conn, _, err := websocket.Dial(dialCtx, (&url.URL{Scheme: "ws", Host: c.addr}).String(), nil)
	cancel()
	if err != nil {
		return false, err
	}
	conn.SetReadLimit(tsReadLimit)
	defer conn.CloseNow()

	if key == "" {
		log.Printf("ts6tray: no API key yet, waiting for approval in the TeamSpeak client")
	}
	if err := tsWriteJSON(ctx, conn, tsAuthMessage(key)); err != nil {
		return false, fmt.Errorf("sending auth: %w", err)
	}

	// The reply may take arbitrarily long: with an empty key it arrives only
	// after the user approves, so there is deliberately no timeout here.
	newKey, err := c.readAuthReply(ctx, conn)
	if err != nil {
		return false, err
	}
	if newKey != "" && newKey != key {
		if err := c.saveKey(newKey); err != nil {
			log.Printf("ts6tray: could not save API key: %v", err)
		}
	}
	log.Printf("ts6tray: connected to TeamSpeak at %s", c.addr)

	c.mu.Lock()
	c.up = true
	c.conn = conn
	if newKey != "" {
		c.key = newKey
	}
	c.mu.Unlock()
	c.notify()

	defer func() {
		c.mu.Lock()
		c.up = false
		c.conn = nil
		c.st = State{}
		c.r = roster{}
		c.mu.Unlock()
		c.notify()
	}()

	for {
		// One reader, connection-lifetime context: a per-read deadline would
		// close the connection (see the note at the top).
		_, data, err := conn.Read(ctx)
		if err != nil {
			return true, err
		}
		c.mu.Lock()
		changed := c.st.Apply(data)
		ns := c.r.apply(data)
		c.mu.Unlock()
		c.pushNotices(ns)
		if changed {
			c.notify()
		}
	}
}

// readAuthReply consumes messages until the auth reply arrives, applies it to
// the state and returns the key TeamSpeak handed back.
func (c *TSClient) readAuthReply(ctx context.Context, conn *websocket.Conn) (string, error) {
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			// A close here is not proof of a revoked key — TeamSpeak quitting
			// looks the same — so this never drops the stored key; only an
			// explicit status.code != 0 does. Just back off and retry.
			return "", fmt.Errorf("waiting for auth reply: %w", err)
		}
		var env struct {
			Type   string `json:"type"`
			Status *struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"status"`
			Payload json.RawMessage `json:"payload"`
		}
		if err := json.Unmarshal(data, &env); err != nil || env.Type != "auth" {
			continue
		}
		if env.Status != nil && env.Status.Code != 0 {
			// The caller turns this into a fresh approval request when the key
			// we used was not empty.
			return "", fmt.Errorf("%w (code %d: %s)", errAuthRejected, env.Status.Code, env.Status.Message)
		}
		var pl struct {
			APIKey string `json:"apiKey"`
		}
		_ = json.Unmarshal(env.Payload, &pl)
		c.mu.Lock()
		err = c.st.ApplyAuth(data)
		if err == nil {
			// Same payload, seeded silently: the snapshot is the baseline the
			// events are diffed against, not news.
			c.r = roster{}
			c.r.auth(env.Payload)
		}
		c.mu.Unlock()
		if err != nil {
			return "", fmt.Errorf("parsing auth reply: %w", err)
		}
		return pl.APIKey, nil
	}
}

// --- wire helpers ---------------------------------------------------------

func tsAuthMessage(key string) any {
	return map[string]any{
		"type": "auth",
		"payload": map[string]any{
			"identifier":  tsIdentifier,
			"version":     tsAppVersion,
			"name":        tsAppName,
			"description": tsDescription,
			"content":     map[string]any{"apiKey": key},
		},
	}
}

func tsButtonMessage(button string, down bool) any {
	return map[string]any{
		"type":    "buttonPress",
		"payload": map[string]any{"button": button, "state": down},
	}
}

func tsWriteJSON(ctx context.Context, conn *websocket.Conn, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, tsWriteTimeout)
	defer cancel()
	return conn.Write(wctx, websocket.MessageText, b)
}

// --- key storage ----------------------------------------------------------

func (c *TSClient) storedKey() string {
	c.mu.Lock()
	key := c.key
	c.mu.Unlock()
	if key != "" {
		return key
	}
	return tsReadKey(c.keyPath)
}

// tsReadKey reads the API key file, trimming whitespace. A missing or
// unreadable file means "no key yet".
func tsReadKey(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// saveKey writes the key with dir 0700 / file 0600. MkdirAll and WriteFile only
// apply their mode to something they create, so an already existing loose dir or
// key file (e.g. from an older version, or a rotated key landing in a 0644 file)
// is tightened explicitly afterwards — the same as tools/probe does.
func (c *TSClient) saveKey(key string) error {
	if c.keyPath == "" {
		return nil
	}
	dir := filepath.Dir(c.keyPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(c.keyPath, []byte(key+"\n"), 0o600); err != nil {
		return err
	}
	return os.Chmod(c.keyPath, 0o600)
}

// --- mute control ---------------------------------------------------------

// SetMute presses the mic or speaker mute button. target is "mic" or "speaker",
// mode is "toggle", "mute" or "unmute". It reports whether the flag actually
// changed. For a "mute"/"unmute" that is already satisfied it sends nothing and
// returns (false, nil). If the press produces no flag change within flipWait it
// returns an error wrapping ErrNotBound. While another SetMute is in flight it
// returns ErrBusy without pressing anything.
func (c *TSClient) SetMute(target, mode string) (changed bool, err error) {
	var button, flag, action, bind string
	switch target {
	case "mic":
		button, flag, action, bind = ButtonMic, "inputMuted", "Toggle microphone mute", "Bind microphone key"
	case "speaker":
		button, flag, action, bind = ButtonSpeaker, "outputMuted", "Toggle speaker mute", "Bind speaker key"
	default:
		return false, fmt.Errorf("unknown mute target %q: want \"mic\" or \"speaker\"", target)
	}
	switch mode {
	case "toggle", "mute", "unmute":
	default:
		return false, fmt.Errorf("unknown mute mode %q: want \"toggle\", \"mute\" or \"unmute\"", mode)
	}

	// One press at a time, so two clicks cannot interleave. A press that is
	// still waiting for its flag to flip holds this for up to flipWait, so
	// rapid clicks are dropped rather than queued: otherwise eight clicks on an
	// unbound button would keep firing presses for ~12s after the last click.
	if !c.press.TryLock() {
		return false, ErrBusy
	}
	defer c.press.Unlock()

	c.mu.Lock()
	up, conn := c.up, c.conn
	var conns []Conn
	var target0 Conn
	if up {
		conns = c.st.Conns()
		if a, ok := c.st.Active(); ok {
			target0 = a
		} else if len(conns) > 0 {
			target0 = conns[0]
		}
	}
	c.mu.Unlock()
	if !up || conn == nil || len(conns) == 0 {
		return false, ErrNotConnected
	}

	cur := tsMuteFlag(target0, target)
	if mode != "toggle" && cur == (mode == "mute") {
		return false, nil
	}

	// Remember every live connection's flag, because the TeamSpeak hotkey may
	// act on a different server tab than the one we looked at.
	before := make(map[int]bool, len(conns))
	for _, cn := range conns {
		before[cn.ID] = tsMuteFlag(cn, target)
	}

	if err := tsSendPress(conn, button); err != nil {
		return false, err
	}

	wait := c.flipTimeout()
	if c.waitFlip(target, before) {
		return true, nil
	}
	return false, fmt.Errorf("TeamSpeak accepted the %s press but %s did not change within %s. "+
		"The button has to be bound once: run `ts6tray settings` and choose %q, then switch to TeamSpeak, "+
		"open Settings -> Key Bindings and start the hotkey assignment for the %q action within 5 seconds "+
		"— ts6tray presses the key and TeamSpeak records it: %w",
		button, flag, wait, bind, action, ErrNotBound)
}

// Press sends one buttonPress down+up for button (ButtonMic or ButtonSpeaker)
// without checking or waiting for any state change. It is what the tray's bind
// helper uses to let TeamSpeak record the button during hotkey assignment. It
// takes the same press lock as SetMute, so a bind press cannot race a toggle:
// while one is in flight it returns ErrBusy. ErrNotConnected means TS6 is down.
func (c *TSClient) Press(button string) error {
	if !c.press.TryLock() {
		return ErrBusy
	}
	defer c.press.Unlock()

	c.mu.Lock()
	up, conn := c.up, c.conn
	c.mu.Unlock()
	if !up || conn == nil {
		return ErrNotConnected
	}
	return tsSendPress(conn, button)
}

// tsSendPress writes one down+up buttonPress pair. Callers hold c.press.
func tsSendPress(conn *websocket.Conn, button string) error {
	ctx := context.Background()
	if err := tsWriteJSON(ctx, conn, tsButtonMessage(button, true)); err != nil {
		return fmt.Errorf("%w: sending button press: %v", ErrNotConnected, err)
	}
	if err := tsWriteJSON(ctx, conn, tsButtonMessage(button, false)); err != nil {
		return fmt.Errorf("%w: sending button release: %v", ErrNotConnected, err)
	}
	return nil
}

func tsMuteFlag(c Conn, target string) bool {
	if target == "speaker" {
		return c.OutputMuted
	}
	return c.InputMuted
}

// flipTimeout is how long to wait for a press to show up as a flag change.
func (c *TSClient) flipTimeout() time.Duration {
	if c.flipWait > 0 {
		return c.flipWait
	}
	return tsDefaultFlipWait
}

// waitFlip waits until any live connection's flag differs from what it was
// before the press, or the wait expires. It does not poll: every state change
// and every connection up/down already signals c.changed, and only one SetMute
// can be in here at a time (press is taken with TryLock), so a single-slot
// channel is enough. A stale signal only costs one extra re-check.
func (c *TSClient) waitFlip(target string, before map[int]bool) bool {
	t := time.NewTimer(c.flipTimeout())
	defer t.Stop()
	for {
		if c.flipped(target, before) {
			return true
		}
		select {
		case <-c.changed:
		case <-t.C:
			// Check once more: the change may have landed just before the
			// timer fired.
			return c.flipped(target, before)
		}
	}
}

// flipped reports whether any live connection's flag differs from before.
func (c *TSClient) flipped(target string, before map[int]bool) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.up {
		return false
	}
	for _, cn := range c.st.Conns() {
		if old, known := before[cn.ID]; known && tsMuteFlag(cn, target) != old {
			return true
		}
	}
	return false
}
