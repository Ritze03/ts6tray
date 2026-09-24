package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// redactedValue replaces every secret we find in a fixture.
const redactedValue = "REDACTED"

// secretKeys are JSON object keys whose *string* value is a secret.
//
//   - apiKey      — our own Remote Apps key (auth reply).
//   - myts_token  — a MyTeamSpeak bearer token. It does not appear as a
//     top-level field: it lives inside the JSON-encoded *string* held by a
//     client's `metaData` and `userTag` properties, so it is only reachable
//     after parsing that string (see scrubString).
var secretKeys = map[string]bool{
	"apiKey":     true,
	"myts_token": true,
}

// mytsTokenRe is the fallback for a `metaData` / `userTag` string that carries
// a token but does not parse as JSON (truncated, or a future format change).
// It rewrites just the token value and leaves the rest of the text alone.
var mytsTokenRe = regexp.MustCompile(`("myts_token"\s*:\s*")(?:[^"\\]|\\.)*(")`)

// decodeJSON unmarshals with UseNumber so that large integers survive a
// decode/encode round trip exactly instead of turning into float64.
func decodeJSON(b []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	return d.Decode(v)
}

type scrubber struct{ n int }

// walk replaces every secret in the tree, in place, keeping the structure
// (key sets, types, array order) untouched so the fixtures still parse into
// whatever the state tests expect.
func (s *scrubber) walk(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, sub := range t {
			if secretKeys[k] {
				if _, ok := sub.(string); ok {
					t[k] = redactedValue
					s.n++
					continue
				}
			}
			t[k] = s.walk(sub)
		}
		return t
	case []any:
		for i := range t {
			t[i] = s.walk(t[i])
		}
		return t
	case string:
		return s.walkString(t)
	}
	return v
}

// walkString handles JSON-in-a-string: TeamSpeak stores a client's metaData
// and userTag as a JSON document encoded into a single string value, e.g.
//
//	"metaData": "{\"myts_token\":\"CkCaVW…\",\"tag\":\"x@myteamspeak.com\",\"updated\":1790159120400}"
//
// so a plain top-level key match never sees the token. Parse it, scrub it,
// and re-encode it as a string.
func (s *scrubber) walkString(str string) string {
	if !strings.Contains(str, "myts_token") {
		return str
	}
	var inner any
	if err := decodeJSON([]byte(str), &inner); err == nil {
		before := s.n
		inner = s.walk(inner)
		if b, err := json.Marshal(inner); err == nil {
			return string(b)
		}
		s.n = before
	}
	out := mytsTokenRe.ReplaceAllString(str, `${1}`+redactedValue+`${2}`)
	if out != str {
		s.n++
	}
	return out
}

// scrub returns v with every secret replaced, plus how many were replaced.
func scrub(v any) (any, int) {
	var s scrubber
	return s.walk(v), s.n
}

// scrubBytes scrubs one raw JSON message and returns it compacted.
func scrubBytes(raw []byte) ([]byte, error) {
	var v any
	if err := decodeJSON(raw, &v); err != nil {
		return nil, err
	}
	v, _ = scrub(v)
	return json.Marshal(v)
}

// writeScrubbedJSON pretty-prints one raw JSON message to path, scrubbed.
func writeScrubbedJSON(path string, raw []byte) error {
	var v any
	if err := decodeJSON(raw, &v); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	v, _ = scrub(v)
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(out, '\n'), 0o644)
}

// scrubInPlace rewrites an existing fixture with every secret replaced.
// A .jsonl file is handled line by line; anything else as a single document.
func scrubInPlace(path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	var out []byte
	var n int

	if filepath.Ext(path) == ".jsonl" {
		var buf bytes.Buffer
		for i, line := range bytes.Split(bytes.TrimRight(b, "\n"), []byte("\n")) {
			if len(bytes.TrimSpace(line)) == 0 {
				continue
			}
			var v any
			if err := decodeJSON(line, &v); err != nil {
				return fmt.Errorf("%s line %d: %w", path, i+1, err)
			}
			var k int
			v, k = scrub(v)
			n += k
			enc, err := json.Marshal(v)
			if err != nil {
				return err
			}
			buf.Write(enc)
			buf.WriteByte('\n')
		}
		out = buf.Bytes()
	} else {
		var v any
		if err := decodeJSON(b, &v); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		v, n = scrub(v)
		enc, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			return err
		}
		out = append(enc, '\n')
	}

	if err := os.WriteFile(path, out, 0o644); err != nil {
		return err
	}
	fmt.Printf("scrubbed %s: %d secret(s) replaced\n", path, n)
	return nil
}
