package main

// state.go holds the pure state model for ts6tray: no IO, no goroutines, no
// time. Feed it the auth reply payload with ApplyAuth and every subsequent raw
// WebSocket envelope with Apply; ask it for Conns, Active and Icon.
//
// See docs/protocol.md for the protocol this models.

import (
	"encoding/json"
	"sort"
	"strconv"
)

// Connect statuses (connectStatusChanged.payload.status and
// connections[].status). Only 0..4 were ever observed.
const (
	StatusDisconnected           = 0
	StatusConnecting             = 1
	StatusConnected              = 2
	StatusConnectionEstablishing = 3
	StatusConnectionEstablished  = 4
)

// talkStatusChanged.payload.status.
const (
	TalkNotTalking           = 0
	TalkTalking              = 1
	TalkTalkingWhileDisabled = 2
)

// Conn is one TeamSpeak server connection and our own audio state on it.
type Conn struct {
	ID         int    // connections[].id == connectionId in every event
	ServerUID  string // properties.uniqueIdentifier / info.serverUid
	ServerName string // properties.name / info.serverName
	ClientID   int    // our own client id on this connection
	Status     int    // last known connect status

	InputMuted       bool
	OutputMuted      bool
	InputDeactivated bool
	InputHardware    bool // capture device open: true on exactly one conn (D6)
	Talking          bool
}

// Live reports whether the connection is fully established.
func (c Conn) Live() bool { return c.Status == StatusConnectionEstablished }

// MicDisabled reports whether this connection cannot transmit: either the
// input is deactivated or the capture device belongs to another connection.
func (c Conn) MicDisabled() bool { return c.InputDeactivated || !c.InputHardware }

// Icon is the tray icon to show.
type Icon int

const (
	IconNone Icon = iota // no live connection
	IconQuiet
	IconTalking
	IconMicMuted
	IconSpeakerMuted
	IconMicDisabled
)

func (i Icon) String() string {
	switch i {
	case IconNone:
		return "none"
	case IconQuiet:
		return "quiet"
	case IconTalking:
		return "talking"
	case IconMicMuted:
		return "mic-muted"
	case IconSpeakerMuted:
		return "speaker-muted"
	case IconMicDisabled:
		return "mic-disabled"
	}
	return "unknown(" + strconv.Itoa(int(i)) + ")"
}

// State is the whole picture: every connection TeamSpeak has told us about.
// The zero value is ready to use. It is not safe for concurrent use; the
// daemon owns it from a single goroutine.
type State struct {
	conns map[int]*Conn

	// pending buffers properties that arrived for a connection whose own
	// client id we do not know yet, keyed connectionId -> clientId. See
	// applyClientProperties.
	pending map[int]map[int]clientProps
}

// maxPendingClients caps a connection's pending bucket. On a busy server every
// visible client sends full-form properties, and a connection that never
// reaches status 2 would otherwise hold them forever.
const maxPendingClients = 128

// --- JSON helpers ---------------------------------------------------------

// flexInt is an int that also accepts a JSON string, because protocol.md shows
// ids in both forms (connectionId/clientId are numbers, channelId is a string
// like "16292" or "0").
type flexInt int

func (f *flexInt) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		if s == "" {
			*f = 0
			return nil
		}
		n, err := strconv.Atoi(s)
		if err != nil {
			return err
		}
		*f = flexInt(n)
		return nil
	}
	if string(b) == "null" {
		*f = 0
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(b, &n); err != nil {
		return err
	}
	i, err := n.Int64()
	if err != nil {
		// Tolerate 1.0-style numbers.
		fl, ferr := n.Float64()
		if ferr != nil {
			return err
		}
		i = int64(fl)
	}
	*f = flexInt(i)
	return nil
}

// flexBool is a bool that also accepts "true"/"false"/0/1.
type flexBool bool

func (f *flexBool) UnmarshalJSON(b []byte) error {
	switch string(b) {
	case "true", `"true"`, "1", `"1"`:
		*f = true
		return nil
	case "false", `"false"`, "0", `"0"`, "null":
		*f = false
		return nil
	}
	var v bool
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	*f = flexBool(v)
	return nil
}

// clientProps is the subset of a client properties object we care about. Every
// field is a pointer so a partial object merges instead of clobbering.
type clientProps struct {
	InputMuted       *flexBool `json:"inputMuted"`
	OutputMuted      *flexBool `json:"outputMuted"`
	InputDeactivated *flexBool `json:"inputDeactivated"`
	InputHardware    *flexBool `json:"inputHardware"`
	FlagTalking      *flexBool `json:"flagTalking"`
}

// merge applies the fields that were present and reports whether anything moved.
func (p clientProps) merge(c *Conn) bool {
	changed := false
	set := func(dst *bool, src *flexBool) {
		if src != nil && *dst != bool(*src) {
			*dst = bool(*src)
			changed = true
		}
	}
	set(&c.InputMuted, p.InputMuted)
	set(&c.OutputMuted, p.OutputMuted)
	set(&c.InputDeactivated, p.InputDeactivated)
	set(&c.InputHardware, p.InputHardware)
	set(&c.Talking, p.FlagTalking)
	return changed
}

// overlay copies the fields that were present in p onto dst, so successive
// partial property objects for the same client accumulate while buffered.
func (p clientProps) overlay(dst *clientProps) {
	if p.InputMuted != nil {
		dst.InputMuted = p.InputMuted
	}
	if p.OutputMuted != nil {
		dst.OutputMuted = p.OutputMuted
	}
	if p.InputDeactivated != nil {
		dst.InputDeactivated = p.InputDeactivated
	}
	if p.InputHardware != nil {
		dst.InputHardware = p.InputHardware
	}
	if p.FlagTalking != nil {
		dst.FlagTalking = p.FlagTalking
	}
}

// resetAudio clears the five per-session audio flags and reports whether any
// of them was set.
func resetAudio(c *Conn) bool {
	changed := c.InputMuted || c.OutputMuted || c.InputDeactivated || c.InputHardware || c.Talking
	c.InputMuted = false
	c.OutputMuted = false
	c.InputDeactivated = false
	c.InputHardware = false
	c.Talking = false
	return changed
}

// setFlag applies one named flag (clientSelfPropertyUpdated). Unknown flag
// names are ignored.
func setFlag(c *Conn, flag string, v bool) bool {
	var dst *bool
	switch flag {
	case "inputMuted":
		dst = &c.InputMuted
	case "outputMuted":
		dst = &c.OutputMuted
	case "inputDeactivated":
		dst = &c.InputDeactivated
	case "inputHardware":
		dst = &c.InputHardware
	case "flagTalking":
		dst = &c.Talking
	default:
		return false
	}
	if *dst == v {
		return false
	}
	*dst = v
	return true
}

// --- auth snapshot --------------------------------------------------------

type authConn struct {
	ID         flexInt `json:"id"`
	ClientID   flexInt `json:"clientId"`
	Status     flexInt `json:"status"`
	Properties struct {
		UniqueIdentifier string `json:"uniqueIdentifier"`
		Name             string `json:"name"`
	} `json:"properties"`
	ClientInfos []struct {
		ID         flexInt     `json:"id"`
		Properties clientProps `json:"properties"`
	} `json:"clientInfos"`
}

type authPayload struct {
	APIKey      string     `json:"apiKey"`
	Connections []authConn `json:"connections"`
	// Present only when a whole auth envelope was handed to us instead of the
	// payload; see ApplyAuth.
	Payload *authPayload `json:"payload"`
}

// ApplyAuth resets the state from the auth reply's payload. It also accepts a
// whole auth envelope, so callers cannot get it subtly wrong. Only
// ConnectionEstablished connections are kept.
func (s *State) ApplyAuth(payload []byte) error {
	var p authPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return err
	}
	if p.Connections == nil && p.Payload != nil {
		p = *p.Payload
	}
	s.conns = make(map[int]*Conn, len(p.Connections))
	s.pending = nil
	for _, ac := range p.Connections {
		if int(ac.Status) != StatusConnectionEstablished {
			continue
		}
		c := &Conn{
			ID:         int(ac.ID),
			ServerUID:  ac.Properties.UniqueIdentifier,
			ServerName: ac.Properties.Name,
			ClientID:   int(ac.ClientID),
			Status:     int(ac.Status),
		}
		for _, ci := range ac.ClientInfos {
			if ci.ID == ac.ClientID {
				ci.Properties.merge(c)
				break
			}
		}
		s.conns[c.ID] = c
	}
	return nil
}

// --- events ---------------------------------------------------------------

type envelope struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

// Apply feeds one raw WebSocket envelope into the state and reports whether
// anything we care about changed. Unknown or irrelevant messages, and
// malformed JSON, return false without error.
func (s *State) Apply(msg []byte) (changed bool) {
	var env envelope
	if err := json.Unmarshal(msg, &env); err != nil || len(env.Payload) == 0 {
		return false
	}
	switch env.Type {
	case "connectStatusChanged":
		return s.applyConnectStatus(env.Payload)
	case "clientPropertiesUpdated", "clientMoved":
		return s.applyClientProperties(env.Payload)
	case "clientSelfPropertyUpdated":
		return s.applySelfProperty(env.Payload)
	case "talkStatusChanged":
		return s.applyTalkStatus(env.Payload)
	}
	return false
}

func (s *State) get(id int) *Conn {
	if s.conns == nil {
		return nil
	}
	return s.conns[id]
}

func (s *State) ensure(id int) *Conn {
	if s.conns == nil {
		s.conns = make(map[int]*Conn)
	}
	c := s.conns[id]
	if c == nil {
		c = &Conn{ID: id}
		s.conns[id] = c
	}
	return c
}

// stash buffers one client's properties on a connection whose own client id is
// not known yet.
func (s *State) stash(connID, clientID int, p clientProps) {
	if s.pending == nil {
		s.pending = make(map[int]map[int]clientProps)
	}
	byClient := s.pending[connID]
	if byClient == nil {
		byClient = make(map[int]clientProps)
		s.pending[connID] = byClient
	}
	cur, seen := byClient[clientID]
	if !seen && len(byClient) >= maxPendingClients {
		return
	}
	p.overlay(&cur)
	byClient[clientID] = cur
}

// dropPending forgets everything buffered for a connection.
func (s *State) dropPending(connID int) {
	delete(s.pending, connID)
	if len(s.pending) == 0 {
		s.pending = nil
	}
}

// flushPending applies the buffered properties that belong to c (matched on
// the client id we have just learned) and discards the rest, which belonged to
// other clients.
func (s *State) flushPending(c *Conn) bool {
	if s.pending == nil || c.ClientID == 0 {
		return false
	}
	byClient, ok := s.pending[c.ID]
	if !ok {
		return false
	}
	s.dropPending(c.ID)
	p, ok := byClient[c.ClientID]
	if !ok {
		return false
	}
	return p.merge(c)
}

func (s *State) applyConnectStatus(raw json.RawMessage) bool {
	var p struct {
		ConnectionID flexInt `json:"connectionId"`
		Status       flexInt `json:"status"`
		Info         *struct {
			ClientID   flexInt `json:"clientId"`
			ServerUID  string  `json:"serverUid"`
			ServerName string  `json:"serverName"`
		} `json:"info"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return false
	}
	id := int(p.ConnectionID)

	if int(p.Status) == StatusDisconnected {
		s.dropPending(id)
		if s.get(id) == nil {
			return false
		}
		delete(s.conns, id)
		return true
	}

	// status 1 (Connecting) carries no info and nothing worth tracking yet.
	if int(p.Status) == StatusConnecting && s.get(id) == nil {
		return false
	}

	c := s.ensure(id)
	changed := c.Status != int(p.Status)
	// A drop from Established back to a lower non-zero status is a reconnect
	// with no Disconnected in between (4 -> 1/2/3 -> 4). The audio flags and
	// our client id describe the session that just ended; keeping them would
	// show stale mute/talk state, and a stale client id would make us reject
	// the new session's own properties. Server identity is re-sent at status 2
	// and names the same server, so it stays.
	if c.Status == StatusConnectionEstablished && int(p.Status) != StatusConnectionEstablished {
		if resetAudio(c) {
			changed = true
		}
		c.ClientID = 0
		s.dropPending(id)
	}
	c.Status = int(p.Status)
	if p.Info != nil {
		// Full info (serverUid/serverName) arrives only at status 2; statuses
		// 3 and 4 carry just clientId.
		if p.Info.ClientID != 0 && c.ClientID != int(p.Info.ClientID) {
			c.ClientID = int(p.Info.ClientID)
			changed = true
		}
		if p.Info.ServerUID != "" && c.ServerUID != p.Info.ServerUID {
			c.ServerUID = p.Info.ServerUID
			changed = true
		}
		if p.Info.ServerName != "" && c.ServerName != p.Info.ServerName {
			c.ServerName = p.Info.ServerName
			changed = true
		}
	}
	// Now that our client id may be known, apply anything that arrived early.
	if s.flushPending(c) {
		changed = true
	}
	return changed
}

// applyClientProperties handles clientPropertiesUpdated and the full form of
// clientMoved. Both carry a properties object for one client on one
// connection; only our own client is interesting. A clientMoved without
// properties is a plain channel switch and is ignored.
func (s *State) applyClientProperties(raw json.RawMessage) bool {
	var p struct {
		ConnectionID flexInt      `json:"connectionId"`
		ClientID     flexInt      `json:"clientId"`
		Properties   *clientProps `json:"properties"`
	}
	if err := json.Unmarshal(raw, &p); err != nil || p.Properties == nil {
		return false
	}
	id := int(p.ConnectionID)
	c := s.get(id)
	if c == nil || c.ClientID == 0 {
		// We do not know our own client id on this connection yet, so we
		// cannot tell our own properties from another client's — and the
		// fixtures show other clients' full-form events are the common case
		// (testdata/events.jsonl lines 13/43/104, events_run2_talk_only.jsonl
		// lines 4/5/10/43/48-50 are all other clients on connection 1).
		// Adopting the first one would be wrong most of the time, so buffer it
		// keyed by client id; connectStatusChanged reveals our id at status
		// 2/3/4 and flushPending then applies the matching one. This is what
		// makes our own initial full-form clientMoved on a new server survive
		// when it overtakes the status message (D6).
		s.stash(id, int(p.ClientID), *p.Properties)
		return false
	}
	if c.ClientID != int(p.ClientID) {
		return false
	}
	return p.Properties.merge(c)
}

func (s *State) applySelfProperty(raw json.RawMessage) bool {
	var p struct {
		ConnectionID flexInt         `json:"connectionId"`
		Flag         string          `json:"flag"`
		NewValue     json.RawMessage `json:"newValue"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return false
	}
	c := s.get(int(p.ConnectionID))
	if c == nil {
		return false
	}
	var v flexBool
	if err := json.Unmarshal(p.NewValue, &v); err != nil {
		// userTag / metaData and friends are strings; not our business.
		return false
	}
	return setFlag(c, p.Flag, bool(v))
}

func (s *State) applyTalkStatus(raw json.RawMessage) bool {
	var p struct {
		ConnectionID flexInt `json:"connectionId"`
		ClientID     flexInt `json:"clientId"`
		Status       flexInt `json:"status"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return false
	}
	c := s.get(int(p.ConnectionID))
	if c == nil || c.ClientID != int(p.ClientID) {
		return false
	}
	talking := int(p.Status) == TalkTalking
	if c.Talking == talking {
		return false
	}
	c.Talking = talking
	return true
}

// --- queries --------------------------------------------------------------

// Conns returns the live connections, sorted by id.
func (s *State) Conns() []Conn {
	out := make([]Conn, 0, len(s.conns))
	for _, c := range s.conns {
		if c.Live() {
			out = append(out, *c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Active returns the live connection that owns the capture device (D6): the
// one with InputHardware set and input not deactivated. At most one connection
// has it. If several somehow do, the lowest id wins, so the result is stable.
func (s *State) Active() (Conn, bool) {
	for _, c := range s.Conns() {
		if c.InputHardware && !c.InputDeactivated {
			return c, true
		}
	}
	return Conn{}, false
}

// Icon derives the tray icon from the active connection.
func (s *State) Icon() Icon {
	if len(s.Conns()) == 0 {
		return IconNone
	}
	c, ok := s.Active()
	if !ok {
		// Live connections exist but none owns the mic.
		return IconMicDisabled
	}
	switch {
	case c.OutputMuted:
		return IconSpeakerMuted
	case c.InputMuted:
		return IconMicMuted
	case c.Talking:
		return IconTalking
	}
	return IconQuiet
}
