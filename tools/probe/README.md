# tools/probe

One-shot protocol probe for the TeamSpeak 6 Remote Apps WebSocket API.
Development tool only — not part of the `ts6tray` binary. Its own nested
module, so it does not pull `github.com/coder/websocket` into the root module.

```sh
go build -o /tmp/probe . && /tmp/probe -out ../../testdata
```

* `-out` fixture directory (default `testdata`)
* `-capture` seconds to record events (default 180; `0` skips straight to the
  input test and leaves `events.jsonl` alone)
* `-auth-wait` how long to wait for the user to approve in the client (default 5m)

Phases:

1. If `~/.config/ts6tray/apikey` is missing: auth with an empty key, wait for
   approval, save the key (dir 0700, file 0600), write `auth_full.json`.
2. Reconnect with the stored key, write `auth_cached.json`.
3. Record every incoming message for `-capture` seconds to `events.jsonl`
   (JSONL: `{"ts","type","raw"}`). Opened `O_EXCL` — delete the file first to
   re-capture, so an accidental re-run cannot destroy a capture.
4. `keyPress` / `buttonPress` / a bogus control type, each on its own fresh
   connection, against the unbound button id `ts6tray.probe`; results to
   `input_test.json`.

Every fixture write goes through `scrub()` in `scrub.go`, which replaces the
`apiKey` value and every MyTeamSpeak bearer token (`myts_token`, which sits
inside the JSON-encoded string held by a client's `metaData` / `userTag`
properties) with `"REDACTED"`, leaving the surrounding structure intact.

To scrub fixtures that already exist:

```sh
go run . -scrub ../../testdata/auth_full.json,../../testdata/events.jsonl
```

`-scrub` takes a comma-separated list, rewrites each file in place and exits.
`.jsonl` files are handled line by line. `go test ./...` covers the scrub.

Findings are written up in `../../docs/protocol.md`.

Note: `coder/websocket` closes the underlying connection when a read's context
is cancelled, which is why each phase uses its own connection.

## Privacy

`apiKey` and all MyTeamSpeak bearer tokens are scrubbed. Real server and user
data remains on purpose: server name and `uniqueIdentifier`, and every
connected user's `nickname`, `databaseId`, `myteamspeakId`, `signedBadges` and
`tag`. Decide whether to scrub those too before publishing the repository.
