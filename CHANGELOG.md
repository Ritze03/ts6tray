# Changelog

All notable user-facing changes, newest first. Categories:

**Added** (new features),
**Fixed** (bug and behaviour fixes),
**Removed** (things taken out),
**Info** (notes worth knowing).

## 2026-11-08

### Added
- **Ignore and alarm name lists**: The alarm and ignore name settings accept several
  names separated by `;`.
- **Ignore users**: Notifications caused by ignored users are hidden. The ignore switch
  is off and its list is empty by default.
- **Filter ServerQuery**: A switch, on by default, hides server query clients from
  notifications. They are recognised by client type and by the shape of their unique ID.

## 2026-10-29

### Info
- **Systemd user service**: ts6tray now runs as a systemd user service instead of an XDG
  autostart entry. Updates keep whether it is enabled and move an old autostart link over.

## 2026-10-24

### Removed
- **Built-in installer**: The `install` and `uninstall` subcommands are gone; `install.sh`
  (or `ritz-get install ts6tray`) builds and links it, and `./install.sh --remove`
  uninstalls it.

## 2026-10-22

### Added
- **Install options**: `install --yes` answers every question yes, and `uninstall` removes
  the binary, the autostart entry and the icon cache while keeping your config.

### Fixed
- **Autostart choice kept**: Updating over an existing install no longer switches
  autostart back on if you had declined it.

## 2026-10-21

### Added
- **Alarm settings**: The Alarms section has Name and Channel text rows (Enter edits, Esc
  cancels). An empty name watches for nobody and an empty channel falls back to Lobby.

### Fixed
- **Alarm off by default**: The alarm ships switched off with no name set, and the siren
  is a quarter of full scale instead of half.

## 2026-10-20

### Added
- **Lobby alarm**: When a watched user enters a channel whose name contains a set word
  (default Lobby), ts6tray plays a loud siren and shows a notification. It fires once per
  arrival, never for yourself, and ignores "Silence while my speakers are muted".

## 2026-09-25

### Fixed
- **New defaults**: Mute notices are on, bursts are not grouped, "quiet while deaf" is on
  and notices show for 3 seconds.
- **Red mute slash**: The tray's mute slash is red, like in the notifications.
- **Replace previous notification**: The replace window now runs from the first show, not
  the last send.

### Info
- **README**: Quick start, per-distribution dependencies and a note on memory use (about
  18 MB).

## 2026-09-24

### Added
- **First release**: A tray icon, mute control and desktop notifications for the TeamSpeak
  6 client on Linux (X11 and Wayland). It uses TeamSpeak's local Remote Apps API, hides
  the icon while TeamSpeak is not running or connected, and mutes mic and speakers.
- **Terminal settings**: `ts6tray settings` is a full-screen terminal UI for every
  switch; changes apply to the running tray at once. The tray menu keeps only the
  per-server rows, the two mute toggles and Quit.
- **Key-binding helper**: A section in settings with a 5-second countdown that walks you
  through TeamSpeak's own settings and then presses the key. `ts6tray press mic|speaker`
  does the same from a window-manager bind.
- **More notices**: Switches for your own mutes and for being moved or kicked, for server
  messages, and for "quiet while my speakers are muted".
- **Notification display time**: Pick default, 3, 5, 10, 30 seconds or never; the left
  arrow cycles backwards.
- **Notification icons**: New flat icons for mutes, joins, moves, leaves, kicks, bans,
  timeouts and disconnects; a mute notice shows the user's resulting state.
- **Numbered batches**: Grouped notifications list events newest first and numbered, and
  say "...and N more earlier" past ten lines.

### Fixed
- **Quiet while deaf**: Your own actions, such as muting your speakers, still notify.
- **Mic notices**: Another user's mic change is only reported while they can hear you.
- **Stale replacements**: "Replace previous notification" no longer edits a notification
  that is already gone.
