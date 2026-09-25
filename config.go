package main

// config.go is the on-disk settings file, shared by everything that reads or
// writes it: the daemon's tray menu (tray.go) and the `ts6tray settings` TUI
// (settings.go), which run as two separate processes.
//
// One definition of the keys, the labels and the defaults lives here, so the
// menu and the TUI can never disagree about what a switch is called or what it
// falls back to. The names keep their "tray" prefix: the tray was the first
// caller and renaming them would churn every call site for nothing.

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// --- the click-target setting ----------------------------------------------

// trayConfigPath is $XDG_CONFIG_HOME/ts6tray/config, next to the API key.
func trayConfigPath() string { return filepath.Join(filepath.Dir(DefaultKeyPath()), "config") }

// The config file is a flat list of "key=value" lines:
//
//	click=mic|speaker
//	notify.<group>=on|off
//
// Anything else — blank lines, "#" comments, garbage, unknown keys — is ignored,
// and a missing key falls back to its default, so the older one-line
// "click=speaker" file still loads unchanged. Writes always rewrite the whole
// file, keeping the keys we do not own.

// trayReadConfig parses the config file into key/value pairs. A missing or
// unreadable file is an empty config, which means "all defaults".
func trayReadConfig(path string) map[string]string {
	out := map[string]string{}
	b, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		out[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return out
}

// trayWriteConfig rewrites the whole config file, creating the dir. Keys are
// sorted so the file is stable and diffable.
func trayWriteConfig(path string, kv map[string]string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	keys := make([]string, 0, len(kv))
	for k := range kv {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(kv[k])
		b.WriteByte('\n')
	}
	return os.WriteFile(path, []byte(b.String()), 0o600)
}

// trayReadClick reads the persisted left-click target. A missing, unreadable or
// unrecognised value means the default, "mic".
func trayReadClick(path string) string {
	if v := trayReadConfig(path)["click"]; v == "speaker" || v == "mic" {
		return v
	}
	return "mic"
}

// trayWriteClick persists the left-click target, leaving the notification
// switches in the file alone.
func trayWriteClick(path, click string) error {
	kv := trayReadConfig(path)
	kv["click"] = click
	return trayWriteConfig(path, kv)
}

// trayNotifyGroup is one on/off switch in the Notifications list. One switch
// can cover more than one noticeKind: "joins or leaves" is a single choice for
// the user but two kinds on the wire.
type trayNotifyGroup struct {
	key   string // config key suffix: notify.<key>
	label string
	def   bool // default when the config says nothing
	kinds []noticeKind
}

// trayNotifyGroups is the menu order, the config keys and the defaults, all in
// one place. Private messages, pokes and channel messages are off by default:
// TeamSpeak already pops up its own notification for those three.
var trayNotifyGroups = []trayNotifyGroup{
	{"joinleave", "Someone joins or leaves my channel", true, []noticeKind{noticeJoin, noticeLeave}},
	{"moved", "Someone is moved in or out", true, []noticeKind{noticeMoved}},
	{"kicked", "Someone is kicked or times out", true, []noticeKind{noticeKicked}},
	{"mute", "Someone in my channel mutes or unmutes", true, []noticeKind{noticeMute}},
	{"privateMsg", "Private messages", false, []noticeKind{noticePrivateMsg}},
	{"poke", "Pokes", false, []noticeKind{noticePoke}},
	{"channelMsg", "Channel messages", false, []noticeKind{noticeChannelMsg}},
	{"connLost", "Connection lost", true, []noticeKind{noticeConnLost}},
	// Off by default: the tray icon already says what we ourselves just did,
	// and a server-wide broadcast is rare enough that most people never want
	// it. They exist so the user *can* let ts6tray replace TeamSpeak's own
	// notifications entirely.
	{"self", "My own changes", false, []noticeKind{noticeSelf}},
	{"serverMsg", "Server messages", false, []noticeKind{noticeServerMsg}},
}

// trayNotifyOptions are the delivery switches at the bottom of the same
// list. They govern no noticeKind, so kinds is nil and notifyEnabled never
// sees them; notifyOptOn reads them instead.
const (
	trayOptBatch         = "batch"
	trayOptReplace       = "replace"
	trayOptQuietWhenDeaf = "quietWhenDeaf"
)

var trayNotifyOptions = []trayNotifyGroup{
	// Group bursts is off by default: every notice arrives on its own, as it
	// happens. Silence while deaf is on: muting the speakers is how people ask
	// to be left alone, so the notices go quiet with them.
	{trayOptBatch, "Group bursts (0.5 s)", false, nil},
	{trayOptReplace, "Replace previous notification", true, nil},
	{trayOptQuietWhenDeaf, "Silence while my speakers are muted", true, nil},
}

// trayNotifySwitches is every switch in the submenu, kinds first: what the
// config file stores and what the defaults cover.
var trayNotifySwitches = append(append([]trayNotifyGroup{}, trayNotifyGroups...), trayNotifyOptions...)

// trayNotifyKindGroup maps a noticeKind back to the switch that governs it.
var trayNotifyKindGroup = func() map[noticeKind]string {
	m := map[noticeKind]string{}
	for _, g := range trayNotifyGroups {
		for _, k := range g.kinds {
			m[k] = g.key
		}
	}
	return m
}()

// trayNotifyDefaults is a fresh copy of the default switch positions.
func trayNotifyDefaults() map[string]bool {
	m := make(map[string]bool, len(trayNotifySwitches))
	for _, g := range trayNotifySwitches {
		m[g.key] = g.def
	}
	return m
}

// trayReadNotify reads the notification switches, defaulting anything missing
// or unparsable.
func trayReadNotify(path string) map[string]bool {
	m := trayNotifyDefaults()
	kv := trayReadConfig(path)
	for _, g := range trayNotifySwitches {
		switch kv["notify."+g.key] {
		case "on":
			m[g.key] = true
		case "off":
			m[g.key] = false
		}
	}
	return m
}

// trayWriteNotify persists the notification switches, leaving click= alone.
func trayWriteNotify(path string, m map[string]bool) error {
	kv := trayReadConfig(path)
	for _, g := range trayNotifySwitches {
		v := "off"
		if m[g.key] {
			v = "on"
		}
		kv["notify."+g.key] = v
	}
	return trayWriteConfig(path, kv)
}

// --- the notification display time ------------------------------------------

// trayOptTimeout is the config key suffix of the display-time setting:
// notify.timeout. It is not a switch, so it is not in trayNotifySwitches and
// trayWriteNotify leaves it alone.
const trayOptTimeout = "timeout"

// trayNotifyTimeouts are the values the setting cycles through, in order.
// "default" hands the decision to the notification server, "never" asks it to
// leave the notification up until it is dismissed, and a number is seconds.
var trayNotifyTimeouts = []string{"default", "3", "5", "10", "30", "never"}

// trayNotifyTimeoutDef is the display time for anything the config does not
// settle: a file that predates the setting, a missing key, an unrecognised
// value. Three seconds, because the servers' own default leaves ts6tray's
// notices up for far longer than anyone wants.
const trayNotifyTimeoutDef = "3"

// trayNotifyTimeoutLabel renders one value for the UI.
func trayNotifyTimeoutLabel(v string) string {
	switch v {
	case "default", "never":
		return v
	}
	return v + " s"
}

// trayNextTimeout is the value step places from v, wrapping either way: step
// +1 is the one after it, -1 the one before. An unknown v restarts at the
// default.
func trayNextTimeout(v string, step int) string {
	n := len(trayNotifyTimeouts)
	for i, t := range trayNotifyTimeouts {
		if t == v {
			return trayNotifyTimeouts[((i+step)%n+n)%n]
		}
	}
	return trayNotifyTimeoutDef
}

// trayReadTimeout reads the display time. Anything missing or unrecognised is
// the default.
func trayReadTimeout(path string) string {
	v := trayReadConfig(path)["notify."+trayOptTimeout]
	for _, t := range trayNotifyTimeouts {
		if t == v {
			return v
		}
	}
	return trayNotifyTimeoutDef
}

// trayWriteTimeout persists the display time, leaving every other key alone.
func trayWriteTimeout(path, v string) error {
	kv := trayReadConfig(path)
	kv["notify."+trayOptTimeout] = v
	return trayWriteConfig(path, kv)
}

// trayTimeoutMillis is the value as Notify's expire_timeout: -1 lets the server
// decide, 0 means never expire, anything else is milliseconds.
func trayTimeoutMillis(v string) int32 {
	switch v {
	case "default":
		return -1
	case "never":
		return 0
	}
	if n, err := strconv.Atoi(v); err == nil && n > 0 {
		return int32(n) * 1000
	}
	n, _ := strconv.Atoi(trayNotifyTimeoutDef)
	return int32(n) * 1000
}
