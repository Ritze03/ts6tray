// Command ts6tray is a tray icon and mute controller for the TeamSpeak 6
// client on Linux. The daemon talks to TeamSpeak's local Remote Apps
// WebSocket API and publishes a StatusNotifierItem; the other subcommands are
// a thin CLI that drives the running daemon over a unix socket.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"
)

const usage = `ts6tray — tray icon and mute control for TeamSpeak 6

usage:
  ts6tray daemon [--addr host:port]        run the tray daemon (systemd user unit ts6tray.service;
                                           default address 127.0.0.1:5899)
  ts6tray settings                         change the settings in the terminal
  ts6tray mic|speaker toggle|mute|unmute   mute control via the running daemon
  ts6tray press mic|speaker                press the virtual key once (key binding)
  ts6tray status                           print the daemon's view of TeamSpeak
  ts6tray reload                           make the running daemon re-read its config
  ts6tray help                             print this help`

// ipcShutdownGrace bounds how long the daemon waits for the IPC server to
// finish an in-flight command after the tray has gone away.
const ipcShutdownGrace = 3 * time.Second

func main() {
	log.SetFlags(0)
	log.SetPrefix("")

	if len(os.Args) < 2 {
		fmt.Println(usage)
		os.Exit(2)
	}

	switch os.Args[1] {
	case "-h", "--help", "help":
		fmt.Println(usage)
		os.Exit(0)
	case "daemon", "--daemon", "-daemon": // the flag spellings: old autostart entries and scripts
		os.Exit(runDaemon(os.Args[1:]))
	case "settings":
		os.Exit(RunSettings(os.Stdin, os.Stdout))
	case "mic", "speaker", "press", "status", "reload":
		os.Exit(RunIPCClient(SocketPath(), os.Args[1:], os.Stdout))
	default:
		fmt.Println(usage)
		os.Exit(2)
	}
}

// runDaemon runs the client, the IPC server and the tray until SIGINT/SIGTERM.
// args[0] is the subcommand (or its --daemon alias).
func runDaemon(args []string) int {
	fs := flag.NewFlagSet("ts6tray daemon", flag.ContinueOnError)
	fs.SetOutput(os.Stdout)
	addr := fs.String("addr", "127.0.0.1:5899", "TeamSpeak Remote Apps address")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}

	// Already running (the service, or an old autostart entry): not an error.
	// Exit 0 so Restart=on-failure does not respawn into the same answer.
	if ipcLive(SocketPath()) {
		fmt.Println(ipcAlreadyRunning)
		return 0
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	c := NewTSClient(*addr, DefaultKeyPath())

	// The tray does not exist yet, and may never (no session bus), so the IPC
	// server gets a handle it can call `reload` through once RunTray has filled
	// it in.
	rl := &trayReload{}

	// A second daemon must not get as far as a tray icon, so an IPC failure
	// cancels everything and the exit code is taken after RunTray unwinds.
	var ipcFailed atomic.Bool
	ipcDone := make(chan struct{})
	go func() {
		defer close(ipcDone)
		if err := ServeIPC(ctx, c, SocketPath(), rl.Reload); err != nil {
			log.Printf("ts6tray: %v", err)
			ipcFailed.Store(true)
			stop()
		}
	}()

	go c.Run(ctx)

	// The tray is optional: the CLI works over SSH or in a TTY with no session
	// bus, so a tray failure only costs the icon, not the daemon.
	if err := RunTray(ctx, c, stop, rl); err != nil && !ipcFailed.Load() {
		log.Printf("ts6tray: tray unavailable: %v; running without a tray icon", err)
		<-ctx.Done()
	}

	// Wait for an in-flight IPC handler to finish replying before exiting, so a
	// mute that already happened is not reported back as an empty answer.
	select {
	case <-ipcDone:
	case <-time.After(ipcShutdownGrace):
		log.Printf("ts6tray: IPC did not shut down within %s", ipcShutdownGrace)
	}

	if ipcFailed.Load() {
		return 1
	}
	return 0
}
