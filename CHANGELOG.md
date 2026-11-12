# Changelog

All notable user-facing changes, newest first. Categories:

**Added** (new features),
**Fixed** (bug and behaviour fixes),
**Removed** (things taken out),
**Info** (notes worth knowing).

## 2026-11-08

### Added
- **Ignore users**: Hide notifications caused by people you choose.
- **Hide ServerQuery clients**: Their notifications are hidden by default.
- **Several names**: The alarm and ignore lists accept more than one name.

## 2026-10-29

### Info
- **Starts with your session**: It now runs as a background service that starts
  automatically.

## 2026-10-22

### Fixed
- **Updates keep your choice**: Updating no longer turns autostart back on if you had
  turned it off.

## 2026-10-21

### Added
- **Alarm settings**: Choose the name and channel the alarm watches for.

### Fixed
- **Alarm off by default**: It starts switched off and quieter.

## 2026-10-20

### Added
- **Lobby alarm**: A siren and a notification when someone you watch joins the lobby.

## 2026-09-25

### Fixed
- **Better defaults**: Mute notices are on and notifications disappear after 3 seconds.
- **Mute icon**: The mute slash is red.

## 2026-09-24

### Added
- **First release**: A tray icon, mute control and desktop notifications for TeamSpeak 6
  on Linux.
- **Settings screen**: Run `ts6tray settings` to change everything in the terminal.
- **Mute key helper**: A guided setup for the mute keys.
- **More notices**: Notices for your own mutes, for being moved or kicked, and for server
  messages.
- **Notification display time**: Choose how long notifications stay.
- **New notification icons**: Clearer pictures for mutes, joins and leaves.

### Fixed
- **Quiet while deaf**: Your own actions still notify when your speakers are muted.
