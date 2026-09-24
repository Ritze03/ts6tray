<p align="center"><img src="assets/ts6tray.svg" width="96" alt="ts6tray"></p>

# ts6tray

A tray icon and mute control for the TeamSpeak 6 client on Linux (X11 and Wayland).

It runs as a background daemon and stays out of the way — around 60 MB of RAM in testing and
practically no CPU. It notices by itself when TeamSpeak isn't running or isn't connected and
hides the tray icon entirely; the icon comes back when TeamSpeak does.

## Quick start

1. **TeamSpeak → Settings → Remote Apps**: enable it and check the port is **5899** (the default).
2. Build: `CGO_ENABLED=0 go build -o ts6tray .` (Go 1.27).
   Optionally `./ts6tray install` — copies the binary to `~/.local/bin` and asks whether to
   autostart it (XDG autostart, a Hyprland `exec-once` line, or nothing). It writes nothing
   before you answer.
3. Run `ts6tray --daemon`. TeamSpeak shows a permission request for "ts6tray" — accept it.
   The key is saved to `~/.config/ts6tray/apikey`; if the approval is ever revoked, ts6tray
   asks again on its own.
4. Bind the mute keys, below.

The tray needs a StatusNotifierItem host: KDE, Waybar's `tray` module, DankMaterialShell and
so on. GNOME needs the *AppIndicator and KStatusNotifierItem Support* extension.

## Bind the mute keys (one-time)

ts6tray mutes by pressing its own virtual keys, so TeamSpeak has to learn them once. Until
then, every mute command reports "not bound" along with these steps.

1. In the tray menu, choose **Settings → Bind microphone key…**. A 10 s countdown starts.
2. Switch to TeamSpeak → **Settings → Key Bindings**. Set **Microphone** to **Toggle**, then
   click **Choose** at the end of that line. TeamSpeak now waits for a key.
3. When the countdown ends ts6tray presses its key and TeamSpeak records it.
4. Repeat with **Bind speaker key…** and the **Speaker** line.

The 10 s delay exists because TeamSpeak stops recording a hotkey as soon as its window loses
focus — you need both windows in that order.

## Usage

Tray: left-click toggles the microphone, middle-click toggles the speaker (swappable under
**Settings**, saved in `~/.config/ts6tray/config`). The right-click menu has per-server
status, both toggles, **Settings** and **Quit**.

CLI, with the daemon running:

```sh
ts6tray mic toggle        # or: mute / unmute
ts6tray speaker toggle    # or: mute / unmute
ts6tray status
```

Mute commands don't queue — a second one while the first is still in progress reports
"a mute command is already in progress".

## Notifications

Desktop notifications for what happens in your channel and to you. Every one names the
person. Switch them on and off under **Settings → Notifications**; the choice is saved in
`~/.config/ts6tray/config` as `notify.<kind>=on|off`.

| Notification | Default |
| --- | --- |
| Someone joins or leaves my channel | on |
| Someone is moved in or out (and by whom) | on |
| Someone is kicked or times out | on |
| Someone in my channel mutes or unmutes | **off** |
| Private messages | **off** |
| Pokes | **off** |
| Channel messages | **off** |
| Connection lost | on |

Messages and pokes are off because TeamSpeak already pops up its own notification for them;
mute changes and channel chatter are off because they are noisy. Nothing about yourself is
ever notified, and with more than one server connected the title says which server it came
from. Every notification carries ts6tray's own icon, unpacked once to
`~/.cache/ts6tray/ts6tray.svg`.

Below the list are two delivery options, both on by default and saved the same way:

| Option | Key | What it does |
| --- | --- | --- |
| Group bursts (0.5 s) | `notify.batch` | Waits half a second, and half a second again after each further event, then sends the whole burst as one notification with a line per event. Five people moved at once is one notification, not five. It gives up waiting after 3 s. |
| Replace previous notification | `notify.replace` | Each new notification takes the place of the last one, so only the newest is on screen. |

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
| mic muted | white mic, slashed |
| speaker muted | white speaker, slashed |
| mic disabled (this server doesn't have your mic) | grey ring |
| TeamSpeak running, no server | faint grey ring |

With both muted, the speaker icon wins.

## Troubleshooting

- `ts6tray status` reports whether the daemon is running, whether TeamSpeak is reachable and
  the per-server mute state.
- No tray icon on GNOME: install the *AppIndicator and KStatusNotifierItem Support* extension.
- With no session bus (SSH, a bare TTY) the daemon runs without a tray icon; the CLI still works.

## Development

`docs/protocol.md` documents the Remote Apps wire protocol; `tools/probe` is a standalone
module for poking the API by hand.
