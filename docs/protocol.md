# TeamSpeak 6 Remote Apps protocol — as observed

Probed against **TeamSpeak 6.0.0beta4.1** (`/opt/teamspeak`) on 2026-09-23 with
`tools/probe`. Everything below marked *observed* is reproducible from the
fixtures in `testdata/`. Anything else is explicitly marked.

Fixtures:

| File | What it is |
|---|---|
| `testdata/auth_full.json` | Reply to the very first `auth` (empty `apiKey`), after the user approved in the client. |
| `testdata/auth_cached.json` | Reply to a reconnect using the stored key. |
| `testdata/events.jsonl` | Run 3: a complete 240 s window, one JSON object per line: `{"ts":<RFC3339Nano>,"type":<msg type>,"raw":<the raw message>}`. **Talk activity only** — see the warning below. |
| `testdata/events_run2_talk_only.jsonl` | Run 2: a complete 180 s window, same shape. Also talk-only. |
| `testdata/events_run4_unmute.jsonl` | Run 4: a complete 270 s window, same shape. Talk only despite the name — no mute events. |
| `testdata/events_run1_salvage.jsonl` | Run 1, partially recovered. Same `raw` shape, **no `ts` field**. See the warning below. |
| `testdata/events_run5_channel.jsonl` | Run 5 (2026-09-24, 300 s): channel joins/leaves/moves, other clients' mute changes, private message, poke, channel messages. Same `{ts,type,raw}` shape. |
| `testdata/auth_run5.json` | Run 5's cached-auth reply (same shape as `auth_cached.json`), taken at the start of that capture. |
| `testdata/input_test.json` | What the client answered to `keyPress` and `buttonPress` on an unbound button id. |

Secrets are replaced by `"REDACTED"` in every fixture — see the privacy note below.

> ### Privacy — what is and is not scrubbed
>
> **Scrubbed.** Every fixture write goes through `scrub()` in
> `tools/probe/scrub.go`, which replaces with `"REDACTED"`:
>
> * the value of any `apiKey` field, and
> * every **MyTeamSpeak bearer token** (`myts_token`). These do not sit at the
>   top level — they live inside the JSON document that TeamSpeak encodes into
>   the single string value of a client's `metaData` and `userTag` properties,
>   so the scrub parses that string, redacts the token inside it, and re-encodes
>   it. The surrounding structure (`tag`, `updated`, …) is preserved, so these
>   fixtures still parse for the state tests.
>
> Applied to the existing fixtures — `REDACTED` occurrences per file
> (`grep -o REDACTED <file> | wc -l`):
>
> | File | `REDACTED` | of which |
> |---|---|---|
> | `auth_full.json` | 6 | 5 `myts_token` + 1 `apiKey` |
> | `auth_cached.json` | 6 | 5 `myts_token` + 1 `apiKey` |
> | `events_run2_talk_only.jsonl` | 1 | 1 `myts_token` |
> | `events.jsonl` | 0 | — |
> | `events_run1_salvage.jsonl` | 0 | — |
> | `input_test.json` | 0 | — |
> | `auth_run5.json` | 4 | 3 `myts_token` + 1 `apiKey` |
> | `events_run5_channel.jsonl` | 0 | — |
>
> The five tokens in each auth reply belong to **five different users on the
> server**, not just the local one. That is 11 `myts_token` values in total
> (5 + 5 + 1) plus one `apiKey` per auth file. A deep diff against the
> pre-scrub copies confirms exactly 11 changed leaves, all of them token
> values, with every key set, type and number byte-identical — the two
> `apiKey` values do not show up in that diff because the pre-scrub copies
> were taken with the key already redacted.
>
> **Not covered by `scrub()`, handled separately.** `scrub()` only removes
> `apiKey` and `myts_token`. The captures also contained real server names,
> real users' `nickname`, `uniqueIdentifier`/`uid`, `myteamspeakId` and `tag`
> values. Those have been **anonymised in place** in the local fixtures, and in
> the examples in this document, using consistent shape-preserving
> placeholders: users are `UserA`…`UserM`, servers are `Example Server A` / `B`,
> and every identifier keeps its original length and format (`ExampleUid…=`,
> `00000000-0000-4000-8000-…`, `someone@myteamspeak.com`). The user's own
> nicknames `Ritze` and `RitzeTest` are kept, since the tests are written
> around them. `databaseId` and `signedBadges` are left as captured.
>
> `testdata/` is **gitignored and not part of the published repository** —
> the anonymisation is a second line of defence, not the only one. A fresh
> capture will contain real data again and must be anonymised the same way
> before any of it is quoted here.
>
> To re-scrub after a new capture:
> `cd tools/probe && go run . -scrub ../../testdata/auth_full.json,../../testdata/events.jsonl`

> ### Warning about the event fixtures — read before writing state tests
>
> Four capture runs happened. **Only run 1 contained the mute and
> capture-handover actions, and run 1 is the one that was partially lost.**
>
> **Run 1** — contained mic mute, speaker mute, talking, a second server
> connecting, the capture device moving between the two servers, and that
> server disconnecting. It answers D6. Its `events.jsonl` was **destroyed by an
> accidental re-run of the probe** before it was preserved. What survives is
> `events_run1_salvage.jsonl`, recovered verbatim from the probe's stdout log.
> The probe truncates stdout lines at 220 bytes, so **every message longer than
> that is missing** — all 20 `clientPropertiesUpdated`, 6 long
> `clientSelfPropertyUpdated` (`userTag`/`metaData`), the `channels` tree, and
> the `connectStatusChanged` with `status: 2`. Nothing was reconstructed or
> edited; truncated lines were dropped, not repaired. 55 messages survive.
>
> **Run 2** (`events_run2_talk_only.jsonl`) — a genuine, complete 180 s window,
> but **talk activity only**: 24 `talkStatusChanged`, 18
> `clientSelfPropertyUpdated` (all `flagTalking`), 6 `clientPropertiesUpdated`,
> 2 `clientMoved`.
>
> **Run 3** (`events.jsonl`) — a deliberate 240 s re-capture specifically to
> record mute / input-deactivate / second-server actions. **None of them
> happened during the window.** The user was in a voice conversation
> throughout. What it contains is 104 messages: 64 `talkStatusChanged`, 36
> `clientSelfPropertyUpdated` (**all 36 are `flagTalking`**), 3
> `clientPropertiesUpdated` (all for *other* clients, ids 11, 7, 9 — none for
> our own client 18), 1 `log`. Specifically **absent**: any `inputMuted`,
> `outputMuted`, `inputDeactivated` or `inputHardware` change; any
> `connectStatusChanged`; any `currentServerConnectionChanged`. It is a good
> talk/`flagTalking` fixture and nothing more.
>
> **Run 4** (`events_run4_unmute.jsonl`, 2026-09-24) — a complete 270 s window
> and, despite the file name, **talk only: it contains no mute events at all.**
> What it confirms is the shape of a talk transition for our own client: it
> arrives as a *pair*, `clientSelfPropertyUpdated{flag:"flagTalking"}` plus
> `talkStatusChanged{clientId:<ours>,status:1|0}`, with the same timestamp and
> in an unstable order — either one alone is enough to drive the tray. Other
> clients' `talkStatusChanged` differ only in `clientId` (run 4 opened with
> clientId 24 while ours was 18), so **filtering on our own clientId is
> mandatory.** Our client's properties also carry `isMuted`,
> `outputOnlyMuted`, `outputHardware`, `isTalker`, `talkPower`, `talkRequest`
> and `talkRequestMsg`, all observed false/zero while unmuted and none of them
> needed for the tray model.
>
> **Consequence.** Everything this document marks *observed* was genuinely seen
> live, but **`state.go`'s fixture-driven tests still cannot cover the mute,
> input-deactivate or capture-handover paths.** Those need one more capture in
> which the user actually performs: mic mute/unmute, speaker mute/unmute, the
> TS6 "disable microphone" action, connect a second server, move the mic
> between servers, disconnect it. Delete `testdata/events.jsonl` first — the
> probe opens it `O_EXCL` and will refuse rather than overwrite.

## Connection

* URL: `ws://127.0.0.1:5899` — plain WebSocket, no subprotocol, no HTTP headers
  needed. Port is user-configurable in the client and remote apps can be
  disabled entirely, so a failed dial is a normal condition, not a bug.
* All frames are **text** frames containing one JSON object.
* Every message has the shape `{"type": "<name>", "payload": {...}}`. Replies to
  requests we send additionally carry `"status": {"code": 0, "message": "ok"}`.
  Events pushed by the client have **no** `status` field.

### WebSocket library

`github.com/coder/websocket` v1.8.15.

Reason: pure Go, zero transitive dependencies (so it works with
`CGO_ENABLED=0` and keeps the static binary small), context-aware
`Read`/`Write` (which is what makes the read-with-deadline loop in the probe
trivial), and it is the maintained continuation of `nhooyr.io/websocket`.
`gorilla/websocket` would also work but has no context support on reads;
`golang.org/x/net/websocket` is documented as incomplete and not recommended.
**The `ts.go` worker should use the same library.**

## Authentication

### The exact payload

The daemon **must** send byte-for-byte this identity, otherwise the stored key
is not recognised:

```json
{
  "type": "auth",
  "payload": {
    "identifier": "ts6tray",
    "version": "0.1.0",
    "name": "ts6tray",
    "description": "Tray icon and mute control for TeamSpeak 6",
    "content": {
      "apiKey": ""
    }
  }
}
```

On subsequent connections the only change is `content.apiKey` holding the
stored key.

> The docs say the identifier must be lowercase letters a–z and dots only.
> `ts6tray` contains a digit and was **accepted anyway** (observed). Keep it —
> changing it would invalidate the key.

### Key storage

`~/.config/ts6tray/apikey`, directory mode `0700`, file mode `0600`, contents
are the raw UUID plus a trailing newline. Trim whitespace when reading.

The client keeps its own copy in `~/.config/TeamSpeak/Default/settings.db`,
table `json_blobs`, key `remoteApps`, as
`apps.<identifier>.{allowed, auth:{identifier,name,description,version,content:{apiKey}}}`.
Observed: that blob is **not** written immediately on approval — it was still
missing `ts6tray` minutes after a successful auth, so it is flushed later
(probably on client shutdown). Do not read it to discover our own key.

### Reply shape — full vs cached

**The cached reply is NOT slim.** In the original back-to-back pair the
first-time reply and the cached-key reply were **byte-for-byte identical JSON**,
46463 bytes each. Nothing is missing, and there is no need to wait for follow-up
events to build the initial picture — the assumption that a cached auth returns
a reduced snapshot is wrong for this build.

(The `auth_cached.json` currently in `testdata/` is from a later reconnect, so
its scalar values — `uptime`, `clientsOnline`, … — differ from `auth_full.json`.
Verified: the two files have **exactly the same key set** at every level, same
number of connections and the same 13 `clientInfos`.)

```
{
  "type": "auth",
  "status": { "code": 0, "message": "ok" },
  "payload": {
    "apiKey": "<uuid>",
    "currentConnectionId": 0,
    "connections": [
      {
        "id": 1,                     // == connectionId in all events
        "clientId": 18,              // OUR client id on this connection
        "status": 4,                 // ConnectStatus, see below
        "properties": { ... },       // server properties
        "channelInfos": { "rootChannels": [...], "subChannels": {...} },
        "clientInfos": [ { "id": <clientId>, "channelId": "<id>", "properties": {...} }, ... ]
      }
    ]
  }
}
```

* `connections[].id` is a per-session counter that is **not** reused stably; the
  second server connected during the probe got `id: 4`.
* The stable server identity is `connections[].properties.uniqueIdentifier`
  (base64, e.g. `"ExampleServerAUid00000000000000000000000000="`). The same
  value arrives on a new connection as `connectStatusChanged.payload.info.serverUid`.
  Human-readable name: `connections[].properties.name`.
* `currentConnectionId` was **0** while the only live connection had `id: 1`.
  So it does **not** mean "the connection that owns the microphone", and 0 is
  not a valid connection id here. Do not use it for D6. *(Unexplained — treat
  as unknown.)*

### Deriving initial per-connection state from the snapshot

For each `conn` in `payload.connections`:

1. Skip it unless `conn.status == 4` (ConnectionEstablished).
2. Server key: `conn.properties.uniqueIdentifier`; display name `conn.properties.name`.
3. Find our own client: the element of `conn.clientInfos` whose `id == conn.clientId`.
4. Read the audio flags from that element's `properties` (see the table).

## Enums

`connectStatusChanged.payload.status` and `connections[].status`
(from the client bundle, and every value except 0/1/2/3/4 unobserved):

| Value | Name |
|---|---|
| 0 | Disconnected |
| 1 | Connecting |
| 2 | Connected |
| 3 | ConnectionEstablishing |
| 4 | ConnectionEstablished |

`talkStatusChanged.payload.status`:

| Value | Name |
|---|---|
| 0 | NotTalking |
| 1 | Talking |
| 2 | TalkingWhileDisabled |

(0 and 1 observed; 2 taken from the client bundle — it is what the client shows
when you talk with the mic muted/disabled, and is a useful extra signal.)

## The flag table

All flags live in a **client properties object**: either
`clientInfos[].properties` in the auth snapshot, or `payload.properties` of a
`clientPropertiesUpdated` / `clientMoved` event, or as
`payload.flag` + `payload.newValue` of a `clientSelfPropertyUpdated` event.

| State (D2/D6) | Field | Type | Meaning | Status | Backed by |
|---|---|---|---|---|---|
| Mic muted | `inputMuted` | bool | true = our microphone is muted on this connection | **observed**, toggled both directions | `events_run1_salvage.jsonl`, `auth_*.json` |
| Speaker muted | `outputMuted` | bool | true = our speakers are muted on this connection | **observed**, toggled both directions | `events_run1_salvage.jsonl`, `auth_*.json` |
| Talking (self) | `flagTalking` | bool | our own talk state, mirrors `talkStatusChanged` for our `clientId` | **observed** | `events.jsonl` (36), `events_run2_talk_only.jsonl` (18) |
| Talking (per client) | `talkStatusChanged.status` | int | 1 = talking, 0 = stopped, 2 = talking while disabled | **observed** (0 and 1 only; **2 never seen** — it needs talking while muted/disabled, which no run captured) | `events.jsonl` (64), `events_run2_talk_only.jsonl` (24) |
| **Which connection owns capture (D6)** | `inputHardware` | bool | **true on exactly one connection at a time**; the connection whose capture device is open | **observed live**, both handover directions; only the `false -> true` self-event survives in a fixture | `events_run1_salvage.jsonl` (1 transition), `auth_*.json` |
| Mic disabled / input deactivated | `inputDeactivated` | bool | present on every client properties object, `false` in every message of every run | **UNKNOWN — never seen change**, across three runs | `auth_*.json` (value only, never a transition) |
| Speaker hardware | `outputHardware` | bool | playback device open; stayed `true` on all connections, including the one without capture | **observed constant** | `auth_*.json` |
| Muted by someone else | `isMuted` / `outputOnlyMuted` | bool | server/local mute of *another* client; not our own state | present, unchanged, **unused by ts6tray** | `auth_*.json` |
| Away | `away` + `awayMessage` | bool/string | | present, unchanged | `auth_*.json` |

### D6: `inputHardware` is the capture-owner flag — confirmed

During run 1 the user connected a second server (`connectionId: 4`) and the
capture device moved between the two. This is the transcript as it was read out
of the (now lost) run-1 `events.jsonl` at the time — of these lines only the
`clientSelfPropertyUpdated` at 47.711 survives in
`events_run1_salvage.jsonl`, because `clientPropertiesUpdated` messages are too
long for the probe's stdout log:

```
20:31:40.407 clientPropertiesUpdated conn 1 client 18   inputHardware: true -> false
20:31:40.684 clientMoved             conn 4 client 63676 inputHardware: true
20:31:47.711 clientPropertiesUpdated conn 1 client 18   inputHardware: false -> true
20:31:47.712 clientPropertiesUpdated conn 4 client 63676 inputHardware: true -> false
20:31:54.199 clientPropertiesUpdated conn 1 client 18   inputHardware: true   (conn 4 gone)
```

So: **the connection with `inputHardware == true` on our own client is the one
with the active microphone. All other connected servers are "mic disabled".**
That is the signal the tray icon should follow.

Note `inputHardware` on connection 1 went `true -> false` *before* connection 4
existed (at 40.407, while connection 4 was still at status Connecting) — the
client releases capture from the old connection first.

### `inputDeactivated` — still UNKNOWN after three runs

`inputDeactivated` exists on every client properties object and was `false` in
**every message of every capture run**. **No `inputDeactivated` transition has
ever been captured**, including in run 3, which was made specifically to record
a TS6 "disable microphone" action. We therefore still do not know:

* whether TS6 6.0.0beta4.1 exposes a distinct "input deactivated" state at all,
  or whether the UI action the user would reach for maps onto `inputHardware`;
* what user action sets it;
* whether it is reported by `clientSelfPropertyUpdated`, by
  `clientPropertiesUpdated`, or both.

Until that is answered, derive "mic disabled" defensively as

```
micDisabled = inputDeactivated || !inputHardware
```

so the daemon is correct either way: `inputHardware == false` is the
**confirmed** signal (run 1), and `inputDeactivated == true` is handled if it
ever appears. **D2's claim that "input deactivated" is a state distinct from
muted is NOT yet confirmed against this build.**

### `currentServerConnectionChanged` — not fired, and not disproven

It appears in the client's event-name catalog in
`/opt/teamspeak/html/client_ui/main.js` but has **never been received** in any
run. Run 3 does not count as evidence against it: no connection was added,
removed or switched during that window, so there was nothing for it to report.
It remains the most likely explanation for `currentConnectionId`, and it stays
**unknown**.

## Observed events

Shapes below are all taken from real messages seen during the probe. Where an
example is not present in a shipped fixture (see the warning at the top) that is
noted inline.

### `clientSelfPropertyUpdated` — **do not rely on this alone**

```json
{"type":"clientSelfPropertyUpdated",
 "payload":{"connectionId":1,"flag":"inputMuted","oldValue":false,"newValue":true}}
```

Flags observed here, across all runs: `inputMuted`, `outputMuted`,
`flagTalking`, `inputHardware`, `userTag`, `metaData`. In run 3
(`events.jsonl`) all 36 of these events carry `flag: "flagTalking"` and nothing
else.

**Important caveat (observed in run 1):** this event is *not* emitted for every
change. When capture moved from connection 1 to connection 4, only
`clientPropertiesUpdated` reported connection 1's `inputHardware: true -> false`
— no `clientSelfPropertyUpdated` was sent for it. Connection 4 never received a
`clientSelfPropertyUpdated` for any audio flag at all. **`state.go` must treat
`clientPropertiesUpdated` (for `clientId == our clientId` on that connection) as
the authoritative source and use `clientSelfPropertyUpdated` only as a
supplement.**

### `clientPropertiesUpdated` — the authoritative state feed

```json
{"type":"clientPropertiesUpdated",
 "payload":{"clientId":18,"connectionId":1,"properties":{ ...full property set... }}}
```

Carries the **complete** property set every time, for any client on any
connection. Filter on `clientId == connections[connectionId].clientId`.

### `clientMoved`

Two shapes. Full form when a client becomes visible (includes `properties`):

```json
{"type":"clientMoved","payload":{
  "clientId":63676,"connectionId":4,"hotReload":false,
  "newChannelId":"16292","oldChannelId":"0",
  "properties":{ ... }}}
```

Short form for an actual channel switch or a client leaving:

```json
{"type":"clientMoved","payload":{
  "clientId":63676,"connectionId":4,"hotReload":false,
  "newChannelId":"0","oldChannelId":"16297","type":1,"visibility":2}}
```

`newChannelId == "0"` means the client left / became invisible.
This is how **our own initial state on a newly connected server arrives** — there
is no second auth-style snapshot for a new connection.

### `connectStatusChanged`

This `status: 2` example is quoted from run 1's live output; it is **not** in a
fixture (too long for the salvage). The `status` 0/1/3/4 messages are in
`events_run1_salvage.jsonl`.

```json
{"type":"connectStatusChanged","payload":{
  "connectionId":4,"error":0,"hotReload":false,"status":2,
  "info":{"clientId":63676,
          "legacyUUID":"ExampleLegacyUUID0000000000=",
          "serverName":"Example Server B",
          "serverUid":"ExampleServerBUid00000000000000000000000000="}}}
```

* `info` is `null` for `status` 0 (Disconnected) and 1 (Connecting).
* The full `info` (with `serverUid`, `serverName`, our `clientId`) arrives
  **only at `status: 2`**. Statuses 3 and 4 carry just `{"clientId":…}`.
  So capture `serverUid` and our `clientId` at status 2 and keep them.
* Observed sequence for a connect: `1, 2, 3, 4`. For a disconnect: `0`.

### `talkStatusChanged`

```json
{"type":"talkStatusChanged","payload":{
  "clientId":18,"connectionId":1,"isWhisper":false,"status":1}}
```

Emitted for every client, including ourselves. Match `clientId` against our own
per-connection `clientId`.

### Others observed (not needed for ts6tray, listed for completeness)

| Type | Payload keys |
|---|---|
| `log` | `channel`, `complete`, `id`, `level`, `message`, `time` — the client's own log stream; high volume (42 of 97 messages in this run were TSDNS noise). Ignore it. |
| `channels` | `connectionId`, `hotReload`, `info: {rootChannels, subChannels}` — full channel tree for a newly connected server. |
| `channelsSubscribed` | `connectionId`, `channelIds: [string]` |
| `channelPropertiesUpdated` | `connectionId`, `channelId`, `properties` |
| `serverPropertiesUpdated` | `connectionId`, `properties` (same shape as `connections[].properties`) |
| `clientChannelGroupChanged` | `connectionId`, `clientId`, `channelId`, `channelGroupId`, `channelGroupInheritedChannelId` |
| `groupInfo` | `connectionId`, `type`, `data: [...]` |
| `neededPermissions` | `connectionId`, `data: {permId: value}` |
| `streamInfoReplace` | `connectionId`, `clientIds: [int]`, `info: []` |

The client's full event-name catalog (extracted from
`/opt/teamspeak/html/client_ui/main.js`, **not** all observed) also includes:
`serverEdited`, `serverShutdown`, `textMessage`, `ignoredWhisper`,
`channelsUnsubscribed`, `channelDescriptionUpdated`, `channelPasswordChanged`,
`clientConnectionInfo`, `serverConnectionInfo`, `currentServerConnectionChanged`,
`fileInfo`, `permissionError`, `permissionList`,
`serverGroupClientMembershipChanged`, `serverGroupClientList`,
`clientChatClosed`, `clientChatComposing`, `clientUidFromClientId`,
`clientNamefromUID`, `clientPermissionHints`, `channelPermissionHints`,
`banList`, `soundDeviceListChanged`, `serverError`, `audioDeviceModes`,
`peerConnectionEvent`.

`currentServerConnectionChanged` is probably the "active tab changed" event that
would explain `currentConnectionId`; it has not been observed in any run — see
the section above.

## Sending input (D12)

**Use `buttonPress`. `keyPress` — what the on-disk docs say — is silently
ignored by this build.** (observed, `testdata/input_test.json`)

Send, for the unbound id `ts6tray.probe`:

```json
{"type":"buttonPress","payload":{"button":"ts6tray.probe","state":true}}
{"type":"buttonPress","payload":{"button":"ts6tray.probe","state":false}}
```

The client acknowledges **each** one:

```json
{"type":"buttonPress",
 "payload":{"button":"ts6tray.probe","state":true},
 "returnCode":"",
 "status":{"code":0,"message":"ok"}}
```

`keyPress` produced **no response at all** — byte-for-byte the same outcome as
the control message `{"type":"ts6trayBogus",...}`, which is certainly not a
valid type. That is the evidence that `keyPress` is not handled: the client
neither acknowledges nor errors on unknown types, and `keyPress` behaves like an
unknown type while `buttonPress` is acknowledged.

Consequences for the daemon:

* Always send `buttonPress`.
* A `status.code == 0` ack means "message accepted", **not** "a hotkey fired" —
  `ts6tray.probe` is bound to nothing and was still acked. There is no feedback
  telling you whether the user has bound the button. The only confirmation that
  a mute toggle worked is the resulting `clientPropertiesUpdated`.
* Always send the `state: false` release after the `state: true` press; the
  client's hotkey recorder requires the release.
* The incoming `buttonPress` ack shares the `type` name with the outgoing
  message, so the read loop must not mistake it for an event.
* `returnCode` is echoed back; it is presumably a request-correlation id you can
  set on outgoing messages. Not tested.

Note: `input_test.json` was regenerated while the user was talking, so the
`responses` arrays also contain unrelated ambient `talkStatusChanged` /
`clientSelfPropertyUpdated` events that arrived in the same 3 s window. The
signal is the presence or absence of a message whose `type` equals the type that
was sent.

## Re-running the probe

```sh
cd tools/probe && go build -o /tmp/probe . && /tmp/probe -out ../../testdata
```

Flags: `-out` (fixture dir), `-capture` (seconds, default 180), `-auth-wait`
(default 5m). If `~/.config/ts6tray/apikey` already exists the first-time auth
phase is skipped and `auth_full.json` is left alone; delete the key file (and
revoke `ts6tray` in the client) to redo it.

## Run 5 (2026-09-24, channel events)

Capture window `2026-09-23T23:41:43.998Z` … `2026-09-23T23:46:37.379Z` (300 s,
local 01:41:43–01:46:44 +02:00), fixture `testdata/events_run5_channel.jsonl`,
snapshot `testdata/auth_run5.json`.

Our own identity for this run, from the snapshot:
`payload.currentConnectionId` → connection `id: 4`, `connections[].clientId:
30`, and that client's entry in `connections[].clientInfos[]` gives
`channelId: 23`, `properties.nickname: "Ritze"`. The "friend" was the user's own
second client, `clientId 33` / `RitzeTest`, and the channel it was parked in
between actions is `37`.

Event counts for the window: `talkStatusChanged` 214, `clientPropertiesUpdated`
16, `clientSelfPropertyUpdated` 8, `clientMoved` 7, `clientChannelGroupChanged`
7, `textMessage` 4, `streamInfoReplace` 3, `clientChatComposing` 1. 260 lines.

### Join / leave a channel — `clientMoved` with `type: 1` (self-move)

Joined our channel (`newChannelId == "23"` == our channel):

```json
{"type":"clientMoved","payload":{"clientId":33,"connectionId":4,"hotReload":false,
  "newChannelId":"23","oldChannelId":"24","type":1,"visibility":1}}
```

Left our channel (`oldChannelId == "23"`, `newChannelId` some other channel):

```json
{"type":"clientMoved","payload":{"clientId":33,"connectionId":4,"hotReload":false,
  "newChannelId":"37","oldChannelId":"23","type":1,"visibility":1}}
```

No `properties`, **no nickname** — see the nickname recipe below. Every
`clientMoved` is followed ~0.1 ms later by a `clientChannelGroupChanged` for the
same `clientId` with the new `channelId`; it carries no name either and is
redundant for notifications.

### Moved by someone — `clientMoved` with `type: 2` and `invoker`

Moved **into** our channel by another user:

```json
{"type":"clientMoved","payload":{"clientId":33,"connectionId":4,"hotReload":false,
  "invoker":{"id":30,"nickname":"Ritze","uid":"ExampleUidRitze000000000000="},
  "newChannelId":"23","oldChannelId":"37","type":2,"visibility":1}}
```

Moved **out** of our channel by another user (same shape, channels swapped):

```json
{"type":"clientMoved","payload":{"clientId":33,"connectionId":4,"hotReload":false,
  "invoker":{"id":30,"nickname":"Ritze","uid":"ExampleUidRitze000000000000="},
  "newChannelId":"37","oldChannelId":"23","type":2,"visibility":1}}
```

`invoker` is present **only** on `type: 2` and carries a `nickname` — the moved
client still does not. `visibility` was `1` on all seven `clientMoved` in this
run (the moved client stayed visible throughout); `0` and `2` were not produced.

`type` enum (from the client bundle, **only 1 and 2 observed**): 0 Subscription,
1 Move, 2 Moved, 3 Timeout, 4 KickFromChannel, 5 KickFromServer, 6
BanFromServer. Values 3–6 are **not** in any fixture.

### Another client muting / unmuting — `clientPropertiesUpdated`

Other users' mute state is observable. The event carries the **full** property
set including `nickname`, so it is self-sufficient (abridged here):

```json
{"type":"clientPropertiesUpdated","payload":{"clientId":33,"connectionId":4,
  "properties":{"nickname":"RitzeTest","inputMuted":true,"outputMuted":false,
    "inputHardware":true,"outputHardware":true,"inputDeactivated":false,
    "flagTalking":false,"away":false,"channelGroupInheritedChannelId":"23", ...}}}
```

There is no `oldValue`/`newValue`: to fire "RitzeTest muted their mic" you must
diff against the last known properties for that `clientId`. Note the payload has
**no `channelId`** — `channelGroupInheritedChannelId` happens to equal the
client's channel here but is a permission-inheritance field, not membership.
Track channels from the snapshot plus `clientMoved` instead.

Unrelated clients also emit `clientPropertiesUpdated` at a low rate without any
mute change (7 such events in this run, e.g. `UserB`, `UserC`), so a naive
"properties changed → notify" would be noisy. Diff the two mute fields.

### Private message, channel message, poke — `textMessage`

Poke (`targetMode: 4`, `targetId` == our `clientId`; the poke carried no text):

```json
{"type":"textMessage","payload":{"connectionId":4,
  "invoker":{"id":33,"nickname":"RitzeTest","uid":"ExampleUidRitzeTest00000000="},
  "message":"","targetId":30,"targetMode":4}}
```

Private text message (`targetMode: 1`, `targetId` == our `clientId`):

```json
{"type":"textMessage","payload":{"connectionId":4,
  "invoker":{"id":33,"nickname":"RitzeTest","uid":"ExampleUidRitzeTest00000000="},
  "message":"hi","targetId":30,"targetMode":1}}
```

Channel message (`targetMode: 2`, `targetId: 0` — the channel is implicit, it is
the one we are in):

```json
{"type":"textMessage","payload":{"connectionId":4,
  "invoker":{"id":23,"nickname":"UserA","uid":"ExampleUidUserA000000000000="},
  "message":"hi","targetId":0,"targetMode":2}}
```

`targetMode` enum: 1 client (private), 2 channel, 3 server, 4 poke. **1, 2 and 4
observed; 3 (server message) not produced in this run.** `invoker.nickname` is
always present, so text messages and pokes need no name lookup.

A private message is preceded by a `clientChatComposing` while the sender types:

```json
{"type":"clientChatComposing","payload":{"clientId":33,"connectionId":4,
  "invoker":{"id":33,"uid":"ExampleUidRitzeTest00000000="}}}
```

(no `nickname` in it — resolve via the id map).

### Recipes

* **Nickname for a `clientMoved`**: not in the event. Keep a `clientId →
  nickname` map per connection, seeded from
  `auth.payload.connections[].clientInfos[].{id, properties.nickname}` and
  updated from every `clientPropertiesUpdated`
  (`payload.clientId` → `payload.properties.nickname`) and from
  `textMessage`/`clientMoved` `invoker` objects. Run 5 contains **no**
  `clientMoved` carrying a `properties` block (the "full form" documented
  above), so for a client that becomes newly visible the map may need the
  snapshot or a later event.
* **"Into MY channel" vs elsewhere**: compare the string `newChannelId` /
  `oldChannelId` against our own channel id. Ours comes from
  `clientInfos[]` where `id == connections[].clientId` (`channelId: 23` here),
  and must be updated whenever a `clientMoved` has `clientId == our clientId`.
  `newChannelId == our channel` → joined; `oldChannelId == our channel` → left;
  neither → elsewhere on the server, ignore. Channel ids are **strings** in
  `clientMoved` but **numbers** in the snapshot's `clientInfos[].channelId`.
* **Join vs moved-by-someone**: `type == 1` → the client moved itself;
  `type == 2` → moved by `invoker.nickname`.

### Not present in this capture

The user did **no** kicks, so no `clientMoved` with `type` 4/5 and no `reason`
field has ever been observed — the kick notification shape is still unknown, as
are `visibility` 0/2 in run 5, `targetMode: 3`, a client leaving the server
(`newChannelId: "0"`), and any `clientMoved` with an inline `properties` block.
