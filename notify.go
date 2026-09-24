package main

// notify.go turns the TeamSpeak 6 event stream into desktop notifications about
// what happens in *our* channel and to *us*: people joining and leaving, being
// moved in or out (and by whom), kicks and timeouts, other people muting and
// unmuting, private messages, pokes, channel messages and a lost connection.
//
// It is a pure model — no D-Bus, no I/O. ts.go feeds it (auth snapshot first,
// then every message) and pushes what comes out onto TSClient.Notices(); tray.go
// decides which kinds the user wants to see and calls notify().
//
// Everything here follows docs/protocol.md, "Run 5 (2026-09-24, channel
// events)". The three facts that shape the whole file:
//
//   - clientMoved carries NO nickname, so a clientId -> nickname map has to be
//     kept, seeded from the auth snapshot and topped up from every
//     clientPropertiesUpdated and every invoker object.
//   - clientPropertiesUpdated carries the FULL property set but no old/new
//     value and no channelId, so mute changes are found by diffing against the
//     last known values, and channel membership comes only from the snapshot
//     plus clientMoved.
//   - channel ids are strings in clientMoved but may be numbers in the
//     snapshot, so they are normalised to strings (flexInt does both).

import (
	"encoding/json"
	"html"
	"strconv"
	"strings"
	"sync"
	"time"
)

// noticeKind is what happened. The tray maps kinds onto the on/off switches in
// Settings -> Notifications (see trayNotifyGroups).
type noticeKind int

const (
	noticeJoin noticeKind = iota
	noticeLeave
	noticeMoved
	noticeKicked // timeout, kick from channel or server, ban
	noticeMute   // someone else muted or unmuted their mic or speakers
	noticePrivateMsg
	noticePoke
	noticeChannelMsg
	noticeConnLost
	noticeSelf      // our own mute changes, and being moved or kicked by someone
	noticeServerMsg // a server-wide broadcast
)

func (k noticeKind) String() string {
	switch k {
	case noticeJoin:
		return "join"
	case noticeLeave:
		return "leave"
	case noticeMoved:
		return "moved"
	case noticeKicked:
		return "kicked"
	case noticeMute:
		return "mute"
	case noticePrivateMsg:
		return "privateMsg"
	case noticePoke:
		return "poke"
	case noticeChannelMsg:
		return "channelMsg"
	case noticeConnLost:
		return "connLost"
	case noticeSelf:
		return "self"
	case noticeServerMsg:
		return "serverMsg"
	}
	return "unknown"
}

// notice is one desktop notification waiting to be shown. Both are
// length-capped; the body is additionally HTML-escaped, because a freedesktop
// notification body may be interpreted as markup while the summary/title is
// always plain text (escaping it would show "Tom&#39;s" verbatim).
//
// icon names the artwork the notification server should show. The zero value,
// iconApp, is the sentinel for "our own app icon". Mute notices set it to the
// mentioned user's *resulting* state, so the picture on screen says what they
// are now rather than what our own tray shows; arrivals and departures get the
// green and red people.
type notice struct {
	kind  noticeKind
	title string
	body  string
	icon  notifyIcon
}

// notifyIcon is the artwork of one notification. It is deliberately not the
// tray's Icon enum: the tray draws states of *our* connection, while these are
// pictures of what just happened to someone. The value is the base name of the
// file in assets/notify/, and the empty string means our own app icon.
type notifyIcon string

const (
	iconApp          notifyIcon = ""
	iconMicMuted     notifyIcon = "mic-muted"
	iconSpeakerMuted notifyIcon = "speaker-muted"
	iconUnmuted      notifyIcon = "unmuted"
	iconJoin         notifyIcon = "join"
	iconLeave        notifyIcon = "leave"
)

// noticeIcon maps a client's {inputMuted, outputMuted} onto the artwork, with
// the tray's own precedence: speakers first, because a muted speaker makes a
// muted mic beside the point, then the mic, then the empty talking indicator
// the tray shows when nothing is muted.
func noticeIcon(m [2]bool) notifyIcon {
	switch {
	case m[1]:
		return iconSpeakerMuted
	case m[0]:
		return iconMicMuted
	}
	return iconUnmuted
}

// with returns the notice with its icon set. mk() leaves the icon at iconApp,
// so every site that has a better picture says so here.
func (n notice) with(ic notifyIcon) notice { n.icon = ic; return n }

// Caps on user-controlled text before it is composed (and, for bodies,
// escaped). Truncating
// the raw strings (rather than the finished title) is what keeps an escaped
// entity from being cut in half.
const (
	noticeMaxNick = 64
	noticeMaxBody = 200
)

// clientMoved.payload.type, from the client bundle. Only 1 and 2 have ever been
// observed; 3..6 are handled on the documented enum alone.
const (
	movedSubscription  = 0
	movedMove          = 1
	movedMoved         = 2
	movedTimeout       = 3
	movedKickChannel   = 4
	movedKickServer    = 5
	movedBanFromServer = 6
)

// textMessage.payload.targetMode.
const (
	targetPrivate = 1
	targetChannel = 2
	targetServer  = 3
	targetPoke    = 4
)

// rosterConn is everything the roster knows about one server connection.
type rosterConn struct {
	clientID int    // our own client id on this connection
	channel  string // the channel we are in, "" while unknown
	server   string // human-readable server name, for the title suffix

	nick  map[int]string    // clientId -> nickname
	chans map[int]string    // clientId -> channel id
	mute  map[int][2]bool   // clientId -> {inputMuted, outputMuted}
	cname map[string]string // channelId -> channel name, for "moved you to X"
}

func newRosterConn() *rosterConn {
	return &rosterConn{
		nick:  map[int]string{},
		chans: map[int]string{},
		mute:  map[int][2]bool{},
		cname: map[string]string{},
	}
}

// roster is the notification model. The zero value is ready to use; ts.go
// resets it to the zero value whenever the connection drops.
type roster struct {
	conns map[int]*rosterConn
}

func (r *roster) conn(id int) *rosterConn {
	if r.conns == nil {
		r.conns = map[int]*rosterConn{}
	}
	c := r.conns[id]
	if c == nil {
		c = newRosterConn()
		r.conns[id] = c
	}
	return c
}

// --- wire shapes ----------------------------------------------------------

type rosterProps struct {
	Nickname    string   `json:"nickname"`
	InputMuted  flexBool `json:"inputMuted"`
	OutputMuted flexBool `json:"outputMuted"`
}

type rosterInvoker struct {
	ID       flexInt `json:"id"`
	Nickname string  `json:"nickname"`
}

// rosterChannel is one entry of a channel tree, which arrives twice in the same
// shape: as connections[].channelInfos in the auth snapshot, and as the
// `channels` event's payload.info after connecting to a server. Only the name
// is of interest — it is what "X moved you to <channel>" needs.
type rosterChannel struct {
	ID         flexInt `json:"id"`
	Properties struct {
		Name string `json:"name"`
	} `json:"properties"`
}

// rosterChannelTree is {rootChannels: [...], subChannels: {parentId: [...]}}.
type rosterChannelTree struct {
	RootChannels []rosterChannel            `json:"rootChannels"`
	SubChannels  map[string][]rosterChannel `json:"subChannels"`
}

// learn records every channel name in the tree.
func (t rosterChannelTree) learn(rc *rosterConn) {
	add := func(cs []rosterChannel) {
		for _, c := range cs {
			if c.Properties.Name != "" {
				rc.cname[strconv.Itoa(int(c.ID))] = c.Properties.Name
			}
		}
	}
	add(t.RootChannels)
	for _, cs := range t.SubChannels {
		add(cs)
	}
}

// --- seeding from the auth snapshot ---------------------------------------

// auth seeds the roster from the auth reply's payload. It never produces
// notices: the snapshot is the baseline everything is diffed against.
func (r *roster) auth(payload []byte) {
	var p struct {
		Connections []struct {
			ID         flexInt `json:"id"`
			ClientID   flexInt `json:"clientId"`
			Properties struct {
				Name string `json:"name"`
			} `json:"properties"`
			ChannelInfos rosterChannelTree `json:"channelInfos"`
			ClientInfos  []struct {
				ID         flexInt     `json:"id"`
				ChannelID  flexInt     `json:"channelId"`
				Properties rosterProps `json:"properties"`
			} `json:"clientInfos"`
		} `json:"connections"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return
	}
	for _, c := range p.Connections {
		rc := r.conn(int(c.ID))
		rc.clientID = int(c.ClientID)
		rc.server = c.Properties.Name
		c.ChannelInfos.learn(rc)
		for _, ci := range c.ClientInfos {
			id := int(ci.ID)
			ch := strconv.Itoa(int(ci.ChannelID))
			if ci.Properties.Nickname != "" {
				rc.nick[id] = ci.Properties.Nickname
			}
			rc.chans[id] = ch
			rc.mute[id] = [2]bool{bool(ci.Properties.InputMuted), bool(ci.Properties.OutputMuted)}
			if id == rc.clientID {
				rc.channel = ch
			}
		}
	}
}

// --- events ---------------------------------------------------------------

// apply feeds one raw message in and returns the notices it produced, in order.
func (r *roster) apply(msg []byte) []notice {
	var env struct {
		Type    string          `json:"type"`
		Payload json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(msg, &env); err != nil || len(env.Payload) == 0 {
		return nil
	}
	switch env.Type {
	case "clientMoved":
		return r.applyMoved(env.Payload)
	case "clientPropertiesUpdated":
		return r.applyProps(env.Payload)
	case "clientSelfPropertyUpdated":
		return r.applySelfProp(env.Payload)
	case "channels":
		return r.applyChannels(env.Payload)
	case "textMessage":
		return r.applyText(env.Payload)
	case "connectStatusChanged":
		return r.applyStatus(env.Payload)
	}
	// clientChannelGroupChanged trails every clientMoved with the same
	// information and no nickname; clientChatComposing only means "typing".
	return nil
}

func (r *roster) applyMoved(raw json.RawMessage) []notice {
	var p struct {
		ConnectionID flexInt        `json:"connectionId"`
		ClientID     flexInt        `json:"clientId"`
		NewChannelID flexInt        `json:"newChannelId"`
		OldChannelID flexInt        `json:"oldChannelId"`
		Type         flexInt        `json:"type"`
		Reason       string         `json:"reason"`
		Invoker      *rosterInvoker `json:"invoker"`
		Properties   *rosterProps   `json:"properties"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil
	}
	rc := r.conn(int(p.ConnectionID))
	id := int(p.ClientID)
	oldCh, knew := rc.chans[id]

	// Full form: clientMoved may carry the moved client's own properties. For a
	// client that just connected or became visible it is the only place its
	// nickname comes from, so without this it would show up as "Client 33".
	// (Never seen in run 5, but TS6ClientViewer relies on it.)
	if p.Properties != nil {
		if p.Properties.Nickname != "" {
			rc.nick[id] = p.Properties.Nickname
		}
		rc.mute[id] = [2]bool{bool(p.Properties.InputMuted), bool(p.Properties.OutputMuted)}
	}
	if p.Invoker != nil && p.Invoker.Nickname != "" {
		rc.nick[int(p.Invoker.ID)] = p.Invoker.Nickname
	}

	newCh := strconv.Itoa(int(p.NewChannelID))
	if !knew {
		oldCh = strconv.Itoa(int(p.OldChannelID))
	}
	// newChannelId "0" is not a channel: the client left the server.
	gone := newCh == "0"
	if gone {
		delete(rc.chans, id)
		delete(rc.mute, id)
	} else {
		rc.chans[id] = newCh
	}

	// Our own move: it only relocates *us*. Everybody else stayed put, so it
	// must not read as everyone leaving and a new crowd arriving.
	if id == rc.clientID {
		rc.channel = newCh
		if gone {
			rc.channel = ""
		}
		return r.selfMoved(rc, int(p.Type), p.Invoker, newCh, escapeRunes(p.Reason, noticeMaxBody))
	}
	if rc.channel == "" {
		return nil
	}

	into := !gone && newCh == rc.channel
	out := oldCh == rc.channel
	if !gone && oldCh == newCh {
		return nil // same channel in and out: nothing moved
	}
	if !into && !out {
		return nil
	}
	who := r.nickOf(rc, id)
	body := ""
	if p.Reason != "" { // never observed; used only when actually present
		body = escapeRunes(p.Reason, noticeMaxBody)
	}

	// A client we had never seen that appears in our channel with its own
	// properties is an arrival, whatever the move type says (a newly visible
	// client arrives as a Subscription, which is otherwise silent).
	if !knew && p.Properties != nil && into {
		return []notice{r.mk(rc, noticeJoin, who+" joined your channel", "").with(iconJoin)}
	}
	if int(p.Type) == movedSubscription {
		return nil // subscription churn is not a join
	}

	// Left the server altogether. The move type still says how.
	if gone {
		switch int(p.Type) {
		case movedTimeout:
			return []notice{r.mk(rc, noticeKicked, who+" timed out", body).with(iconLeave)}
		case movedKickServer:
			return []notice{r.mk(rc, noticeKicked, who+" was kicked from the server", body).with(iconLeave)}
		case movedBanFromServer:
			return []notice{r.mk(rc, noticeKicked, who+" was banned from the server", body).with(iconLeave)}
		}
		return []notice{r.mk(rc, noticeLeave, who+" disconnected", body).with(iconLeave)}
	}

	switch int(p.Type) {
	case movedMove:
		if into {
			return []notice{r.mk(rc, noticeJoin, who+" joined your channel", "").with(iconJoin)}
		}
		return []notice{r.mk(rc, noticeLeave, who+" left your channel", "").with(iconLeave)}
	case movedMoved:
		by := "Someone"
		if p.Invoker != nil {
			by = r.nickOf(rc, int(p.Invoker.ID))
		}
		if into {
			return []notice{r.mk(rc, noticeMoved, by+" moved "+who+" into your channel", body).with(iconJoin)}
		}
		return []notice{r.mk(rc, noticeMoved, by+" moved "+who+" out of your channel", body).with(iconLeave)}
	case movedTimeout, movedKickChannel, movedKickServer, movedBanFromServer:
		if !out {
			// A kick can land someone *in* our channel (the default channel);
			// from our side that is simply an arrival.
			return []notice{r.mk(rc, noticeJoin, who+" joined your channel", "").with(iconJoin)}
		}
		var line string
		switch int(p.Type) {
		case movedTimeout:
			line = who + " timed out"
		case movedKickChannel:
			line = who + " was kicked from the channel"
		case movedKickServer:
			line = who + " was kicked from the server"
		default:
			line = who + " was banned from the server"
		}
		return []notice{r.mk(rc, noticeKicked, line, body).with(iconLeave)}
	}
	return nil
}

func (r *roster) applyProps(raw json.RawMessage) []notice {
	var p struct {
		ConnectionID flexInt     `json:"connectionId"`
		ClientID     flexInt     `json:"clientId"`
		Properties   rosterProps `json:"properties"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil
	}
	rc := r.conn(int(p.ConnectionID))
	id := int(p.ClientID)
	if p.Properties.Nickname != "" {
		rc.nick[id] = p.Properties.Nickname
	}

	now := [2]bool{bool(p.Properties.InputMuted), bool(p.Properties.OutputMuted)}
	was, known := rc.mute[id]
	rc.mute[id] = now
	if !known {
		return nil // no baseline to diff against
	}
	if id == rc.clientID {
		// Our own mute changes are a switch of their own ("My own changes"),
		// and rc.mute is the single baseline both this event and
		// clientSelfPropertyUpdated diff against — which is what keeps the two
		// of them from announcing the same flip twice.
		return r.selfMute(rc, was, now)
	}
	// No channelId in this event: membership comes from the snapshot plus
	// clientMoved only (channelGroupInheritedChannelId is a permission field).
	if rc.channel == "" || rc.chans[id] != rc.channel {
		return nil
	}
	who := r.nickOf(rc, id)
	ic := noticeIcon(now)
	var out []notice
	// A mic change only matters to us while they can hear us: someone whose
	// speakers are muted is not in the conversation either way, and TeamSpeak
	// mutes their mic implicitly in that state, so their mic flapping is
	// nothing that happened to us. That covers the "muted before and after"
	// case (was[1] && now[1]) and the two combined changes, where only the
	// speaker notice — the one that says whether they are back — is shown.
	if was[0] != now[0] && !now[1] && !was[1] {
		out = append(out, r.mk(rc, noticeMute, who+" "+mutedWord(now[0])+" their microphone", "").with(ic))
	}
	if was[1] != now[1] {
		out = append(out, r.mk(rc, noticeMute, who+" "+mutedWord(now[1])+" their speakers", "").with(ic))
	}
	return out
}

// applySelfProp handles clientSelfPropertyUpdated, the *other* event that
// reports our own mute state. It carries one flag with its new value, and it
// arrives alongside a clientPropertiesUpdated for our own clientId saying the
// same thing — in either order. Both write the same rc.mute[clientID] baseline
// and both notify only on a real difference, so whichever lands first speaks
// and the second one sees nothing to report.
func (r *roster) applySelfProp(raw json.RawMessage) []notice {
	var p struct {
		ConnectionID flexInt  `json:"connectionId"`
		Flag         string   `json:"flag"`
		NewValue     flexBool `json:"newValue"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil
	}
	i := 0
	switch p.Flag {
	case "inputMuted":
	case "outputMuted":
		i = 1
	default:
		return nil // the other self flags are not about muting
	}
	rc := r.conn(int(p.ConnectionID))
	if rc.clientID == 0 {
		return nil // we do not know who we are yet
	}
	was, known := rc.mute[rc.clientID]
	now := was
	now[i] = bool(p.NewValue)
	rc.mute[rc.clientID] = now
	if !known {
		return nil
	}
	return r.selfMute(rc, was, now)
}

// selfMute turns a change in our own {inputMuted, outputMuted} into notices,
// with the same resulting-state icon other people's mute notices get.
func (r *roster) selfMute(rc *rosterConn, was, now [2]bool) []notice {
	ic := noticeIcon(now)
	var out []notice
	if was[0] != now[0] {
		out = append(out, r.mk(rc, noticeSelf, "You "+mutedWord(now[0])+" your microphone", "").with(ic))
	}
	if was[1] != now[1] {
		out = append(out, r.mk(rc, noticeSelf, "You "+mutedWord(now[1])+" your speakers", "").with(ic))
	}
	return out
}

// selfMoved reports what someone else did to us. Our own moves (type 1) are
// silent: we just did them, we know. A type 2 move only counts with an invoker
// that is not us, since the client sends type 2 for a move we made ourselves
// through the server as well.
func (r *roster) selfMoved(rc *rosterConn, typ int, invoker *rosterInvoker, newCh, body string) []notice {
	switch typ {
	case movedMoved:
		if invoker == nil || int(invoker.ID) == rc.clientID {
			return nil
		}
		where := "another channel"
		if n := rc.cname[newCh]; n != "" {
			where = truncRunes(n, noticeMaxNick)
		}
		return []notice{r.mk(rc, noticeSelf, r.nickOf(rc, int(invoker.ID))+" moved you to "+where, body)}
	case movedTimeout:
		return []notice{r.mk(rc, noticeSelf, "You timed out", body).with(iconLeave)}
	case movedKickChannel:
		return []notice{r.mk(rc, noticeSelf, "You were kicked from the channel", body).with(iconLeave)}
	case movedKickServer:
		return []notice{r.mk(rc, noticeSelf, "You were kicked from the server", body).with(iconLeave)}
	case movedBanFromServer:
		return []notice{r.mk(rc, noticeSelf, "You were banned from the server", body).with(iconLeave)}
	}
	return nil
}

// applyChannels records the channel names of a newly connected server. It never
// notifies; it only feeds "X moved you to <channel>".
func (r *roster) applyChannels(raw json.RawMessage) []notice {
	var p struct {
		ConnectionID flexInt           `json:"connectionId"`
		Info         rosterChannelTree `json:"info"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil
	}
	p.Info.learn(r.conn(int(p.ConnectionID)))
	return nil
}

func mutedWord(muted bool) string {
	if muted {
		return "muted"
	}
	return "unmuted"
}

func (r *roster) applyText(raw json.RawMessage) []notice {
	var p struct {
		ConnectionID flexInt        `json:"connectionId"`
		Invoker      *rosterInvoker `json:"invoker"`
		Message      string         `json:"message"`
		TargetMode   flexInt        `json:"targetMode"`
	}
	if err := json.Unmarshal(raw, &p); err != nil || p.Invoker == nil {
		return nil
	}
	rc := r.conn(int(p.ConnectionID))
	from := int(p.Invoker.ID)
	if p.Invoker.Nickname != "" {
		rc.nick[from] = p.Invoker.Nickname
	}
	if from == rc.clientID {
		return nil // our own message echoed back
	}
	who := r.nickOf(rc, from)
	body := escapeRunes(p.Message, noticeMaxBody)

	switch int(p.TargetMode) {
	case targetPrivate:
		return []notice{r.mk(rc, noticePrivateMsg, "Message from "+who, body)}
	case targetPoke:
		return []notice{r.mk(rc, noticePoke, who+" poked you", body)}
	case targetChannel:
		return []notice{r.mk(rc, noticeChannelMsg, who+" in channel", body)}
	case targetServer:
		// A server-wide broadcast. Never observed in a capture, so the wording
		// names the sender and says where it came from, nothing more.
		return []notice{r.mk(rc, noticeServerMsg, who+" (server)", body)}
	}
	return nil
}

func (r *roster) applyStatus(raw json.RawMessage) []notice {
	var p struct {
		ConnectionID flexInt `json:"connectionId"`
		Status       flexInt `json:"status"`
		Error        flexInt `json:"error"`
		Info         *struct {
			ClientID   flexInt `json:"clientId"`
			ServerName string  `json:"serverName"`
		} `json:"info"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil
	}
	rc := r.conn(int(p.ConnectionID))
	if p.Info != nil {
		if p.Info.ServerName != "" {
			rc.server = p.Info.ServerName
		}
		if int(p.Info.ClientID) != 0 {
			rc.clientID = int(p.Info.ClientID)
		}
	}
	if int(p.Status) != StatusDisconnected {
		return nil
	}
	// A clean disconnect (the user pressed Disconnect) carries error 0 and is
	// not worth a notification; only a real drop has error != 0.
	lost := int(p.Error) != 0
	name := rc.server
	// The connection is gone: forget who was where, keep the server name so a
	// later reconnect still has something to say.
	rc.channel = ""
	rc.chans = map[int]string{}
	rc.mute = map[int][2]bool{}
	if !lost {
		return nil
	}
	where := truncRunes(name, noticeMaxNick)
	if where == "" {
		where = "the server"
	}
	// No suffix here: the server is already named in the line.
	return []notice{{kind: noticeConnLost, title: "Lost connection to " + where}}
}

// --- helpers --------------------------------------------------------------

// nickOf returns a capped, unescaped display name for a client id, falling back
// to "Client <id>" when no event has ever carried the nickname.
func (r *roster) nickOf(rc *rosterConn, id int) string {
	if n := rc.nick[id]; n != "" {
		return truncRunes(n, noticeMaxNick)
	}
	return "Client " + strconv.Itoa(id)
}

// mk builds a notice, appending " — <server>" to the title while more than one
// connection is live so the user knows which server it came from.
func (r *roster) mk(rc *rosterConn, kind noticeKind, title, body string) notice {
	if len(r.conns) > 1 && rc.server != "" {
		title += " — " + truncRunes(rc.server, noticeMaxNick)
	}
	return notice{kind: kind, title: title, body: body}
}

// truncRunes caps s at max runes, marking a cut with an ellipsis. Titles use
// it on its own: a notification summary is plain text, never markup.
func truncRunes(s string, max int) string {
	rs := []rune(s)
	if len(rs) > max {
		return string(rs[:max]) + "…"
	}
	return s
}

// escapeRunes caps s at max runes and HTML-escapes it, for body text only.
// Capping first is what keeps an escape sequence from being cut in half.
func escapeRunes(s string, max int) string {
	return html.EscapeString(truncRunes(s, max))
}

// --- batching -------------------------------------------------------------

// Batching exists because one moderator action can move five people at once:
// five notifications in a row is spam, one with five lines is a summary. It is
// a sliding window — every new notice pushes the flush out again — bounded by a
// hard cap so a continuous stream cannot postpone it forever.
const (
	noticeBatchWindow = 500 * time.Millisecond
	noticeBatchCap    = 3 * time.Second

	// noticeBatchLines is how many notices a flushed body lists before it
	// summarises the rest as "…and N more".
	noticeBatchLines = 10
)

// noticeBatcher collects notices and flushes them as one notification. window
// and cap are fields rather than constants so tests can shorten them. send is
// called off the caller's goroutine (from the timer), never under the lock.
type noticeBatcher struct {
	window time.Duration
	cap    time.Duration
	send   func(title, body string, icon notifyIcon)

	mu      sync.Mutex
	pending []notice
	first   time.Time
	timer   *time.Timer
	gen     uint64 // invalidates a timer callback that already got past Stop
}

// add queues one notice and (re)arms the flush timer.
func (b *noticeBatcher) add(n notice) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pending = append(b.pending, n)
	if len(b.pending) == 1 {
		b.first = time.Now()
	}
	d := b.window
	// ponytail: hard cap noticeBatchCap (3 s) from the first notice of the
	// batch. Without it a steady trickle of events at under half a second
	// apart would slide the window forever and nothing would ever be shown.
	if rest := time.Until(b.first.Add(b.cap)); rest < d {
		d = max(rest, 0)
	}
	b.gen++
	gen := b.gen
	if b.timer != nil {
		b.timer.Stop()
	}
	b.timer = time.AfterFunc(d, func() { b.fire(gen) })
}

// fire flushes the batch unless a newer add has already re-armed the timer.
func (b *noticeBatcher) fire(gen uint64) {
	b.mu.Lock()
	if gen != b.gen {
		b.mu.Unlock()
		return
	}
	b.gen++
	ns := b.pending
	b.pending = nil
	b.mu.Unlock()
	b.deliver(ns)
}

// flush sends whatever is pending right now. RunTray calls it on shutdown, so
// events that arrived in the last half second are still shown rather than lost,
// and toggleNotify calls it when batching is switched off mid-burst.
func (b *noticeBatcher) flush() {
	b.mu.Lock()
	if b.timer != nil {
		b.timer.Stop()
	}
	b.gen++
	ns := b.pending
	b.pending = nil
	b.mu.Unlock()
	b.deliver(ns)
}

func (b *noticeBatcher) deliver(ns []notice) {
	if len(ns) == 0 || b.send == nil {
		return
	}
	title, body, icon := flattenNotices(ns)
	b.send(title, body, icon)
}

// flattenNotices composes one notification out of a batch. A single notice
// passes through untouched; several become a counted title and one numbered
// line each, newest first — the thing that just happened is what the user is
// looking for, and a notification is read from the top.
//
// Because the newest is line 1, the lines that fall off the ten-line cap are
// the *oldest* ones, which the last line says in so many words.
//
// The icon returned is the newest notice's, the one shown as line 1.
//
// The titles are plain text but the body is markup, so every title has to be
// escaped on its way into the body. The bodies already are (escapeRunes).
func flattenNotices(ns []notice) (title, body string, icon notifyIcon) {
	if len(ns) == 0 {
		return "", "", iconApp
	}
	if len(ns) == 1 {
		return ns[0].title, ns[0].body, ns[0].icon
	}
	title = strconv.Itoa(len(ns)) + " TeamSpeak events"
	if s := noticeServerSuffix(ns); s != "" {
		title += " — " + s
	}

	// Newest first: walk the arrival order backwards. Anything past the cap is
	// what arrived earliest, so it is dropped from the tail of that walk.
	shown, extra := len(ns), 0
	if shown > noticeBatchLines {
		extra = shown - noticeBatchLines
		shown = noticeBatchLines
	}
	var b strings.Builder
	for i := 0; i < shown; i++ {
		n := ns[len(ns)-1-i]
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(strconv.Itoa(i + 1))
		b.WriteString(". ")
		b.WriteString(html.EscapeString(n.title))
		if n.body != "" {
			b.WriteString(": ")
			b.WriteString(n.body)
		}
	}
	if extra > 0 {
		b.WriteString("\n…and " + strconv.Itoa(extra) + " more earlier")
	}
	return title, b.String(), ns[len(ns)-1].icon
}

// noticeServerSuffix returns the " — <server>" tail every notice in the batch
// shares, or "" when they differ or none has one. mk() appends that tail while
// more than one connection is live, and hoisting it into the batch title keeps
// it from being repeated on every line.
func noticeServerSuffix(ns []notice) string {
	const sep = " — "
	i := strings.LastIndex(ns[0].title, sep)
	if i < 0 {
		return ""
	}
	want := ns[0].title[i+len(sep):]
	for _, n := range ns[1:] {
		j := strings.LastIndex(n.title, sep)
		if j < 0 || n.title[j+len(sep):] != want {
			return ""
		}
	}
	return want
}
