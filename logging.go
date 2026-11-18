package main

import (
	"log"
	"os"
	"path/filepath"
)

// appLog is wtui's own diagnostic log, separate from wacli's: bubbletea's
// alt-screen mode owns the terminal, so stderr from a failed wacli call is
// otherwise invisible until the whole program exits. Every runWacli call
// is logged here (command, duration, and outcome) so a lock timeout or
// other failure can be diagnosed after the fact. nil until initLogging
// succeeds; callers must tolerate that (e.g. a read-only filesystem).
var appLog *log.Logger

// logPath is where initLogging writes wtui's log, next to wacli's own
// store directory convention (~/.wacli) but under its own dotfile so the
// two never collide.
func logPath() string {
	dir, err := os.UserHomeDir()
	if err != nil {
		dir = "."
	}
	return filepath.Join(dir, ".wtui.log")
}

// initLogging opens wtui's log file for appending and returns it so main
// can close it on exit. Logging is best-effort: a failure to open the file
// is reported but doesn't stop the program, and appLog stays nil.
func initLogging() *os.File {
	f, err := os.OpenFile(logPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil
	}
	appLog = log.New(f, "", log.LstdFlags)
	return f
}

// logErr records err to appLog (a no-op if logging never initialized) and
// returns its message, so a failure that surfaces in the UI as m.err is
// also captured in ~/.wtui.log for debugging after the TUI has moved on.
func logErr(err error) string {
	if appLog != nil {
		appLog.Printf("ui error: %v", err)
	}
	return err.Error()
}
