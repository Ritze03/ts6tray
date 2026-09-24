package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// --- helpers --------------------------------------------------------------

// fixture reads testdata/<name>. Captures under testdata/ hold real users'
// nicknames and are not part of the repo, so a missing fixture skips the test
// instead of failing it.
func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if os.IsNotExist(err) {
		t.Skipf("fixture %s not present (testdata/ is not in the repo)", name)
	}
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return b
}

// authPayloadBytes returns payload of testdata/auth_full.json.
func authPayloadBytes(t *testing.T) []byte {
	t.Helper()
	b := fixture(t, "auth_full.json")
	var env struct {
		Payload json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(b, &env); err != nil {
		t.Fatalf("parse auth fixture: %v", err)
	}
	if len(env.Payload) == 0 {
		t.Fatal("auth fixture has no payload")
	}
	return env.Payload
}

func freshState(t *testing.T) *State {
	t.Helper()
	s := &State{}
	if err := s.ApplyAuth(authPayloadBytes(t)); err != nil {
		t.Fatalf("ApplyAuth: %v", err)
	}
	return s
}

// salvageLines returns the raw envelopes from events_run1_salvage.jsonl. Each
// line of that file is {"raw": <envelope>, "type": ...}; the envelope is what
// came off the wire.
func salvageLines(t *testing.T) []json.RawMessage {
	t.Helper()
	return captureLines(t, "events_run1_salvage.jsonl")
}

// captureLines returns the raw envelopes of any probe capture file under
// testdata/ (JSONL, one {"ts","type","raw"} record per line).
func captureLines(t *testing.T, name string) []json.RawMessage {
	t.Helper()
	var out []json.RawMessage
	sc := bufio.NewScanner(bytes.NewReader(fixture(t, name)))
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		if len(sc.Bytes()) == 0 {
			continue
		}
		var rec struct {
			Raw json.RawMessage `json:"raw"`
		}
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			t.Fatalf("parse salvage line %d: %v", len(out)+1, err)
		}
		out = append(out, rec.Raw)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan salvage fixture: %v", err)
	}
	return out
}

func mustActive(t *testing.T, s *State, wantID int) Conn {
	t.Helper()
	c, ok := s.Active()
	if !ok {
		t.Fatalf("Active(): no active connection, want %d", wantID)
	}
	if c.ID != wantID {
		t.Fatalf("Active(): got conn %d, want %d", c.ID, wantID)
	}
	return c
}

// --- 1. auth snapshot -----------------------------------------------------

// Proves: ApplyAuth reads the real auth reply and yields exactly one live
// connection with the identity and flags that are literally in the fixture.
func TestApplyAuthFixture(t *testing.T) {
	s := freshState(t)

	conns := s.Conns()
	if len(conns) != 1 {
		t.Fatalf("Conns(): got %d live connections, want 1: %+v", len(conns), conns)
	}
	got := conns[0]
	want := Conn{
		ID:         1,
		ServerUID:  "ExampleServerAUid00000000000000000000000000=",
		ServerName: "Example Server A",
		ClientID:   18,
		Status:     StatusConnectionEstablished,
		// fixture flags: all false except inputHardware
		InputHardware: true,
	}
	if got != want {
		t.Errorf("Conns()[0]:\n got %+v\nwant %+v", got, want)
	}
	if got.MicDisabled() {
		t.Error("MicDisabled() = true, want false (inputHardware true, inputDeactivated false)")
	}
	mustActive(t, s, 1)
	if ic := s.Icon(); ic != IconQuiet {
		t.Errorf("Icon() = %v, want %v", ic, IconQuiet)
	}
}

// Proves: ApplyAuth resets rather than merges, and tolerates being handed the
// whole envelope instead of just the payload.
func TestApplyAuthResetsAndAcceptsEnvelope(t *testing.T) {
	s := freshState(t)
	s.conns[99] = &Conn{ID: 99, Status: StatusConnectionEstablished, InputHardware: true}

	whole := fixture(t, "auth_full.json")
	if err := s.ApplyAuth(whole); err != nil {
		t.Fatalf("ApplyAuth(envelope): %v", err)
	}
	if len(s.Conns()) != 1 || s.Conns()[0].ID != 1 {
		t.Fatalf("after re-auth: %+v, want just conn 1", s.Conns())
	}
}

// --- 2. replaying the real salvage capture --------------------------------

// Proves: replaying the real run-1 messages after the real auth snapshot
// reproduces every mute/talk/connect transition the file still contains.
// Line numbers are 1-based into testdata/events_run1_salvage.jsonl.
func TestReplaySalvage(t *testing.T) {
	s := freshState(t)
	lines := salvageLines(t)
	if len(lines) != 55 {
		t.Fatalf("salvage fixture has %d messages, expected 55 — fixture changed?", len(lines))
	}

	// wantChanged: line -> expected return of Apply.
	wantChanged := map[int]bool{
		1:  false, // talkStatusChanged for client 24, not us
		2:  false,
		3:  true,  // self flagTalking false -> true
		4:  false, // talkStatusChanged for us, status 1, already talking
		5:  true,  // self flagTalking true -> false
		7:  true,  // inputMuted true
		8:  true,  // inputMuted false
		9:  true,  // outputMuted true
		10: true,  // outputMuted false
		11: false, // conn 4 status 1 (Connecting), info null -> not tracked yet
		12: true,  // conn 4 status 3, info {clientId}
		13: false, // neededPermissions
		14: true,  // conn 4 status 4 -> live
		15: false, // clientMoved without properties
		16: false, // clientChannelGroupChanged
		18: false, // conn 1 inputHardware false->true, but it was already true
		19: false, // streamInfoReplace
		20: false, // clientMoved without properties
		21: true,  // conn 4 status 0 -> removed
	}

	check := map[int]func(t *testing.T, s *State){
		3: func(t *testing.T, s *State) {
			if ic := s.Icon(); ic != IconTalking {
				t.Errorf("line 3: Icon() = %v, want %v", ic, IconTalking)
			}
		},
		6: func(t *testing.T, s *State) {
			if ic := s.Icon(); ic != IconQuiet {
				t.Errorf("line 6: Icon() = %v, want %v", ic, IconQuiet)
			}
		},
		7: func(t *testing.T, s *State) {
			c := mustActive(t, s, 1)
			if !c.InputMuted {
				t.Error("line 7: InputMuted = false, want true")
			}
			if ic := s.Icon(); ic != IconMicMuted {
				t.Errorf("line 7: Icon() = %v, want %v", ic, IconMicMuted)
			}
		},
		8: func(t *testing.T, s *State) {
			if ic := s.Icon(); ic != IconQuiet {
				t.Errorf("line 8: Icon() = %v, want %v", ic, IconQuiet)
			}
		},
		9: func(t *testing.T, s *State) {
			c := mustActive(t, s, 1)
			if !c.OutputMuted {
				t.Error("line 9: OutputMuted = false, want true")
			}
			if ic := s.Icon(); ic != IconSpeakerMuted {
				t.Errorf("line 9: Icon() = %v, want %v", ic, IconSpeakerMuted)
			}
		},
		10: func(t *testing.T, s *State) {
			if ic := s.Icon(); ic != IconQuiet {
				t.Errorf("line 10: Icon() = %v, want %v", ic, IconQuiet)
			}
		},
		12: func(t *testing.T, s *State) {
			// status 3 = ConnectionEstablishing: known but not live yet.
			if n := len(s.Conns()); n != 1 {
				t.Errorf("line 12: %d live conns, want 1", n)
			}
			c := s.conns[4]
			if c == nil || c.ClientID != 63676 {
				t.Errorf("line 12: conn 4 = %+v, want ClientID 63676", c)
			}
		},
		14: func(t *testing.T, s *State) {
			conns := s.Conns()
			if len(conns) != 2 || conns[0].ID != 1 || conns[1].ID != 4 {
				t.Fatalf("line 14: Conns() = %+v, want live conns 1 and 4 in that order", conns)
			}
			// The salvage lost the status-2 message, so conn 4 has no server
			// identity and (having received no properties) no capture device.
			if !conns[1].MicDisabled() {
				t.Error("line 14: conn 4 MicDisabled() = false, want true")
			}
			mustActive(t, s, 1)
			if ic := s.Icon(); ic != IconQuiet {
				t.Errorf("line 14: Icon() = %v, want %v", ic, IconQuiet)
			}
		},
		21: func(t *testing.T, s *State) {
			if n := len(s.Conns()); n != 1 {
				t.Errorf("line 21: %d live conns after disconnect, want 1", n)
			}
			if _, ok := s.conns[4]; ok {
				t.Error("line 21: conn 4 still present after status 0")
			}
		},
		52: func(t *testing.T, s *State) {
			if ic := s.Icon(); ic != IconTalking {
				t.Errorf("line 52: Icon() = %v, want %v", ic, IconTalking)
			}
		},
	}

	for i, raw := range lines {
		n := i + 1
		changed := s.Apply(raw)
		if want, ok := wantChanged[n]; ok && changed != want {
			t.Errorf("line %d: Apply() changed = %v, want %v (%s)", n, changed, want, raw)
		}
		if fn, ok := check[n]; ok {
			fn(t, s)
		}
	}

	// The capture ends with us not talking, nothing muted, one live server.
	if ic := s.Icon(); ic != IconQuiet {
		t.Errorf("end of replay: Icon() = %v, want %v", ic, IconQuiet)
	}
	final := s.Conns()
	if len(final) != 1 || final[0] != (Conn{
		ID: 1, ServerUID: "ExampleServerAUid00000000000000000000000000=",
		ServerName: "Example Server A", ClientID: 18,
		Status: StatusConnectionEstablished, InputHardware: true,
	}) {
		t.Errorf("end of replay: Conns() = %+v", final)
	}
}

// --- 3. multi-server capture handover (SYNTHETIC) -------------------------

// SYNTHETIC. The run-1 handover transcript in docs/protocol.md lives only in
// the lost events.jsonl (all clientPropertiesUpdated messages exceeded the
// probe's 220-byte stdout truncation). The envelopes below are hand-written to
// the exact shapes documented in docs/protocol.md, not captured.
//
// Proves: a second server can connect with no snapshot of its own, the mic
// follows inputHardware across both directions of the handover, Active() and
// Icon() follow it, the non-owning server reports MicDisabled, and status 0
// removes the connection.
func TestHandoverSynthetic(t *testing.T) {
	const (
		conn4UID  = "ExampleServerBUid00000000000000000000000000="
		conn4Name = "Example Server B"
	)
	s := freshState(t)

	step := func(name, msg string, wantChanged bool) {
		t.Helper()
		if got := s.Apply([]byte(msg)); got != wantChanged {
			t.Errorf("%s: Apply() changed = %v, want %v", name, got, wantChanged)
		}
	}

	// --- connect the second server: 1, 2, 3, 4 -----------------------------
	step("status 1", `{"type":"connectStatusChanged","payload":{
	  "connectionId":4,"error":0,"hotReload":false,"status":1,"info":null}}`, false)

	step("status 2", `{"type":"connectStatusChanged","payload":{
	  "connectionId":4,"error":0,"hotReload":false,"status":2,
	  "info":{"clientId":63676,"legacyUUID":"ExampleLegacyUUID0000000000=",
	          "serverName":"Example Server B",
	          "serverUid":"ExampleServerBUid00000000000000000000000000="}}}`, true)
	if n := len(s.Conns()); n != 1 {
		t.Errorf("after status 2: %d live conns, want 1 (status 2 is not established)", n)
	}

	step("status 3", `{"type":"connectStatusChanged","payload":{
	  "connectionId":4,"error":0,"hotReload":false,"status":3,"info":{"clientId":63676}}}`, true)
	step("status 4", `{"type":"connectStatusChanged","payload":{
	  "connectionId":4,"error":0,"hotReload":false,"status":4,"info":{"clientId":63676}}}`, true)

	conns := s.Conns()
	if len(conns) != 2 {
		t.Fatalf("after status 4: Conns() = %+v, want 2", conns)
	}
	if conns[1].ServerUID != conn4UID || conns[1].ServerName != conn4Name || conns[1].ClientID != 63676 {
		t.Errorf("conn 4 identity from status 2 not kept: %+v", conns[1])
	}
	if !conns[1].MicDisabled() {
		t.Error("fresh conn 4 MicDisabled() = false, want true (no capture device yet)")
	}
	mustActive(t, s, 1)

	// --- the client releases capture from conn 1 first ---------------------
	step("conn 1 releases capture", `{"type":"clientPropertiesUpdated","payload":{
	  "clientId":18,"connectionId":1,"properties":{
	    "inputMuted":false,"outputMuted":false,"inputDeactivated":false,
	    "inputHardware":false,"outputHardware":true,"flagTalking":false}}}`, true)
	if _, ok := s.Active(); ok {
		t.Error("Active() during handover: want none, every server has the mic disabled")
	}
	if ic := s.Icon(); ic != IconMicDisabled {
		t.Errorf("during handover: Icon() = %v, want %v", ic, IconMicDisabled)
	}

	// --- conn 4's first state arrives as clientMoved with properties -------
	step("conn 4 takes capture", `{"type":"clientMoved","payload":{
	  "clientId":63676,"connectionId":4,"hotReload":false,
	  "newChannelId":"16292","oldChannelId":"0","properties":{
	    "inputMuted":false,"outputMuted":false,"inputDeactivated":false,
	    "inputHardware":true,"outputHardware":true,"flagTalking":false}}}`, true)
	mustActive(t, s, 4)
	if ic := s.Icon(); ic != IconQuiet {
		t.Errorf("after handover to conn 4: Icon() = %v, want %v", ic, IconQuiet)
	}
	if c := s.Conns()[0]; !c.MicDisabled() {
		t.Errorf("conn 1 MicDisabled() = false while conn 4 owns the mic: %+v", c)
	}

	// conn 4 muting its speaker must now drive the icon.
	step("conn 4 mutes speaker", `{"type":"clientSelfPropertyUpdated","payload":{
	  "connectionId":4,"flag":"outputMuted","oldValue":false,"newValue":true}}`, true)
	if ic := s.Icon(); ic != IconSpeakerMuted {
		t.Errorf("conn 4 speaker muted: Icon() = %v, want %v", ic, IconSpeakerMuted)
	}
	step("conn 4 unmutes speaker", `{"type":"clientSelfPropertyUpdated","payload":{
	  "connectionId":4,"flag":"outputMuted","oldValue":true,"newValue":false}}`, true)

	// A mute on the *inactive* connection must not move the icon.
	step("conn 1 mutes mic (inactive)", `{"type":"clientSelfPropertyUpdated","payload":{
	  "connectionId":1,"flag":"inputMuted","oldValue":false,"newValue":true}}`, true)
	if ic := s.Icon(); ic != IconQuiet {
		t.Errorf("inactive conn 1 muted: Icon() = %v, want %v (conn 4 is active)", ic, IconQuiet)
	}
	step("conn 1 unmutes mic", `{"type":"clientSelfPropertyUpdated","payload":{
	  "connectionId":1,"flag":"inputMuted","oldValue":true,"newValue":false}}`, true)

	// --- and back: conn 1 takes capture, conn 4 loses it -------------------
	step("conn 1 takes capture back", `{"type":"clientPropertiesUpdated","payload":{
	  "clientId":18,"connectionId":1,"properties":{
	    "inputMuted":false,"outputMuted":false,"inputDeactivated":false,
	    "inputHardware":true,"outputHardware":true,"flagTalking":false}}}`, true)
	step("conn 4 loses capture", `{"type":"clientPropertiesUpdated","payload":{
	  "clientId":63676,"connectionId":4,"properties":{
	    "inputMuted":false,"outputMuted":false,"inputDeactivated":false,
	    "inputHardware":false,"outputHardware":true,"flagTalking":false}}}`, true)
	mustActive(t, s, 1)
	if c := s.Conns()[1]; !c.MicDisabled() {
		t.Errorf("conn 4 MicDisabled() = false after losing capture: %+v", c)
	}

	// clientPropertiesUpdated for somebody else's client id is ignored.
	step("other client's properties", `{"type":"clientPropertiesUpdated","payload":{
	  "clientId":24,"connectionId":1,"properties":{
	    "inputMuted":true,"outputMuted":true,"inputDeactivated":true,
	    "inputHardware":false,"outputHardware":true,"flagTalking":true}}}`, false)
	mustActive(t, s, 1)

	// --- disconnect the second server --------------------------------------
	step("conn 4 disconnects", `{"type":"connectStatusChanged","payload":{
	  "connectionId":4,"error":0,"hotReload":false,"status":0,"info":null}}`, true)
	if conns := s.Conns(); len(conns) != 1 || conns[0].ID != 1 {
		t.Fatalf("after disconnect: Conns() = %+v, want just conn 1", conns)
	}
	mustActive(t, s, 1)
	if ic := s.Icon(); ic != IconQuiet {
		t.Errorf("after disconnect: Icon() = %v, want %v", ic, IconQuiet)
	}

	// Disconnecting the last server leaves no icon at all.
	step("conn 1 disconnects", `{"type":"connectStatusChanged","payload":{
	  "connectionId":1,"error":0,"hotReload":false,"status":0,"info":null}}`, true)
	if ic := s.Icon(); ic != IconNone {
		t.Errorf("no connections: Icon() = %v, want %v", ic, IconNone)
	}
	step("conn 1 disconnects again", `{"type":"connectStatusChanged","payload":{
	  "connectionId":1,"error":0,"hotReload":false,"status":0,"info":null}}`, false)
}

// --- 4. icon priority -----------------------------------------------------

// Proves: all six icons, and the documented priority order
// OutputMuted > InputMuted > Talking > quiet, with mic ownership above all.
func TestIconPriority(t *testing.T) {
	live := func(c Conn) *Conn {
		c.Status = StatusConnectionEstablished
		return &c
	}
	tests := []struct {
		name  string
		conns []*Conn
		want  Icon
	}{
		{"no connections at all", nil, IconNone},
		{"only a non-established connection", []*Conn{{ID: 1, Status: StatusConnecting, InputHardware: true}}, IconNone},
		{"live and idle", []*Conn{live(Conn{ID: 1, InputHardware: true})}, IconQuiet},
		{"talking", []*Conn{live(Conn{ID: 1, InputHardware: true, Talking: true})}, IconTalking},
		{"mic muted beats talking", []*Conn{live(Conn{ID: 1, InputHardware: true, InputMuted: true, Talking: true})}, IconMicMuted},
		{"speaker muted beats mic muted", []*Conn{live(Conn{ID: 1, InputHardware: true, InputMuted: true, OutputMuted: true, Talking: true})}, IconSpeakerMuted},
		{"no capture device anywhere", []*Conn{live(Conn{ID: 1})}, IconMicDisabled},
		{"input deactivated on the capture owner", []*Conn{live(Conn{ID: 1, InputHardware: true, InputDeactivated: true})}, IconMicDisabled},
		{"mic disabled beats every mute", []*Conn{live(Conn{ID: 1, InputMuted: true, OutputMuted: true, Talking: true})}, IconMicDisabled},
		{"two servers, the second owns the mic and is muted", []*Conn{
			live(Conn{ID: 1, Talking: true}),
			live(Conn{ID: 4, InputHardware: true, InputMuted: true}),
		}, IconMicMuted},
		{"two servers, neither owns the mic", []*Conn{
			live(Conn{ID: 1}), live(Conn{ID: 4}),
		}, IconMicDisabled},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := &State{conns: map[int]*Conn{}}
			for _, c := range tc.conns {
				s.conns[c.ID] = c
			}
			if got := s.Icon(); got != tc.want {
				t.Errorf("Icon() = %v, want %v", got, tc.want)
			}
		})
	}
}

// Proves: every Icon has a distinct, non-empty String().
func TestIconString(t *testing.T) {
	want := map[Icon]string{
		IconNone: "none", IconQuiet: "quiet", IconTalking: "talking",
		IconMicMuted: "mic-muted", IconSpeakerMuted: "speaker-muted",
		IconMicDisabled: "mic-disabled",
	}
	seen := map[string]bool{}
	for ic, s := range want {
		if got := ic.String(); got != s {
			t.Errorf("Icon(%d).String() = %q, want %q", int(ic), got, s)
		}
		if seen[s] {
			t.Errorf("duplicate icon name %q", s)
		}
		seen[s] = true
	}
	if got := Icon(99).String(); got == "" {
		t.Error("Icon(99).String() is empty")
	}
}

// --- 5. junk input --------------------------------------------------------

// Proves: noise, unknown types and malformed input change nothing and never
// panic.
func TestApplyIgnoresJunk(t *testing.T) {
	tests := []struct {
		name string
		msg  string
	}{
		{"log noise", `{"type":"log","payload":{"channel":"TSDNS","complete":true,"id":0,"level":4,"message":"blah","time":"2026-09-23T20:30:00Z"}}`},
		{"unknown type", `{"type":"ts6trayBogus","payload":{"whatever":1}}`},
		{"unobserved catalog type", `{"type":"soundDeviceListChanged","payload":{"connectionId":1}}`},
		{"buttonPress ack", `{"type":"buttonPress","payload":{"button":"ts6tray.probe","state":true},"returnCode":"","status":{"code":0,"message":"ok"}}`},
		{"channels tree", `{"type":"channels","payload":{"connectionId":1,"hotReload":false,"info":{"rootChannels":[],"subChannels":{}}}}`},
		{"empty", ``},
		{"not json", `not json at all`},
		{"truncated json", `{"type":"clientPropertiesUpdated","payload":{"clientId":18,`},
		{"null", `null`},
		{"array", `[1,2,3]`},
		{"no payload", `{"type":"clientPropertiesUpdated"}`},
		{"null payload", `{"type":"clientPropertiesUpdated","payload":null}`},
		{"payload of the wrong type", `{"type":"clientPropertiesUpdated","payload":"nope"}`},
		{"self property on an unknown connection", `{"type":"clientSelfPropertyUpdated","payload":{"connectionId":77,"flag":"inputMuted","oldValue":false,"newValue":true}}`},
		{"self property with an unknown flag", `{"type":"clientSelfPropertyUpdated","payload":{"connectionId":1,"flag":"userTag","oldValue":"","newValue":"{\"tag\":\"x\"}"}}`},
		{"talk status for another client", `{"type":"talkStatusChanged","payload":{"clientId":24,"connectionId":1,"isWhisper":false,"status":1}}`},
		{"talk status on an unknown connection", `{"type":"talkStatusChanged","payload":{"clientId":18,"connectionId":77,"isWhisper":false,"status":1}}`},
		{"clientMoved without properties", `{"type":"clientMoved","payload":{"clientId":18,"connectionId":1,"hotReload":false,"newChannelId":"0","oldChannelId":"16297","type":1,"visibility":2}}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := freshState(t)
			before := s.Conns()
			if changed := s.Apply([]byte(tc.msg)); changed {
				t.Errorf("Apply() changed = true, want false")
			}
			after := s.Conns()
			if len(before) != len(after) || before[0] != after[0] {
				t.Errorf("state moved: %+v -> %+v", before, after)
			}
		})
	}

	// The same on a zero-value State, which has a nil map.
	var zero State
	for _, tc := range tests {
		if zero.Apply([]byte(tc.msg)) {
			t.Errorf("zero State: Apply(%s) changed = true, want false", tc.name)
		}
	}
	if ic := zero.Icon(); ic != IconNone {
		t.Errorf("zero State: Icon() = %v, want %v", ic, IconNone)
	}
	if _, ok := zero.Active(); ok {
		t.Error("zero State: Active() returned a connection")
	}
	if n := len(zero.Conns()); n != 0 {
		t.Errorf("zero State: Conns() has %d entries", n)
	}
	if err := zero.ApplyAuth([]byte(`not json`)); err == nil {
		t.Error("ApplyAuth(garbage) returned nil error")
	}
	if err := zero.ApplyAuth([]byte(`{}`)); err != nil {
		t.Errorf("ApplyAuth(empty object) = %v, want nil", err)
	}
}

// Proves: ids and bools are accepted as JSON numbers or JSON strings, since
// docs/protocol.md shows both spellings (channelId is "16292", connectionId is
// 4). The fixtures use numbers for connectionId/clientId and strings only for
// channelId; this guards the ambiguous cases.
func TestFlexibleScalars(t *testing.T) {
	s := freshState(t)
	if !s.Apply([]byte(`{"type":"clientSelfPropertyUpdated","payload":{
	  "connectionId":"1","flag":"inputMuted","oldValue":false,"newValue":"true"}}`)) {
		t.Fatal(`Apply with string connectionId/newValue: changed = false, want true`)
	}
	c := mustActive(t, s, 1)
	if !c.InputMuted {
		t.Error("InputMuted = false after a string-typed newValue")
	}
	if !s.Apply([]byte(`{"type":"talkStatusChanged","payload":{
	  "clientId":"18","connectionId":"1","isWhisper":false,"status":"1"}}`)) {
		t.Error("Apply with string ids in talkStatusChanged: changed = false, want true")
	}
	if c, _ := s.Active(); !c.Talking {
		t.Error("Talking = false after a string-typed talk status")
	}
}

// --- 6. regressions -------------------------------------------------------

// SYNTHETIC. Regression for the "early properties are lost" bug: our own
// full-form clientMoved for a new server arrived before connectStatusChanged
// had revealed our client id on that connection, so it was dropped for good.
// Nothing resends it, so Active() never found the mic owner again and the icon
// stuck on mic-disabled.
//
// Proves: such an event is buffered per client id and applied once status 2
// (or 3/4) names our client, while another client's early event on the same
// connection is never mistaken for ours.
func TestEarlyClientMovedIsNotLost(t *testing.T) {
	s := freshState(t)

	step := func(name, msg string, wantChanged bool) {
		t.Helper()
		if got := s.Apply([]byte(msg)); got != wantChanged {
			t.Errorf("%s: Apply() changed = %v, want %v", name, got, wantChanged)
		}
	}

	// Conn 1 hands the capture device over first, so nothing owns the mic.
	step("conn 1 releases capture", `{"type":"clientPropertiesUpdated","payload":{
	  "clientId":18,"connectionId":1,"properties":{
	    "inputMuted":false,"outputMuted":false,"inputDeactivated":false,
	    "inputHardware":false,"outputHardware":true,"flagTalking":false}}}`, true)
	if ic := s.Icon(); ic != IconMicDisabled {
		t.Fatalf("after release: Icon() = %v, want %v", ic, IconMicDisabled)
	}

	// Conn 4 is connecting. status 1 carries no info, so our client id on it
	// is still unknown.
	step("status 1", `{"type":"connectStatusChanged","payload":{
	  "connectionId":4,"error":0,"hotReload":false,"status":1,"info":null}}`, false)

	// Another client on conn 4 becomes visible first, with the capture device
	// set — as far as its own machine is concerned. This must not be adopted
	// as ours.
	step("other client's clientMoved", `{"type":"clientMoved","payload":{
	  "clientId":999,"connectionId":4,"hotReload":false,
	  "newChannelId":"16292","oldChannelId":"0","properties":{
	    "inputMuted":true,"outputMuted":true,"inputDeactivated":false,
	    "inputHardware":true,"outputHardware":true,"flagTalking":true}}}`, false)

	// Our own initial state for conn 4, still before any status with info.
	step("our clientMoved", `{"type":"clientMoved","payload":{
	  "clientId":63676,"connectionId":4,"hotReload":false,
	  "newChannelId":"16292","oldChannelId":"0","properties":{
	    "inputMuted":false,"outputMuted":false,"inputDeactivated":false,
	    "inputHardware":true,"outputHardware":true,"flagTalking":false}}}`, false)

	// Nothing is live yet, so the icon has not moved.
	if ic := s.Icon(); ic != IconMicDisabled {
		t.Errorf("before status 2: Icon() = %v, want %v", ic, IconMicDisabled)
	}

	// status 2 names our client id: the buffered properties belong to us now.
	step("status 2", `{"type":"connectStatusChanged","payload":{
	  "connectionId":4,"error":0,"hotReload":false,"status":2,
	  "info":{"clientId":63676,"legacyUUID":"ExampleLegacyUUID0000000000=",
	          "serverName":"Example Server B",
	          "serverUid":"ExampleServerBUid00000000000000000000000000="}}}`, true)

	c := s.conns[4]
	if c == nil {
		t.Fatal("conn 4 missing after status 2")
	}
	if !c.InputHardware {
		t.Error("conn 4 InputHardware = false: the early clientMoved was lost")
	}
	if c.InputMuted || c.OutputMuted || c.Talking {
		t.Errorf("conn 4 adopted the other client's flags: %+v", *c)
	}

	step("status 3", `{"type":"connectStatusChanged","payload":{
	  "connectionId":4,"error":0,"hotReload":false,"status":3,"info":{"clientId":63676}}}`, true)
	step("status 4", `{"type":"connectStatusChanged","payload":{
	  "connectionId":4,"error":0,"hotReload":false,"status":4,"info":{"clientId":63676}}}`, true)

	got := mustActive(t, s, 4)
	if !got.InputHardware {
		t.Errorf("Active() = %+v, want the capture device on conn 4", got)
	}
	if ic := s.Icon(); ic != IconQuiet {
		t.Errorf("Icon() = %v, want %v (conn 4 owns the mic)", ic, IconQuiet)
	}
}

// SYNTHETIC. Regression for stale flags across a reconnect: TeamSpeak can go
// 4 -> 1 -> 2 -> 3 -> 4 without a Disconnected (status 0) in between, and the
// old session's mute/talk/capture flags survived into the new one.
//
// Proves: dropping out of Established clears the five per-session audio flags
// and our client id, the new session's own properties are accepted, and the
// ordinary 2 -> 3 -> 4 first connect is untouched.
func TestReconnectWithoutDisconnectClearsFlags(t *testing.T) {
	s := freshState(t)

	step := func(name, msg string, wantChanged bool) {
		t.Helper()
		if got := s.Apply([]byte(msg)); got != wantChanged {
			t.Errorf("%s: Apply() changed = %v, want %v", name, got, wantChanged)
		}
	}

	// Conn 1 is established from the auth snapshot; mute it and start talking.
	step("mute input", `{"type":"clientSelfPropertyUpdated","payload":{
	  "connectionId":1,"flag":"inputMuted","oldValue":false,"newValue":true}}`, true)
	step("mute output", `{"type":"clientSelfPropertyUpdated","payload":{
	  "connectionId":1,"flag":"outputMuted","oldValue":false,"newValue":true}}`, true)
	step("talking", `{"type":"clientSelfPropertyUpdated","payload":{
	  "connectionId":1,"flag":"flagTalking","oldValue":false,"newValue":true}}`, true)
	if c := s.conns[1]; c == nil || !c.InputMuted || !c.OutputMuted || !c.Talking || !c.InputHardware {
		t.Fatalf("setup: conn 1 = %+v, want the flags set", c)
	}

	// The connection drops back to Connecting without a status 0.
	step("status 1 (reconnect)", `{"type":"connectStatusChanged","payload":{
	  "connectionId":1,"error":0,"hotReload":false,"status":1,"info":null}}`, true)

	c := s.conns[1]
	if c == nil {
		t.Fatal("conn 1 removed by a reconnect, want it kept")
	}
	if c.InputMuted || c.OutputMuted || c.InputDeactivated || c.InputHardware || c.Talking {
		t.Errorf("stale flags kept across a reconnect: %+v", *c)
	}
	if c.ServerUID == "" || c.ServerName == "" {
		t.Errorf("server identity lost across a reconnect: %+v", *c)
	}
	if n := len(s.Conns()); n != 0 {
		t.Errorf("%d live conns while reconnecting, want 0", n)
	}
	if ic := s.Icon(); ic != IconNone {
		t.Errorf("while reconnecting: Icon() = %v, want %v", ic, IconNone)
	}

	// Back up: 2, 3, 4 with a new client id, then our new properties.
	step("status 2", `{"type":"connectStatusChanged","payload":{
	  "connectionId":1,"error":0,"hotReload":false,"status":2,
	  "info":{"clientId":21,"serverName":"Example Server A",
	          "serverUid":"ExampleServerAUid00000000000000000000000000="}}}`, true)
	step("status 3", `{"type":"connectStatusChanged","payload":{
	  "connectionId":1,"error":0,"hotReload":false,"status":3,"info":{"clientId":21}}}`, true)
	step("status 4", `{"type":"connectStatusChanged","payload":{
	  "connectionId":1,"error":0,"hotReload":false,"status":4,"info":{"clientId":21}}}`, true)

	// A plain 2 -> 3 -> 4 climb must not have cleared anything itself, so the
	// properties that arrive now are the only source of the flags.
	step("our properties", `{"type":"clientMoved","payload":{
	  "clientId":21,"connectionId":1,"hotReload":false,
	  "newChannelId":"16292","oldChannelId":"0","properties":{
	    "inputMuted":false,"outputMuted":false,"inputDeactivated":false,
	    "inputHardware":true,"outputHardware":true,"flagTalking":false}}}`, true)

	got := mustActive(t, s, 1)
	if got.ClientID != 21 {
		t.Errorf("ClientID = %d, want 21 (the new session's id)", got.ClientID)
	}
	if got.InputMuted || got.OutputMuted || got.Talking {
		t.Errorf("after reconnect: %+v, want the old session's flags gone", got)
	}
	if ic := s.Icon(); ic != IconQuiet {
		t.Errorf("after reconnect: Icon() = %v, want %v", ic, IconQuiet)
	}
}

// --- 6. run-4 live capture: the state model returns to quiet ---------------

// REAL CAPTURE. testdata/events_run4_unmute.jsonl was recorded live on
// 2026-09-24 with tools/probe while the user talked on connection 1 (our
// client id 18, the same session testdata/auth_full.json snapshots).
//
// Caveat, recorded honestly: the user never muted during the two capture
// windows, so the file holds no inputMuted/outputMuted event. What it does
// prove against the real wire is the transition the bug report is about — mic
// unmuted and not talking — end to end: every talk start/stop pair, in the
// exact shapes and the exact order TeamSpeak sent them (the self-property
// flagTalking and talkStatusChanged arrive as a pair, µs apart, in either
// order), leaves the model on IconQuiet, and the other clients'
// clientPropertiesUpdated messages interleaved with them never disturb it.
//
// The live fault was NOT in this model: the daemon's `ts6tray status` reported
// "icon: quiet" correctly throughout the capture. See the report on tray.go's
// pixmap cache.
func TestReplayRun4EndsQuiet(t *testing.T) {
	s := freshState(t)
	if ic := s.Icon(); ic != IconQuiet {
		t.Fatalf("after auth: Icon() = %v, want %v", ic, IconQuiet)
	}

	var seq []Icon
	prev := s.Icon()
	for i, msg := range captureLines(t, "events_run4_unmute.jsonl") {
		changed := s.Apply(msg)
		ic := s.Icon()
		if !changed && ic != prev {
			t.Errorf("line %d: Apply() reported no change but Icon() moved %v -> %v", i+1, prev, ic)
		}
		if ic != prev {
			seq = append(seq, ic)
			prev = ic
		}
	}

	// Two talk bursts in the first capture window, one in the second.
	want := []Icon{
		IconTalking, IconQuiet,
		IconTalking, IconQuiet,
		IconTalking, IconQuiet,
	}
	if len(seq) != len(want) {
		t.Fatalf("icon sequence = %v, want %v", seq, want)
	}
	for i := range want {
		if seq[i] != want[i] {
			t.Fatalf("icon sequence = %v, want %v", seq, want)
		}
	}
	if ic := s.Icon(); ic != IconQuiet {
		t.Errorf("end of replay: Icon() = %v, want %v (unmuted and silent)", ic, IconQuiet)
	}

	final := s.Conns()
	if len(final) != 1 || final[0] != (Conn{
		ID: 1, ServerUID: "ExampleServerAUid00000000000000000000000000=",
		ServerName: "Example Server A", ClientID: 18,
		Status: StatusConnectionEstablished, InputHardware: true,
	}) {
		t.Errorf("end of replay: Conns() = %+v", final)
	}
}
