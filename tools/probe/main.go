// Command probe is a one-shot protocol probe for the TeamSpeak 6 Remote Apps
// WebSocket API. It authenticates as the ts6tray identity, persists the API
// key, and captures raw protocol traffic as test fixtures.
//
// It is a development tool, not part of the ts6tray daemon.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/coder/websocket"
)

const (
	wsURL = "ws://127.0.0.1:5899"

	// Identity. The daemon MUST send an identical identity or the stored
	// API key will not be accepted.
	appIdentifier  = "ts6tray"
	appName        = "ts6tray"
	appDescription = "Tray icon and mute control for TeamSpeak 6"
	appVersion     = "0.1.0"
)

type authPayload struct {
	Identifier  string      `json:"identifier"`
	Version     string      `json:"version"`
	Name        string      `json:"name"`
	Description string      `json:"description"`
	Content     authContent `json:"content"`
}

type authContent struct {
	APIKey string `json:"apiKey"`
}

type envelope struct {
	Type    string `json:"type"`
	Payload any    `json:"payload"`
}

var (
	outDir      = flag.String("out", "testdata", "directory for fixtures")
	captureSecs = flag.Int("capture", 180, "seconds to record events after cached auth")
	authWait    = flag.Duration("auth-wait", 5*time.Minute, "how long to wait for the user to approve in TS6")
	scrubPaths  = flag.String("scrub", "", "comma-separated existing fixture files to scrub in place, then exit")
)

func main() {
	flag.Parse()
	if *scrubPaths != "" {
		for _, p := range strings.Split(*scrubPaths, ",") {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			if err := scrubInPlace(p); err != nil {
				fmt.Fprintln(os.Stderr, "probe: "+err.Error())
				os.Exit(1)
			}
		}
		return
	}
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "probe: "+err.Error())
		os.Exit(1)
	}
}

func keyPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "ts6tray", "apikey"), nil
}

func loadKey() (string, error) {
	p, err := keyPath()
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return string(trimSpace(b)), nil
}

func trimSpace(b []byte) []byte {
	i, j := 0, len(b)
	for i < j && (b[i] == ' ' || b[i] == '\n' || b[i] == '\t' || b[i] == '\r') {
		i++
	}
	for j > i && (b[j-1] == ' ' || b[j-1] == '\n' || b[j-1] == '\t' || b[j-1] == '\r') {
		j--
	}
	return b[i:j]
}

func saveKey(k string) (string, error) {
	p, err := keyPath()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return "", err
	}
	if err := os.Chmod(filepath.Dir(p), 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(p, []byte(k+"\n"), 0o600); err != nil {
		return "", err
	}
	return p, os.Chmod(p, 0o600)
}

// Secret redaction lives in scrub.go: every fixture write goes through
// writeScrubbedJSON or scrubBytes, which strip the apiKey AND the MyTeamSpeak
// bearer tokens buried inside the metaData / userTag JSON-in-a-string values.

type conn struct {
	c *websocket.Conn
}

func dial(ctx context.Context) (*conn, error) {
	c, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", wsURL, err)
	}
	c.SetReadLimit(32 << 20)
	return &conn{c: c}, nil
}

func (c *conn) close() { _ = c.c.Close(websocket.StatusNormalClosure, "") }

func (c *conn) send(ctx context.Context, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	fmt.Printf(">> %s\n", b)
	return c.c.Write(ctx, websocket.MessageText, b)
}

func (c *conn) read(ctx context.Context) ([]byte, error) {
	_, b, err := c.c.Read(ctx)
	return b, err
}

func (c *conn) sendAuth(ctx context.Context, key string) error {
	return c.send(ctx, envelope{Type: "auth", Payload: authPayload{
		Identifier:  appIdentifier,
		Version:     appVersion,
		Name:        appName,
		Description: appDescription,
		Content:     authContent{APIKey: key},
	}})
}

// typeOf returns the "type" field of a raw message, or "" .
func typeOf(raw []byte) string {
	var m struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(raw, &m)
	return m.Type
}

func apiKeyOf(raw []byte) string {
	var m struct {
		Payload struct {
			APIKey string `json:"apiKey"`
		} `json:"payload"`
	}
	_ = json.Unmarshal(raw, &m)
	return m.Payload.APIKey
}

// waitAuth reads until an "auth" message arrives, logging anything else.
func (c *conn) waitAuth(ctx context.Context) ([]byte, [][]byte, error) {
	var pre [][]byte
	for {
		raw, err := c.read(ctx)
		if err != nil {
			return nil, pre, err
		}
		t := typeOf(raw)
		fmt.Printf("<< [%s] %d bytes\n", t, len(raw))
		if t == "auth" {
			return raw, pre, nil
		}
		pre = append(pre, raw)
	}
}

type record struct {
	Ts   string          `json:"ts"`
	Type string          `json:"type"`
	Raw  json.RawMessage `json:"raw"`
}

func run() error {
	ctx := context.Background()

	existing, err := loadKey()
	if err != nil {
		return err
	}

	key := existing
	if existing == "" {
		fmt.Println("== phase 1: first-time auth (empty apiKey). Approve 'ts6tray' in the TeamSpeak window.")
		ctx1, cancel := context.WithTimeout(ctx, *authWait)
		defer cancel()
		c, err := dial(ctx1)
		if err != nil {
			return err
		}
		if err := c.sendAuth(ctx1, ""); err != nil {
			return err
		}
		raw, _, err := c.waitAuth(ctx1)
		if err != nil {
			c.close()
			return fmt.Errorf("waiting for approval: %w", err)
		}
		key = apiKeyOf(raw)
		if key == "" {
			c.close()
			return fmt.Errorf("auth reply carried no apiKey: %s", truncate(raw, 500))
		}
		p, err := saveKey(key)
		if err != nil {
			c.close()
			return err
		}
		fmt.Printf("== saved API key to %s\n", p)
		if err := writeScrubbedJSON(filepath.Join(*outDir, "auth_full.json"), raw); err != nil {
			c.close()
			return err
		}
		fmt.Printf("== wrote auth_full.json (%d bytes raw)\n", len(raw))
		c.close()
		time.Sleep(2 * time.Second)
	} else {
		fmt.Println("== existing key found; skipping first-time auth (auth_full.json not regenerated)")
	}

	fmt.Println("== phase 2: reconnect with cached key")
	c, err := dial(ctx)
	if err != nil {
		return err
	}
	defer c.close()

	ctx2, cancel2 := context.WithTimeout(ctx, 60*time.Second)
	defer cancel2()
	if err := c.sendAuth(ctx2, key); err != nil {
		return err
	}
	cachedRaw, preAuth, err := c.waitAuth(ctx2)
	if err != nil {
		return fmt.Errorf("cached auth: %w", err)
	}
	if err := writeScrubbedJSON(filepath.Join(*outDir, "auth_cached.json"), cachedRaw); err != nil {
		return err
	}
	fmt.Printf("== wrote auth_cached.json (%d bytes raw)\n", len(cachedRaw))

	if *captureSecs <= 0 {
		fmt.Println("== phase 3: skipped (-capture <= 0); events.jsonl left untouched")
		c.close()
		return inputPhase(ctx, key)
	}

	fmt.Printf("== phase 3: recording events for %ds. Do your mic/speaker/talk actions now.\n", *captureSecs)
	// O_EXCL: never silently destroy an existing capture.
	f, err := os.OpenFile(filepath.Join(*outDir, "events.jsonl"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return fmt.Errorf("events.jsonl: %w (move or delete it to re-capture)", err)
	}
	enc := json.NewEncoder(f)
	counts := map[string]int{}

	writeRec := func(raw []byte) error {
		red, err := scrubBytes(raw)
		if err != nil {
			return err
		}
		t := typeOf(raw)
		counts[t]++
		return enc.Encode(record{Ts: time.Now().UTC().Format(time.RFC3339Nano), Type: t, Raw: red})
	}

	// Anything that arrived before the cached auth reply belongs in the log too.
	for _, raw := range preAuth {
		if err := writeRec(raw); err != nil {
			return err
		}
	}

	deadline := time.Now().Add(time.Duration(*captureSecs) * time.Second)
	for time.Now().Before(deadline) {
		rctx, rcancel := context.WithDeadline(ctx, deadline)
		raw, err := c.read(rctx)
		rcancel()
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				break
			}
			f.Close()
			return fmt.Errorf("read during capture: %w", err)
		}
		t := typeOf(raw)
		fmt.Printf("<< [%s] %s\n", t, truncate(raw, 220))
		if err := writeRec(raw); err != nil {
			f.Close()
			return err
		}
	}
	if err := f.Close(); err != nil {
		return err
	}
	fmt.Println("== event counts:")
	for t, n := range counts {
		fmt.Printf("   %-28s %d\n", t, n)
	}

	// NOTE: coder/websocket closes the underlying connection when a Read's
	// context is cancelled, so the capture connection is dead by now. Each
	// input test therefore runs on its own fresh connection.
	c.close()
	return inputPhase(ctx, key)
}

func inputPhase(ctx context.Context, key string) error {
	fmt.Println("== phase 4: input test on unbound id ts6tray.probe")
	results := map[string]any{}
	// "ts6trayBogus" is a control: it is certainly not a valid message type,
	// so its result shows whether the client answers unknown types at all.
	for _, kind := range []string{"keyPress", "buttonPress", "ts6trayBogus"} {
		res, err := inputTest(ctx, key, kind)
		if err != nil {
			res = map[string]any{"error": err.Error()}
		}
		results[kind] = res
	}
	// Route the whole document through the scrub as well, so an error string
	// or a future field cannot smuggle a secret past the per-message scrub.
	raw, err := json.Marshal(results)
	if err != nil {
		return err
	}
	if err := writeScrubbedJSON(filepath.Join(*outDir, "input_test.json"), raw); err != nil {
		return err
	}
	fmt.Println("== wrote input_test.json; done")
	return nil
}

// inputTest opens a fresh authenticated connection, sends a press+release of
// the given message type on the unbound button id ts6tray.probe, and collects
// everything the client sends back within 3 seconds.
func inputTest(ctx context.Context, key, kind string) (map[string]any, error) {
	c, err := dial(ctx)
	if err != nil {
		return nil, err
	}
	defer c.close()

	actx, acancel := context.WithTimeout(ctx, 60*time.Second)
	if err := c.sendAuth(actx, key); err != nil {
		acancel()
		return nil, err
	}
	if _, _, err := c.waitAuth(actx); err != nil {
		acancel()
		return nil, fmt.Errorf("auth before %s: %w", kind, err)
	}
	acancel()

	var sent []any
	for _, state := range []bool{true, false} {
		p := map[string]any{"button": "ts6tray.probe", "state": state}
		ictx, icancel := context.WithTimeout(ctx, 5*time.Second)
		err := c.send(ictx, envelope{Type: kind, Payload: p})
		icancel()
		if err != nil {
			return map[string]any{"sendError": err.Error(), "sent": sent}, nil
		}
		sent = append(sent, map[string]any{"type": kind, "payload": p})
	}

	got := []json.RawMessage{}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		rctx, rcancel := context.WithDeadline(ctx, deadline)
		raw, err := c.read(rctx)
		rcancel()
		if err != nil {
			break
		}
		fmt.Printf("<< [%s] %s\n", typeOf(raw), truncate(raw, 300))
		b, err := scrubBytes(raw)
		if err != nil {
			continue
		}
		got = append(got, b)
	}
	return map[string]any{"sent": sent, "responses": got}, nil
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + fmt.Sprintf("...(+%d bytes)", len(b)-n)
}
