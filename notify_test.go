package main

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- helpers ---------------------------------------------------------------

// run5Roster is a roster seeded from run 5's auth snapshot: our connection is
// 4, our client id 30 ("Ritze") in channel 23, and the second account is client
// 33 ("RitzeTest"), parked in channel 24 with both mute flags set.
func run5Roster(t *testing.T) *roster {
	t.Helper()
	b := fixture(t, "auth_run5.json")
	var env struct {
		Payload json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(b, &env); err != nil {
		t.Fatalf("parse auth fixture: %v", err)
	}
	var r roster
	r.auth(env.Payload)
	return &r
}

// line renders a notice for a readable test failure.
func line(n notice) string {
	s := n.kind.String() + " | " + n.title
	if n.body != "" {
		s += " | " + n.body
	}
	return s
}

func lines(ns []notice) []string {
	out := make([]string, 0, len(ns))
	for _, n := range ns {
		out = append(out, line(n))
	}
	return out
}

// applyAll replays raw messages and collects everything the roster produced.
func applyAll(r *roster, msgs []json.RawMessage) []notice {
	var out []notice
	for _, m := range msgs {
		out = append(out, r.apply(m)...)
	}
	return out
}

func wantNotices(t *testing.T, got []notice, want []string) {
	t.Helper()
	if g := lines(got); !reflect.DeepEqual(g, want) {
		t.Errorf("notices:\n got %#v\nwant %#v", g, want)
	}
}

// --- the fixture replay ----------------------------------------------------

// TestRosterReplaysRun5 pins the exact, ordered output of replaying the whole
// run-5 capture on top of run 5's snapshot.
//
// Note two things the capture contains that the hand-written brief did not
// mention: client 23 ("UserA") was in our channel at
// snapshot time and leaves and rejoins it in the middle of the RitzeTest
// sequence, and the eight mute flips arrive interleaved with nothing else. The
// mute and channel-message notices come out of the roster unconditionally —
// they are off by default, but that filtering happens in the tray, not here.
func TestRosterReplaysRun5(t *testing.T) {
	r := run5Roster(t)
	got := applyAll(r, captureLines(t, "events_run5_channel.jsonl"))

	want := []string{
		"join | RitzeTest joined your channel",
		"leave | RitzeTest left your channel",
		"leave | UserA left your channel",
		"moved | Ritze moved RitzeTest into your channel",
		"join | UserA joined your channel",
		"moved | Ritze moved RitzeTest out of your channel",
		"moved | Ritze moved RitzeTest into your channel",
		"mute | RitzeTest unmuted their speakers",
		"mute | RitzeTest unmuted their microphone",
		"mute | RitzeTest muted their speakers",
		"mute | RitzeTest unmuted their speakers",
		"mute | RitzeTest muted their microphone",
		"mute | RitzeTest unmuted their microphone",
		"mute | RitzeTest muted their microphone",
		"mute | RitzeTest muted their speakers",
		"poke | RitzeTest poked you",
		"privateMsg | Message from RitzeTest | hi",
		"channelMsg | RitzeTest in channel | hi",
		"channelMsg | UserA in channel | hi",
	}
	wantNotices(t, got, want)
}

// TestRosterReplayWithDefaultFilterMatchesTheTray applies the shipped defaults
// to the same replay: the mute flips, the channel chatter and — because
// TeamSpeak already pops those up itself — the poke and the private message
// disappear, which is the quiet menu the user gets out of the box.
func TestRosterReplayWithDefaultFilterMatchesTheTray(t *testing.T) {
	r := run5Roster(t)
	all := applyAll(r, captureLines(t, "events_run5_channel.jsonl"))

	defs := trayNotifyDefaults()
	var got []notice
	for _, n := range all {
		if defs[trayNotifyKindGroup[n.kind]] {
			got = append(got, n)
		}
	}
	want := []string{
		"join | RitzeTest joined your channel",
		"leave | RitzeTest left your channel",
		"leave | UserA left your channel",
		"moved | Ritze moved RitzeTest into your channel",
		"join | UserA joined your channel",
		"moved | Ritze moved RitzeTest out of your channel",
		"moved | Ritze moved RitzeTest into your channel",
	}
	wantNotices(t, got, want)
}

// TestRosterIgnoresUnrelatedAndSelfEvents: the run-5 capture carries 7
// clientPropertiesUpdated for clients outside our channel with no mute change,
// 214 talkStatusChanged, 7 clientChannelGroupChanged and a clientChatComposing.
// None of them may produce a notice, and nothing in the whole replay may be
// about us (client 30, "Ritze") as the subject.
func TestRosterIgnoresUnrelatedAndSelfEvents(t *testing.T) {
	r := run5Roster(t)
	for _, m := range captureLines(t, "events_run5_channel.jsonl") {
		var env struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(m, &env); err != nil {
			t.Fatal(err)
		}
		ns := r.apply(m)
		switch env.Type {
		case "talkStatusChanged", "clientChannelGroupChanged", "clientChatComposing",
			"clientSelfPropertyUpdated", "streamInfoReplace":
			if len(ns) != 0 {
				t.Errorf("%s produced %v, want nothing", env.Type, lines(ns))
			}
		}
	}

	// "Ritze" only ever appears as the invoker of a move, never as the subject.
	r2 := run5Roster(t)
	for _, n := range applyAll(r2, captureLines(t, "events_run5_channel.jsonl")) {
		switch n.kind {
		case noticeJoin, noticeLeave, noticeMute:
			if strings.HasPrefix(n.title, "Ritze") && !strings.HasPrefix(n.title, "RitzeTest") {
				t.Errorf("notice about ourselves: %q", line(n))
			}
		}
	}
}

// --- synthetic events ------------------------------------------------------

// moved builds a clientMoved message; extra is merged into the payload.
func moved(clientID int, oldCh, newCh string, typ int, extra map[string]any) json.RawMessage {
	p := map[string]any{
		"clientId":     clientID,
		"connectionId": 4,
		"oldChannelId": oldCh,
		"newChannelId": newCh,
		"type":         typ,
		"visibility":   1,
	}
	for k, v := range extra {
		p[k] = v
	}
	b, err := json.Marshal(map[string]any{"type": "clientMoved", "payload": p})
	if err != nil {
		panic(err)
	}
	return b
}

func status(connID, st, errCode int, info map[string]any) json.RawMessage {
	p := map[string]any{"connectionId": connID, "status": st, "error": errCode}
	if info != nil {
		p["info"] = info
	}
	b, err := json.Marshal(map[string]any{"type": "connectStatusChanged", "payload": p})
	if err != nil {
		panic(err)
	}
	return b
}

// TestRosterKickWordings covers clientMoved types 3..6 out of our channel. None
// of them has ever been captured, so this pins the best-effort wording taken
// from the client bundle's enum, not from the wire.
func TestRosterKickWordings(t *testing.T) {
	for _, tc := range []struct {
		typ  int
		want string
	}{
		{movedTimeout, "kicked | RitzeTest timed out"},
		{movedKickChannel, "kicked | RitzeTest was kicked from the channel"},
		{movedKickServer, "kicked | RitzeTest was kicked from the server"},
		{movedBanFromServer, "kicked | RitzeTest was banned from the server"},
	} {
		r := run5Roster(t)
		// Put 33 in our channel first, then throw it out.
		r.apply(moved(33, "24", "23", movedMove, nil))
		got := r.apply(moved(33, "23", "37", tc.typ, nil))
		wantNotices(t, got, []string{tc.want})
	}
}

// TestRosterKickReasonGoesInTheBody: no reason field has ever been observed, so
// it is used only when it is actually there.
func TestRosterKickReasonGoesInTheBody(t *testing.T) {
	r := run5Roster(t)
	r.apply(moved(33, "24", "23", movedMove, nil))
	got := r.apply(moved(33, "23", "37", movedKickChannel, map[string]any{"reason": "spam"}))
	wantNotices(t, got, []string{"kicked | RitzeTest was kicked from the channel | spam"})
}

// TestRosterDisconnectFromServer: newChannelId "0" is not a channel, it means
// the client left the server. Plain leaves say so; a timeout, server kick or
// ban keeps its own wording.
func TestRosterDisconnectFromServer(t *testing.T) {
	for _, tc := range []struct {
		typ  int
		want string
	}{
		{movedMove, "leave | RitzeTest disconnected"},
		{movedTimeout, "kicked | RitzeTest timed out"},
		{movedKickServer, "kicked | RitzeTest was kicked from the server"},
		{movedBanFromServer, "kicked | RitzeTest was banned from the server"},
	} {
		r := run5Roster(t)
		r.apply(moved(33, "24", "23", movedMove, nil))
		got := r.apply(moved(33, "23", "0", tc.typ, nil))
		wantNotices(t, got, []string{tc.want})

		// Membership is gone: a later property update for that client must not
		// be read as "someone in my channel muted".
		rc := r.conns[4]
		if _, ok := rc.chans[33]; ok {
			t.Errorf("type %d: client 33 still has a channel after disconnecting", tc.typ)
		}
		if _, ok := rc.mute[33]; ok {
			t.Errorf("type %d: client 33 still has mute state after disconnecting", tc.typ)
		}
	}
}

// TestRosterNewClientWithProperties: a client that was never seen arrives with
// an inline properties block (the "full form" of clientMoved, which run 5 never
// produced). Its nickname must come from there, so it cannot read as
// "Client 99 joined", and it must announce itself even when the move type is
// Subscription, which is silent for everyone else.
func TestRosterNewClientWithProperties(t *testing.T) {
	props := map[string]any{
		"nickname":    "Newcomer",
		"inputMuted":  true,
		"outputMuted": false,
	}
	for _, typ := range []int{movedSubscription, movedMove} {
		r := run5Roster(t)
		got := r.apply(moved(99, "0", "23", typ, map[string]any{"properties": props}))
		wantNotices(t, got, []string{"join | Newcomer joined your channel"})

		// The mute baseline came with it, so the very next property update is
		// a real diff and not a silently swallowed first sighting.
		b, _ := json.Marshal(map[string]any{
			"type": "clientPropertiesUpdated",
			"payload": map[string]any{
				"clientId": 99, "connectionId": 4,
				"properties": map[string]any{"nickname": "Newcomer", "inputMuted": false, "outputMuted": false},
			},
		})
		wantNotices(t, r.apply(b), []string{"mute | Newcomer unmuted their microphone"})
	}
}

// TestRosterKnownClientWithProperties: the same full form for a client we
// already know is an ordinary move that also refreshes the nickname.
func TestRosterKnownClientWithProperties(t *testing.T) {
	r := run5Roster(t)
	got := r.apply(moved(33, "24", "23", movedMove, map[string]any{
		"properties": map[string]any{"nickname": "Renamed", "inputMuted": false, "outputMuted": false},
	}))
	wantNotices(t, got, []string{"join | Renamed joined your channel"})
}

// TestRosterSameChannelMoveIsSilent: old == new is a no-op, not a join.
func TestRosterSameChannelMoveIsSilent(t *testing.T) {
	r := run5Roster(t)
	r.apply(moved(33, "24", "23", movedMove, nil))
	if got := r.apply(moved(33, "23", "23", movedMove, nil)); len(got) != 0 {
		t.Errorf("same-channel move produced %v, want nothing", lines(got))
	}
}

// TestRosterOwnMoveIsSilent: when we move, membership is still tracked per
// clientId, so the one event must produce nothing at all — no mass leave, no
// mass join — while still updating which channel is "ours".
func TestRosterOwnMoveIsSilent(t *testing.T) {
	r := run5Roster(t)
	if got := r.apply(moved(30, "23", "37", movedMove, nil)); len(got) != 0 {
		t.Fatalf("our own move produced %v, want nothing", lines(got))
	}
	if ch := r.conns[4].channel; ch != "37" {
		t.Fatalf("our channel = %q after moving, want 37", ch)
	}
	// Our new channel is now the one that matters: 33 is in 24, so its move to
	// 37 is a join and its move back to 23 is silent.
	wantNotices(t, r.apply(moved(33, "24", "37", movedMove, nil)),
		[]string{"join | RitzeTest joined your channel"})
	wantNotices(t, r.apply(moved(33, "37", "23", movedMove, nil)),
		[]string{"leave | RitzeTest left your channel"})
}

// TestRosterConnLost: status 0 with a non-zero error is a drop worth telling
// the user about; status 0 with error 0 is the user pressing Disconnect.
func TestRosterConnLost(t *testing.T) {
	r := run5Roster(t)
	wantNotices(t, r.apply(status(4, StatusDisconnected, 5, nil)),
		[]string{"connLost | Lost connection to Example Server A"})

	r2 := run5Roster(t)
	if got := r2.apply(status(4, StatusDisconnected, 0, nil)); len(got) != 0 {
		t.Errorf("clean disconnect produced %v, want nothing", lines(got))
	}
}

// TestRosterConnLostUsesTheLatestServerName: for a connection that was not in
// the snapshot the name arrives with connectStatusChanged status 2.
func TestRosterConnLostUsesTheLatestServerName(t *testing.T) {
	var r roster
	r.apply(status(7, StatusConnected, 0, map[string]any{"clientId": 5, "serverName": "Other <Server>"}))
	wantNotices(t, r.apply(status(7, StatusDisconnected, 3, nil)),
		[]string{"connLost | Lost connection to Other <Server>"})
}

// TestRosterEscapesUserText: a freedesktop notification body may be
// interpreted as markup, so user text in it is escaped; the summary/title is
// plain text, so user text in it stays literal.
func TestRosterEscapesUserText(t *testing.T) {
	r := run5Roster(t)
	b, _ := json.Marshal(map[string]any{
		"type": "textMessage",
		"payload": map[string]any{
			"connectionId": 4,
			"invoker":      map[string]any{"id": 33, "nickname": "Tom's <b>Bold</b>"},
			"message":      "<i>hi</i> & bye",
			"targetId":     30,
			"targetMode":   targetPrivate,
		},
	})
	wantNotices(t, r.apply(b),
		[]string{"privateMsg | Message from Tom's <b>Bold</b> | &lt;i&gt;hi&lt;/i&gt; &amp; bye"})

	// A nickname from clientMoved also lands in a title: still literal.
	wantNotices(t, r.apply(moved(33, "24", "23", movedMove, nil)),
		[]string{"join | Tom's <b>Bold</b> joined your channel"})
}

// TestRosterCapsLongBodies: a 10 000-character message must not become a
// 10 000-character notification.
func TestRosterCapsLongBodies(t *testing.T) {
	long := make([]rune, 10000)
	for i := range long {
		long[i] = 'x'
	}
	r := run5Roster(t)
	b, _ := json.Marshal(map[string]any{
		"type": "textMessage",
		"payload": map[string]any{
			"connectionId": 4,
			"invoker":      map[string]any{"id": 33, "nickname": "RitzeTest"},
			"message":      string(long),
			"targetId":     30,
			"targetMode":   targetPrivate,
		},
	})
	got := r.apply(b)
	if len(got) != 1 {
		t.Fatalf("got %v, want one notice", lines(got))
	}
	if n := len([]rune(got[0].body)); n != noticeMaxBody+1 { // +1 for the ellipsis
		t.Errorf("body is %d runes, want %d", n, noticeMaxBody+1)
	}
}

// TestRosterOwnMessageIsNotNotified: our own client id as the invoker means the
// message came from us.
func TestRosterOwnMessageIsNotNotified(t *testing.T) {
	r := run5Roster(t)
	b, _ := json.Marshal(map[string]any{
		"type": "textMessage",
		"payload": map[string]any{
			"connectionId": 4,
			"invoker":      map[string]any{"id": 30, "nickname": "Ritze"},
			"message":      "hi",
			"targetId":     0,
			"targetMode":   targetChannel,
		},
	})
	if got := r.apply(b); len(got) != 0 {
		t.Errorf("our own channel message produced %v, want nothing", lines(got))
	}
}

// TestRosterServerSuffixWhenMultipleConnections: with more than one server
// connected the title has to say which one it is about.
func TestRosterServerSuffixWhenMultipleConnections(t *testing.T) {
	r := run5Roster(t)
	wantNotices(t, r.apply(moved(33, "24", "23", movedMove, nil)),
		[]string{"join | RitzeTest joined your channel"})

	// A second server appears; from now on every title is qualified.
	r.apply(status(9, StatusConnected, 0, map[string]any{"clientId": 2, "serverName": "Second"}))
	wantNotices(t, r.apply(moved(33, "23", "37", movedMove, nil)),
		[]string{"leave | RitzeTest left your channel — Example Server A"})
}

// TestRosterGarbageIsIgnored: a malformed or unknown message must not panic.
func TestRosterGarbageIsIgnored(t *testing.T) {
	r := run5Roster(t)
	for _, s := range []string{"", "{", "null", `{"type":"clientMoved"}`, `{"type":"nope","payload":{}}`,
		`{"type":"textMessage","payload":{"connectionId":4}}`} {
		if got := r.apply([]byte(s)); len(got) != 0 {
			t.Errorf("%q produced %v, want nothing", s, lines(got))
		}
	}
}

// TestNoticeKindsAllHaveASwitch: every kind the roster can emit must be
// reachable from a menu item, otherwise it would be silently undisplayable.
func TestNoticeKindsAllHaveASwitch(t *testing.T) {
	for k := noticeJoin; k <= noticeConnLost; k++ {
		if _, ok := trayNotifyKindGroup[k]; !ok {
			t.Errorf("noticeKind %v has no Settings -> Notifications switch", k)
		}
		if k.String() == "unknown" {
			t.Errorf("noticeKind %d has no name", int(k))
		}
	}
}

// --- batching -------------------------------------------------------------

// TestFlattenNotices covers the flush format: a single notice passes through,
// several become a counted title and one numbered, escaped line each in
// newest-first order, a shared server suffix is hoisted into the title, and a
// long batch is cut off.
func TestFlattenNotices(t *testing.T) {
	tests := []struct {
		name  string
		in    []notice
		title string
		body  string
	}{
		{
			name:  "one passes through unnumbered",
			in:    []notice{{title: "Mo & co joined your channel", body: "hi &amp; bye"}},
			title: "Mo & co joined your channel",
			body:  "hi &amp; bye",
		},
		{
			name: "several, newest first, titles escaped for the markup body",
			in: []notice{
				{title: "<b> joined your channel"},
				{title: "Mo moved <b> out of your channel", body: "afk"},
			},
			title: "2 TeamSpeak events",
			body:  "1. Mo moved &lt;b&gt; out of your channel: afk\n2. &lt;b&gt; joined your channel",
		},
		{
			name: "three keep arriving order reversed",
			in: []notice{
				{title: "A joined your channel"},
				{title: "B joined your channel"},
				{title: "C joined your channel"},
			},
			title: "3 TeamSpeak events",
			body:  "1. C joined your channel\n2. B joined your channel\n3. A joined your channel",
		},
		{
			name: "a shared server suffix moves into the title",
			in: []notice{
				{title: "A joined your channel — Home"},
				{title: "B joined your channel — Home"},
			},
			title: "2 TeamSpeak events — Home",
			body:  "1. B joined your channel — Home\n2. A joined your channel — Home",
		},
		{
			name: "mixed servers leave the title plain",
			in: []notice{
				{title: "A joined your channel — Home"},
				{title: "B joined your channel — Work"},
			},
			title: "2 TeamSpeak events",
			body:  "1. B joined your channel — Work\n2. A joined your channel — Home",
		},
		{
			name: "no suffix at all leaves the title plain",
			in: []notice{
				{title: "A joined your channel"},
				{title: "B joined your channel"},
			},
			title: "2 TeamSpeak events",
			body:  "1. B joined your channel\n2. A joined your channel",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			title, body, _ := flattenNotices(tc.in)
			if title != tc.title {
				t.Errorf("title = %q, want %q", title, tc.title)
			}
			if body != tc.body {
				t.Errorf("body =\n%q\nwant\n%q", body, tc.body)
			}
		})
	}
}

// TestFlattenNoticesCap: past ten lines the rest is summarised, and because the
// newest is line 1 what falls off is the oldest — which the wording has to say.
func TestFlattenNoticesCap(t *testing.T) {
	var ns []notice
	for i := 0; i < 14; i++ {
		ns = append(ns, notice{title: "line " + strconv.Itoa(i)})
	}
	title, body, _ := flattenNotices(ns)
	if title != "14 TeamSpeak events" {
		t.Errorf("title = %q", title)
	}
	lines := strings.Split(body, "\n")
	if len(lines) != noticeBatchLines+1 {
		t.Fatalf("body has %d lines, want %d:\n%s", len(lines), noticeBatchLines+1, body)
	}
	// Newest (line 13) first, counting down to line 4; lines 0..3 are dropped.
	for i := 0; i < noticeBatchLines; i++ {
		want := strconv.Itoa(i+1) + ". line " + strconv.Itoa(13-i)
		if lines[i] != want {
			t.Errorf("lines[%d] = %q, want %q", i, lines[i], want)
		}
	}
	if lines[10] != "…and 4 more earlier" {
		t.Errorf("last line = %q, want %q", lines[10], "…and 4 more earlier")
	}
}

// TestFlattenNoticesIcon: the batch shows the newest notice's icon, the one it
// puts on line 1, and a batch of plain notices asks for no state icon at all.
func TestFlattenNoticesIcon(t *testing.T) {
	ns := []notice{
		{kind: noticeMute, title: "A muted their microphone", icon: iconMicMuted},
		{kind: noticeMute, title: "B muted their speakers", icon: iconSpeakerMuted},
		{kind: noticeMute, title: "C unmuted their microphone", icon: iconUnmuted},
	}
	if _, body, ic := flattenNotices(ns); ic != iconUnmuted {
		t.Errorf("batch icon = %v, want %v (body: %q)", ic, iconUnmuted, body)
	}
	// One notice passes its own icon through.
	if _, _, ic := flattenNotices(ns[1:2]); ic != iconSpeakerMuted {
		t.Errorf("single icon = %v, want %v", ic, iconSpeakerMuted)
	}
	// Nothing sets an icon: the sentinel, meaning the app icon.
	plain := []notice{{title: "A joined your channel"}, {title: "B joined your channel"}}
	if _, _, ic := flattenNotices(plain); ic != iconApp {
		t.Errorf("plain batch icon = %v, want %v", ic, iconApp)
	}
	if _, _, ic := flattenNotices(nil); ic != iconApp {
		t.Errorf("empty batch icon = %v, want %v", ic, iconApp)
	}
}

// TestNoticeIcon pins the precedence: speakers beat the mic, exactly as the
// tray's own icon does, and unmuted is the plain ring.
func TestNoticeIcon(t *testing.T) {
	tests := []struct {
		in   [2]bool // {inputMuted, outputMuted}
		want notifyIcon
	}{
		{[2]bool{false, false}, iconUnmuted},
		{[2]bool{true, false}, iconMicMuted},
		{[2]bool{false, true}, iconSpeakerMuted},
		{[2]bool{true, true}, iconSpeakerMuted},
	}
	for _, tc := range tests {
		if got := noticeIcon(tc.in); got != tc.want {
			t.Errorf("noticeIcon(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// TestRosterMuteIcons walks a client through every mute transition and checks
// the notice carries the state it ended up in, not the change it made.
func TestRosterMuteIcons(t *testing.T) {
	props := func(id int, in, out bool) []byte {
		return []byte(fmt.Sprintf(
			`{"type":"clientPropertiesUpdated","payload":{"connectionId":1,"clientId":%d,`+
				`"properties":{"nickname":"RitzeTest","inputMuted":%t,"outputMuted":%t}}}`,
			id, in, out))
	}
	// Us (7) in channel 5, RitzeTest (9) beside us, nothing muted.
	newRoster := func() *roster {
		r := &roster{}
		r.auth([]byte(`{"connections":[{"id":1,"clientId":7,"properties":{"name":"Home"},
			"clientInfos":[
				{"id":7,"channelId":5,"properties":{"nickname":"Me"}},
				{"id":9,"channelId":5,"properties":{"nickname":"RitzeTest","inputMuted":false,"outputMuted":false}}
			]}]}`))
		return r
	}

	tests := []struct {
		name      string
		in, out   bool
		wantTitle []string
		wantIcon  notifyIcon
	}{
		{"mic muted", true, false,
			[]string{"RitzeTest muted their microphone"}, iconMicMuted},
		{"speakers muted", false, true,
			[]string{"RitzeTest muted their speakers"}, iconSpeakerMuted},
		{"both muted at once: only the speakers are worth saying",
			true, true,
			[]string{"RitzeTest muted their speakers"},
			iconSpeakerMuted},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := newRoster()
			ns := r.apply(props(9, tc.in, tc.out))
			if len(ns) != len(tc.wantTitle) {
				t.Fatalf("got %d notices, want %d: %+v", len(ns), len(tc.wantTitle), ns)
			}
			for i, n := range ns {
				if n.title != tc.wantTitle[i] {
					t.Errorf("notice %d title = %q, want %q", i, n.title, tc.wantTitle[i])
				}
				if n.icon != tc.wantIcon {
					t.Errorf("notice %d icon = %v, want %v", i, n.icon, tc.wantIcon)
				}
			}
		})
	}

	// A full unmute from both-muted ends at the empty talking indicator: the icon
	// says "nothing is muted any more". It is one notice, the speakers': the
	// mic was not worth reporting while they could not hear.
	r := newRoster()
	r.apply(props(9, true, true))
	ns := r.apply(props(9, false, false))
	if len(ns) != 1 || ns[0].title != "RitzeTest unmuted their speakers" {
		t.Fatalf("full unmute produced %+v, want one speaker-unmute notice", ns)
	}
	if ns[0].icon != iconUnmuted {
		t.Errorf("%q carries icon %v, want %v", ns[0].title, ns[0].icon, iconUnmuted)
	}
}

// TestRosterMuteWhileDeaf: another user's microphone is only news while they
// can hear us. Someone whose speakers are muted flipping their mic does
// nothing to us at all — TeamSpeak mutes their mic implicitly in that state —
// so it says nothing, while their speakers going on and off still do.
func TestRosterMuteWhileDeaf(t *testing.T) {
	props := func(in, out bool) []byte {
		return []byte(fmt.Sprintf(
			`{"type":"clientPropertiesUpdated","payload":{"connectionId":1,"clientId":9,`+
				`"properties":{"nickname":"RitzeTest","inputMuted":%t,"outputMuted":%t}}}`,
			in, out))
	}
	newRoster := func() *roster {
		r := &roster{}
		r.auth([]byte(`{"connections":[{"id":1,"clientId":7,"properties":{"name":"Home"},
			"clientInfos":[
				{"id":7,"channelId":5,"properties":{"nickname":"Me"}},
				{"id":9,"channelId":5,"properties":{"nickname":"RitzeTest","inputMuted":false,"outputMuted":true}}
			]}]}`))
		return r
	}

	// Speaker-muted throughout: muting and unmuting the mic is silent.
	r := newRoster()
	if ns := r.apply(props(true, true)); len(ns) != 0 {
		t.Errorf("mic muted while deaf produced %v, want nothing", lines(ns))
	}
	if ns := r.apply(props(false, true)); len(ns) != 0 {
		t.Errorf("mic unmuted while deaf produced %v, want nothing", lines(ns))
	}

	// The speakers themselves still speak.
	ns := r.apply(props(false, false))
	if len(ns) != 1 || ns[0].title != "RitzeTest unmuted their speakers" {
		t.Fatalf("got %+v, want one speaker-unmute notice", ns)
	}
	if ns[0].icon != iconUnmuted {
		t.Errorf("icon = %v, want %v", ns[0].icon, iconUnmuted)
	}

	// Both flags in one update: only the speaker notice, the one that says
	// whether they are in the conversation.
	ns = r.apply(props(true, true))
	if len(ns) != 1 || ns[0].title != "RitzeTest muted their speakers" {
		t.Fatalf("combined change produced %+v, want only the speaker notice", ns)
	}

	// Our own mic is unaffected by any of this: we hear ourselves regardless.
	self := &roster{}
	self.auth([]byte(`{"connections":[{"id":1,"clientId":7,"properties":{"name":"Home"},
		"clientInfos":[{"id":7,"channelId":5,"properties":{"nickname":"Me","inputMuted":false,"outputMuted":true}}]}]}`))
	got := self.apply([]byte(`{"type":"clientPropertiesUpdated","payload":{"connectionId":1,"clientId":7,
		"properties":{"nickname":"Me","inputMuted":true,"outputMuted":true}}}`))
	wantNotices(t, got, []string{"self | You muted your microphone"})
}

// TestNoticeBatcherFlush: flush() empties the queue at once and a flushed
// batcher does not fire again afterwards.
func TestNoticeBatcherFlush(t *testing.T) {
	var mu sync.Mutex
	var sent []string
	b := &noticeBatcher{window: time.Hour, cap: time.Hour, send: func(title, _ string, _ notifyIcon) {
		mu.Lock()
		defer mu.Unlock()
		sent = append(sent, title)
	}}
	b.add(notice{title: "A"})
	b.add(notice{title: "B"})
	b.flush()
	b.flush() // nothing pending: must not send an empty notification

	mu.Lock()
	defer mu.Unlock()
	if len(sent) != 1 || sent[0] != "2 TeamSpeak events" {
		t.Errorf("sent = %q, want one batch", sent)
	}
}

// --- our own changes -------------------------------------------------------

// msg is one raw event, written the way the capture does.
func msg(s string) json.RawMessage { return json.RawMessage(s) }

// selfPropsMuted is a clientPropertiesUpdated for our own client (30) on
// connection 4 with the two mute flags set as given.
func selfPropsMuted(in, out bool) json.RawMessage {
	return msg(fmt.Sprintf(`{"type":"clientPropertiesUpdated","payload":{"clientId":30,"connectionId":4,
		"properties":{"nickname":"Ritze","inputMuted":%t,"outputMuted":%t}}}`, in, out))
}

// selfFlag is the other event that reports the same thing, one flag at a time.
func selfFlag(flag string, v bool) json.RawMessage {
	return msg(fmt.Sprintf(`{"type":"clientSelfPropertyUpdated","payload":{"connectionId":4,
		"flag":%q,"oldValue":%t,"newValue":%t}}`, flag, !v, v))
}

// TestRosterSelfMuteNotices: our own mute changes are notices of their own, and
// they carry the same resulting-state icon other people's mute notices do.
func TestRosterSelfMuteNotices(t *testing.T) {
	r := run5Roster(t)

	got := r.apply(selfPropsMuted(true, false))
	wantNotices(t, got, []string{"self | You muted your microphone"})
	if got[0].icon != iconMicMuted {
		t.Errorf("icon = %v, want iconMicMuted", got[0].icon)
	}

	got = r.apply(selfPropsMuted(true, true))
	wantNotices(t, got, []string{"self | You muted your speakers"})
	if got[0].icon != iconSpeakerMuted {
		t.Errorf("icon = %v, want iconSpeakerMuted", got[0].icon)
	}

	// Both back at once: two notices, both showing the unmuted ring.
	got = r.apply(selfPropsMuted(false, false))
	wantNotices(t, got, []string{
		"self | You unmuted your microphone",
		"self | You unmuted your speakers",
	})
	for _, n := range got {
		if n.icon != iconUnmuted {
			t.Errorf("icon = %v, want iconUnmuted", n.icon)
		}
	}
}

// TestRosterSelfMuteNotifiesOnce: TeamSpeak reports one of our own mute flips
// twice, as clientPropertiesUpdated *and* clientSelfPropertyUpdated, in either
// order. Whichever lands first is the notice; the second must be silent.
func TestRosterSelfMuteNotifiesOnce(t *testing.T) {
	for _, tc := range []struct {
		name string
		msgs []json.RawMessage
	}{
		{"properties first", []json.RawMessage{selfPropsMuted(true, false), selfFlag("inputMuted", true)}},
		{"self flag first", []json.RawMessage{selfFlag("inputMuted", true), selfPropsMuted(true, false)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := run5Roster(t)
			wantNotices(t, applyAll(r, tc.msgs), []string{"self | You muted your microphone"})
		})
	}
}

// TestRosterSelfFlagIcons: a mute reported only as a self flag still gets the
// right icon, and flags that are not about muting say nothing at all.
func TestRosterSelfFlagIcons(t *testing.T) {
	r := run5Roster(t)
	got := r.apply(selfFlag("outputMuted", true))
	wantNotices(t, got, []string{"self | You muted your speakers"})
	if got[0].icon != iconSpeakerMuted {
		t.Errorf("icon = %v, want iconSpeakerMuted", got[0].icon)
	}
	if ns := r.apply(selfFlag("flagTalking", true)); len(ns) != 0 {
		t.Errorf("flagTalking produced %v", lines(ns))
	}
}

// TestRosterSelfMoved: being moved by someone else is a notice and names the
// channel when the snapshot's channel tree knew it; our own move is not.
func TestRosterSelfMoved(t *testing.T) {
	moved := func(typ int, ch string, invoker string) json.RawMessage {
		inv := ""
		if invoker != "" {
			inv = `"invoker":` + invoker + `,`
		}
		return msg(fmt.Sprintf(`{"type":"clientMoved","payload":{"clientId":30,"connectionId":4,
			%s"newChannelId":%q,"oldChannelId":"23","type":%d,"visibility":1}}`, inv, ch, typ))
	}
	const other = `{"id":33,"nickname":"RitzeTest"}`
	const us = `{"id":30,"nickname":"Ritze"}`

	for _, tc := range []struct {
		name string
		in   json.RawMessage
		want []string
	}{
		{"moved by someone into a known channel", moved(movedMoved, "37", other),
			[]string{"self | RitzeTest moved you to ╠ Rastung (live)"}},
		{"moved into a channel we have no name for", moved(movedMoved, "9999", other),
			[]string{"self | RitzeTest moved you to another channel"}},
		{"our own move", moved(movedMove, "37", ""), []string{}},
		{"a type 2 move we invoked ourselves", moved(movedMoved, "37", us), []string{}},
		{"kicked from the channel", moved(movedKickChannel, "1", other),
			[]string{"self | You were kicked from the channel"}},
		{"kicked from the server", moved(movedKickServer, "0", other),
			[]string{"self | You were kicked from the server"}},
		{"timed out", moved(movedTimeout, "0", ""), []string{"self | You timed out"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := run5Roster(t)
			wantNotices(t, r.apply(tc.in), tc.want)
		})
	}
}

// TestRosterNoticeIconsByKind pins the picture every kind of notice carries.
// It is the direction that decides, not the event: a move is green when it
// lands in our channel and red when it leaves it, and a kick is a departure
// however it is spelled. Our own move keeps the app icon — we are still here —
// while our own kick is a departure like anybody else's.
func TestRosterNoticeIconsByKind(t *testing.T) {
	// Client 33 is parked in channel 24; 23 is ours.
	moved := func(client int, from, to string, typ int, invoker bool) json.RawMessage {
		inv := ""
		if invoker {
			inv = `"invoker":{"id":33,"nickname":"RitzeTest"},`
		}
		return msg(fmt.Sprintf(`{"type":"clientMoved","payload":{"clientId":%d,"connectionId":4,
			%s"newChannelId":%q,"oldChannelId":%q,"type":%d,"visibility":1}}`,
			client, inv, to, from, typ))
	}
	for _, tc := range []struct {
		name string
		in   json.RawMessage
		want notifyIcon
	}{
		{"join", moved(33, "24", "23", movedMove, false), iconJoin},
		{"leave", moved(33, "23", "24", movedMove, false), iconLeave},
		{"moved into our channel", moved(33, "24", "23", movedMoved, true), iconJoin},
		{"moved out of our channel", moved(33, "23", "24", movedMoved, true), iconLeave},
		{"kicked from our channel", moved(33, "23", "24", movedKickChannel, true), iconLeave},
		{"kicked into our channel is an arrival", moved(33, "24", "23", movedKickChannel, true), iconJoin},
		{"timed out", moved(33, "23", "0", movedTimeout, false), iconLeave},
		{"banned", moved(33, "23", "0", movedBanFromServer, true), iconLeave},
		{"disconnected", moved(33, "23", "0", movedMove, false), iconLeave},
		{"we were kicked", moved(30, "23", "24", movedKickChannel, true), iconLeave},
		{"we were moved", moved(30, "23", "24", movedMoved, true), iconApp},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := run5Roster(t)
			// Put 33 in our channel first where the case needs it there.
			if tc.in != nil && strings.Contains(string(tc.in), `"oldChannelId":"23"`) {
				r.apply(moved(33, "24", "23", movedMove, false))
			}
			ns := r.apply(tc.in)
			if len(ns) != 1 {
				t.Fatalf("got %v, want one notice", lines(ns))
			}
			if ns[0].icon != tc.want {
				t.Errorf("%q icon = %q, want %q", ns[0].title, ns[0].icon, tc.want)
			}
		})
	}

	// Messages, pokes and a lost connection have no picture of their own.
	r := run5Roster(t)
	for _, m := range []json.RawMessage{
		msg(`{"type":"textMessage","payload":{"connectionId":4,"invoker":{"id":33,"nickname":"RitzeTest"},"message":"hi","targetMode":1}}`),
		msg(`{"type":"textMessage","payload":{"connectionId":4,"invoker":{"id":33,"nickname":"RitzeTest"},"message":"hi","targetMode":2}}`),
		msg(`{"type":"textMessage","payload":{"connectionId":4,"invoker":{"id":33,"nickname":"RitzeTest"},"message":"hi","targetMode":3}}`),
		msg(`{"type":"textMessage","payload":{"connectionId":4,"invoker":{"id":33,"nickname":"RitzeTest"},"message":"hi","targetMode":4}}`),
	} {
		for _, n := range r.apply(m) {
			if n.icon != iconApp {
				t.Errorf("%q icon = %q, want the app icon", n.title, n.icon)
			}
		}
	}
}

// TestRosterServerMessage: targetMode 3 is a server-wide broadcast, which used
// to be silent and now has a switch of its own.
func TestRosterServerMessage(t *testing.T) {
	r := run5Roster(t)
	got := r.apply(msg(`{"type":"textMessage","payload":{"connectionId":4,
		"invoker":{"id":33,"nickname":"RitzeTest"},"message":"reboot in 5 <min>","targetMode":3}}`))
	wantNotices(t, got, []string{"serverMsg | RitzeTest (server) | reboot in 5 &lt;min&gt;"})
}

// TestRosterLearnsChannelNamesFromTheChannelsEvent: a server we connect to
// after the snapshot sends its channel tree as a `channels` event, which is
// where "moved you to X" then gets the name from.
func TestRosterLearnsChannelNamesFromTheChannelsEvent(t *testing.T) {
	r := run5Roster(t)
	if ns := r.apply(msg(`{"type":"channels","payload":{"connectionId":4,"info":{
		"rootChannels":[{"id":"70","properties":{"name":"Lobby"}}],
		"subChannels":{"70":[{"id":"71","properties":{"name":"AFK"}}]}}}}`)); len(ns) != 0 {
		t.Errorf("the channel tree produced notices: %v", lines(ns))
	}
	got := r.apply(msg(`{"type":"clientMoved","payload":{"clientId":30,"connectionId":4,
		"invoker":{"id":33,"nickname":"RitzeTest"},"newChannelId":"71","oldChannelId":"23",
		"type":2,"visibility":1}}`))
	wantNotices(t, got, []string{"self | RitzeTest moved you to AFK"})
}
