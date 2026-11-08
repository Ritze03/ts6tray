package main

import (
	"encoding/json"
	"fmt"
	"testing"
)

// queryRoster is a roster with us (30) and normal Pal (41) in channel 2 and
// "Robot" (50), a server query client (properties.type 1) from the auth
// snapshot, with the hide switch on.
func queryRoster(t *testing.T) *roster {
	t.Helper()
	var r roster
	r.auth([]byte(`{"connections":[{"id":4,"clientId":30,"properties":{"name":"S"},
		"channelInfos":{"rootChannels":[{"id":1,"properties":{"name":"Lobby"}},{"id":2,"properties":{"name":"Talk"}}]},
		"clientInfos":[{"id":30,"channelId":2,"properties":{"nickname":"Me","type":0}},
			{"id":41,"channelId":2,"properties":{"nickname":"Pal","type":0}},
			{"id":50,"channelId":2,"properties":{"nickname":"Robot","type":1}}]}]}`))
	r.hideQuery = true
	return &r
}

func queryText(id int, nick string) json.RawMessage {
	return msg(fmt.Sprintf(`{"type":"textMessage","payload":{"connectionId":4,"targetMode":2,
		"message":"moin","targetId":0,"invoker":{"id":%d,"nickname":%q}}}`, id, nick))
}

// TestRosterHideQueryFromAuth: a query client known from the auth snapshot
// makes no notice (message, move), while a normal client, whose moves carry
// the move type 1 too, does; with the switch off nothing is hidden.
func TestRosterHideQueryFromAuth(t *testing.T) {
	r := queryRoster(t)
	wantNotices(t, r.apply(queryText(50, "Robot")), []string{})
	wantNotices(t, r.apply(alarmMove(50, "2", "1")), []string{})
	wantNotices(t, r.apply(alarmMove(41, "2", "1")), []string{"leave | Pal left your channel"})
	wantNotices(t, r.apply(queryText(41, "Pal")), []string{"channelMsg | Pal in channel | moin"})

	r = queryRoster(t)
	r.hideQuery = false
	wantNotices(t, r.apply(queryText(50, "Robot")), []string{"channelMsg | Robot in channel | moin"})
	wantNotices(t, r.apply(alarmMove(50, "2", "1")), []string{"leave | Robot left your channel"})
}

// TestRosterHideQueryFromJoin: a query client that joins later is recognised by
// the properties on its join and filtered by id afterwards; once it has left,
// the id is forgotten and a normal client reusing it is announced again.
func TestRosterHideQueryFromJoin(t *testing.T) {
	r := queryRoster(t)
	join := func(id, typ int, nick string) json.RawMessage {
		return msg(fmt.Sprintf(`{"type":"clientMoved","payload":{"clientId":%d,"connectionId":4,"hotReload":false,
			"newChannelId":"2","oldChannelId":"0","type":1,"visibility":0,
			"properties":{"nickname":%q,"type":%d}}}`, id, nick, typ))
	}
	wantNotices(t, r.apply(join(60, 1, "Bot2")), []string{})
	wantNotices(t, r.apply(queryText(60, "Bot2")), []string{})
	wantNotices(t, r.apply(alarmMove(60, "2", "1")), []string{})
	wantNotices(t, r.apply(msg(`{"type":"clientMoved","payload":{"clientId":60,"connectionId":4,
		"newChannelId":"0","oldChannelId":"1","type":3,"visibility":2}}`)), []string{})
	wantNotices(t, r.apply(join(60, 0, "Human")), []string{"join | Human joined your channel"})
	wantNotices(t, r.apply(queryText(60, "Human")), []string{"channelMsg | Human in channel | moin"})
}

// TestRosterHideQueryKeepsAlarm: a query client named in alarm.nick still
// sounds the alarm.
func TestRosterHideQueryKeepsAlarm(t *testing.T) {
	r := queryRoster(t)
	w := alarmWatch{nick: "Robot", channel: "Lobby"}
	wantNotices(t, step(r, w, alarmMove(50, "2", "1")), []string{"alarm | Robot is in Lobby"})
}

// TestHideQueryConfigDefault: the switch is on without a key and reads off
// when set so.
func TestHideQueryConfigDefault(t *testing.T) {
	testState(t)
	path := trayConfigPath()
	if !trayReadNotify(path)[trayHideQueryKey] {
		t.Errorf("missing key: switch is off")
	}
	if err := trayWriteConfig(path, map[string]string{"notify.hideQuery": "off"}); err != nil {
		t.Fatal(err)
	}
	if trayReadNotify(path)[trayHideQueryKey] {
		t.Errorf("notify.hideQuery=off still reads as on")
	}
}
