package main

import (
	"encoding/json"
	"strings"
	"testing"
)

const fakeToken = "FAKETOKENfakeTOKENfakeTOKENfakeTOKENfakeTOKENfakeTOKEN000000"

func scrubJSON(t *testing.T, in string) (string, int) {
	t.Helper()
	var v any
	if err := decodeJSON([]byte(in), &v); err != nil {
		t.Fatalf("decode: %v", err)
	}
	v, n := scrub(v)
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b), n
}

func TestScrubAPIKey(t *testing.T) {
	// Not a real key — never put the live one in a test fixture.
	out, n := scrubJSON(t, `{"type":"auth","payload":{"apiKey":"00000000-1111-2222-3333-444444444444","currentConnectionId":0}}`)
	if n != 1 {
		t.Errorf("want 1 secret, got %d", n)
	}
	if strings.Contains(out, "00000000-1111") {
		t.Errorf("api key survived: %s", out)
	}
	if !strings.Contains(out, `"apiKey":"REDACTED"`) {
		t.Errorf("want redacted apiKey, got %s", out)
	}
	if !strings.Contains(out, `"currentConnectionId":0`) {
		t.Errorf("unrelated field changed: %s", out)
	}
}

// The token lives inside a JSON document encoded into a single string value,
// so a top-level key match is not enough.
func TestScrubTokenInsideMetaDataAndUserTag(t *testing.T) {
	for _, field := range []string{"metaData", "userTag"} {
		inner := `{\"myts_token\":\"` + fakeToken + `\",\"tag\":\"someone@myteamspeak.com\",\"updated\":1790159120400}`
		in := `{"properties":{"` + field + `":"` + inner + `","nickname":"Ritze"}}`

		out, n := scrubJSON(t, in)
		if n != 1 {
			t.Errorf("%s: want 1 secret, got %d", field, n)
		}
		if strings.Contains(out, fakeToken) {
			t.Errorf("%s: token survived: %s", field, out)
		}
		if !strings.Contains(out, `myts_token`) || !strings.Contains(out, `REDACTED`) {
			t.Errorf("%s: structure lost: %s", field, out)
		}
		// Everything else in the embedded document must survive intact,
		// including the timestamp, which must not become a float.
		for _, want := range []string{"someone@myteamspeak.com", "1790159120400", `"nickname":"Ritze"`} {
			if !strings.Contains(out, want) {
				t.Errorf("%s: lost %q: %s", field, want, out)
			}
		}
	}
}

// clientSelfPropertyUpdated carries the same document as oldValue/newValue.
func TestScrubTokenInSelfPropertyEvent(t *testing.T) {
	inner := `{\"myts_token\":\"` + fakeToken + `\",\"tag\":\"x@myteamspeak.com\"}`
	in := `{"type":"clientSelfPropertyUpdated","payload":{"connectionId":1,"flag":"userTag","oldValue":"","newValue":"` + inner + `"}}`
	out, n := scrubJSON(t, in)
	if n != 1 {
		t.Errorf("want 1 secret, got %d", n)
	}
	if strings.Contains(out, fakeToken) {
		t.Errorf("token survived: %s", out)
	}
	if !strings.Contains(out, `"flag":"userTag"`) {
		t.Errorf("payload mangled: %s", out)
	}
}

// A metaData string that does not parse as JSON must still lose its token.
func TestScrubTokenFallbackUnparseable(t *testing.T) {
	in := `{"properties":{"metaData":"{\"myts_token\":\"` + fakeToken + `\",\"tag\":\"trunc"}}`
	out, n := scrubJSON(t, in)
	if n != 1 {
		t.Errorf("want 1 secret via fallback, got %d", n)
	}
	if strings.Contains(out, fakeToken) {
		t.Errorf("token survived the fallback: %s", out)
	}
}

func TestScrubLeavesCleanDataAlone(t *testing.T) {
	in := `{"payload":{"clientId":18,"connectionId":1,"properties":{"inputMuted":true,"inputHardware":false,"nickname":"Ritze","metaData":"","totalBytesDownloaded":"30935430236","created":1775677654}}}`
	out, n := scrubJSON(t, in)
	if n != 0 {
		t.Errorf("want 0 secrets, got %d", n)
	}
	for _, want := range []string{`"inputMuted":true`, `"inputHardware":false`, `"totalBytesDownloaded":"30935430236"`, `"created":1775677654`} {
		if !strings.Contains(out, want) {
			t.Errorf("lost %q: %s", want, out)
		}
	}
}

// Large integers must not be rewritten in exponent form by the round trip.
func TestScrubPreservesLargeNumbers(t *testing.T) {
	in := `{"a":18446744073709551615,"b":1790159120400,"c":1.5,"d":-18}`
	out, n := scrubJSON(t, in)
	if n != 0 {
		t.Errorf("want 0 secrets, got %d", n)
	}
	if out != in {
		t.Errorf("numbers changed:\n in: %s\nout: %s", in, out)
	}
}
