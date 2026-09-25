<p align="center"><img src="assets/ts6tray.svg" width="96" alt="ts6tray"></p>

# ts6tray

A tray icon, mute control and desktop notifications for the TeamSpeak 6 client on Linux
(X11 and Wayland). It talks to TeamSpeak's local Remote Apps WebSocket API, publishes a
StatusNotifierItem, and mutes by pressing its own virtual keys.

It runs as a background daemon and stays out of the way — around 60 MB of RAM in testing and
practically no CPU. It notices by itself when TeamSpeak isn't running or isn't connected and
hides the tray icon entirely; the icon comes back when TeamSpeak does.

## Quick start

```sh
# 1. dependencies (see below) — Arch: pacman -S go   Fedora: dnf install golang

# 2. build: one static binary, no cgo (on Fedora prefix GOTOOLCHAIN=auto, see below)
CGO_ENABLED=0 go build -o ts6tray .

# 3. optional: copy to ~/.local/bin and offer autostart
./ts6tray install

# 4. run
./ts6tray --daemon
```

In TeamSpeak, **Settings → Remote Apps**: enable it and check the port is **5899** (the
default). On the first run TeamSpeak shows a permission request for "ts6tray" — accept it. The
key is saved to `~/.config/ts6tray/apikey`; if the approval is ever revoked, ts6tray asks
again on its own.

Then [bind the mute keys](#bind-the-mute-keys-one-time) once, and you are done.

`ts6tray install` copies the binary to `~/.local/bin` and asks whether to autostart it (XDG
autostart, a Hyprland `exec-once` line, or nothing). It writes nothing before you answer.

## Dependencies

| What | Needed for | Arch Linux | Fedora |
| --- | --- | --- | --- |
| Go 1.27 or newer | building only | `pacman -S go` | `dnf install golang`, plus `GOTOOLCHAIN=auto` (see below) |
| TeamSpeak 6 client | everything | from teamspeak.com | from teamspeak.com |
| D-Bus session bus | tray icon, notifications | in every desktop session (`dbus`) | in every desktop session (`dbus`) |
| A StatusNotifierItem host | drawing the icon | KDE, `waybar` (`tray` module), DankMaterialShell, … | KDE, `waybar`, …; GNOME needs the AppIndicator extension |
| A notification server | the notifications | KDE and GNOME have one; on a bare compositor `mako`, `dunst`, `swaync` | same |

Notes:

- **Build**: the build is `CGO_ENABLED=0`, so there is no C toolchain, no headers and no
  shared libraries to install — just Go. The result is one static binary.
- **Go version**: `go.mod` requires Go 1.27. Arch already has it. Fedora does not — its
  `golang` package is 1.25 on Fedora 42 and 1.26 on 44 — and it also ships
  `GOTOOLCHAIN=local`, so the plain build stops with
  `go.mod requires go >= 1.27 (running go 1.26.8; GOTOOLCHAIN=local)`. Ask for the toolchain
  explicitly and Go fetches the right one itself:

  ```sh
  CGO_ENABLED=0 GOTOOLCHAIN=auto go build -o ts6tray .
  ```

  Elsewhere `GOTOOLCHAIN=auto` is already the default and the plain build does this by
  itself; `GOTOOLCHAIN=local` forbids the download, and then you need a newer Go installed.
- **TeamSpeak 6**: no distribution package is assumed — install the client however you like
  (it was probed against 6.0.0beta4.1). It must have **Remote Apps** enabled, and the first
  connection needs an approval click in the client.
- **Tray host**: on GNOME install the *AppIndicator and KStatusNotifierItem Support*
  extension, often packaged as `gnome-shell-extension-appindicator`. Without a session bus at
  all (SSH, a bare TTY) the daemon still runs — you just get no icon, and the CLI still works.

## Bind the mute keys (one-time)

ts6tray mutes by pressing its own virtual keys, so TeamSpeak has to learn them once. Until
then, every mute command reports "not bound" along with these steps.

1. Run `ts6tray settings` and activate **Bind microphone key** (space or enter). A 5 s
   countdown starts, with the steps below on screen. Esc or `q` cancels it.
2. Switch to TeamSpeak → **Settings → Key Bindings**. Set **Microphone** to **Toggle**, then
   click **Choose** at the end of that line. TeamSpeak now waits for a key.
3. When the countdown ends ts6tray presses its key and TeamSpeak records it.
4. Repeat with **Bind speaker key** and the **Speaker** line.

The 5 s delay exists because TeamSpeak stops recording a hotkey as soon as its window loses
focus — you need both windows in that order.

## Usage

Settings live in `~/.config/ts6tray/config`. The way to change them is a terminal:

```sh
ts6tray settings
```

A small full-screen UI with the key-binding helper and every switch below:

| Key | Does |
| --- | --- |
| `↑` `↓` / `k` `j` | move between rows |
| space, enter | toggle a switch, or start a key-binding countdown |
| `→` `←` / `l` `h` | cycle the two multi-value rows — the left-click target and the display time — forwards and backwards |
| `q`, Esc, Ctrl-C | quit (while a countdown runs, the first press cancels it) |

Each change is saved immediately and the running daemon picks it up at once (`ts6tray reload`
does that on its own, if you edit the file by hand). It is a separate, short-lived process, so
the daemon's memory is untouched by it.

Tray: left-click toggles the microphone, middle-click toggles the speaker (swappable in the
settings). The right-click menu is just per-server status, both toggles and **Quit** —
everything else lives in `ts6tray settings`.

CLI, with the daemon running:

```sh
ts6tray mic toggle        # or: mute / unmute
ts6tray speaker toggle    # or: mute / unmute
ts6tray press mic         # press the virtual key once (what the bind helper does)
ts6tray status
ts6tray reload            # re-read the config file
ts6tray help
```

The daemon takes `--addr host:port` if TeamSpeak's Remote Apps port is not the default
`127.0.0.1:5899`.

Mute commands don't queue — a second one while the first is still in progress reports
"a mute command is already in progress".

## Notifications

Desktop notifications for what happens in your channel and to you. Every one names the
person. Switch them on and off in `ts6tray settings`; the choice is saved in
`~/.config/ts6tray/config` as `notify.<kind>=on|off`.

| Notification | Key | Default |
| --- | --- | --- |
| Someone joins or leaves my channel | `joinleave` | on |
| Someone is moved in or out (and by whom) | `moved` | on |
| Someone is kicked or times out | `kicked` | on |
| Someone in my channel mutes or unmutes | `mute` | **off** |
| Private messages | `privateMsg` | **off** |
| Pokes | `poke` | **off** |
| Channel messages | `channelMsg` | **off** |
| Connection lost | `connLost` | on |
| My own changes | `self` | **off** |
| Server messages | `serverMsg` | **off** |

Messages and pokes are off because TeamSpeak already pops up its own notification for them;
mute changes and channel chatter are off because they are noisy. **My own changes** — you
muting or unmuting your own microphone or speakers, and being moved or kicked by someone
else — is off because the tray icon already says all of that; turn it on if you would rather
read it. **Server messages** are server-wide broadcasts. Someone whose speakers are muted
muting or unmuting their microphone says nothing — they are out of the conversation either
way — but their speakers going off and on still does.

Between them these cover everything TeamSpeak itself pops up, so you can have one source of
notifications instead of two: turn messages, pokes and the rest off in TeamSpeak and on here.
With more than one server connected the title says which server a notice came from.

Every notification carries ts6tray's own icon, unpacked once to
`~/.cache/ts6tray/ts6tray.svg` — except:

- a **mute** notification, which shows the person's *new* state: speakers or microphone under
  a red slash, or, when they are unmuted again, the empty talking indicator — the blue ring on
  the navy disc the tray shows while it is quiet, with no glyph in it;
- an **arrival or departure**, which is a green person for someone joining and a red one for
  someone leaving, being kicked or dropping out.

Those icons are unpacked beside it in `~/.cache/ts6tray/notify/`. A grouped notification lists
its events numbered with the newest first and takes that newest event's icon. They are all
sent under the name **TeamSpeak**, so the notification centre shows one source rather than two.

Below the list are the delivery options, saved the same way:

| Option | Key | What it does |
| --- | --- | --- |
| Group bursts (0.5 s) | `notify.batch` | Waits half a second, and half a second again after each further event, then sends the whole burst as one notification with a numbered line per event, newest first. Five people moved at once is one notification, not five. It gives up waiting after 3 s, and lists at most ten lines — past that the oldest are dropped and counted ("…and 3 more earlier"). |
| Replace previous notification | `notify.replace` | Each new notification takes the place of the last one, so only the newest is on screen. The window it may do that in runs from when the notification was **first shown**, not from the last send — a replacement does not restart the server's expiry. Once that time is up, a fresh notification is sent instead: some servers apply a replacement in place, and would otherwise silently edit a row nobody can see. With the display time set to `never` there is no window, since the notification really does stay up. |
| Silence while my speakers are muted | `notify.quietWhenDeaf` | **Off by default.** While your speakers are muted on any connected server, event notifications are dropped rather than held back — you muted them to be left alone, and unmuting should not then deliver the backlog. A lost connection still comes through, and so do your own actions (**My own changes**) — muting the speakers silences other people, not the confirmation of what you just did. |
| Show notifications for | `notify.timeout` | How long one stays on screen: `3`, `5` (the default), `10`, `30` seconds, `never` (until you dismiss it), or `default` to let the notification server decide. |

Kick, ban and timeout wording is best-effort: TeamSpeak has never sent one during a capture,
so it follows the client's documented event codes rather than an observed message, and a kick
reason is shown only if one is actually present.

## Global hotkeys (window manager)

On Wayland TeamSpeak's own hotkeys don't fire while TeamSpeak is unfocused, so bind the
ts6tray commands in your window manager or desktop instead. This is separate from the
one-time binding above: that one teaches TeamSpeak what ts6tray's virtual key means, this one
lets you fire it from anywhere. The daemon must be running, and some WMs don't have
`~/.local/bin` on `$PATH` — use the full path `~/.local/bin/ts6tray` if the bind does nothing.

- **Hyprland**: `bind = , XF86AudioMicMute, exec, ts6tray mic toggle`
  and e.g. `bind = SUPER, M, exec, ts6tray speaker toggle`
- **Sway**: `bindsym XF86AudioMicMute exec ts6tray mic toggle`
- **GNOME**: Settings → Keyboard → Keyboard Shortcuts → Custom Shortcuts, command
  `ts6tray mic toggle`
- **KDE**: System Settings → Shortcuts → Add New → Command, command `ts6tray mic toggle`

## Icons

The icon is hidden while TeamSpeak isn't running, and follows whichever server holds your mic.

| State | Icon |
| --- | --- |
| quiet | blue ring |
| talking | the ring lit up |
| mic muted | white mic, red slash |
| speaker muted | white speaker, red slash |
| mic disabled (this server doesn't have your mic) | grey ring |
| TeamSpeak running, no server | faint grey ring |

With both muted, the speaker icon wins.

## Troubleshooting

- `ts6tray status` reports whether the daemon is running, whether TeamSpeak is reachable and
  the per-server mute state.
- Mute commands say "not bound": redo the [one-time key binding](#bind-the-mute-keys-one-time).
- Nothing connects: check **Settings → Remote Apps** is enabled in TeamSpeak and that the port
  matches (`--addr`, default `127.0.0.1:5899`).
- No tray icon on GNOME: install the *AppIndicator and KStatusNotifierItem Support* extension.
- With no session bus (SSH, a bare TTY) the daemon runs without a tray icon; the CLI still works.
- `ts6tray settings` says it needs a terminal: it is a full-screen TUI and wants a tty; edit
  `~/.config/ts6tray/config` by hand and run `ts6tray reload` instead.

## Development

```sh
go test ./...                    # the root module
cd tools/probe && go test ./...  # the probe's scrubber
```

`docs/protocol.md` documents the Remote Apps wire protocol as observed live; `tools/probe` is
a standalone nested module for poking the API by hand and capturing fixtures. The fixtures
themselves live in `testdata/`, which is not part of this repository.
