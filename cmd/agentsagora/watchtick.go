package main

// Liveness stamp of a2 watch: while a watch runs it writes one RFC3339 line to
// a stamp file at start and every minute, whether or not messages arrive, so another tool
// can tell a live watch from a dead one. `a2 watch-status` reads the stamp.

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

const (
	tickEvery   = 60 * time.Second
	tickOff     = "off"
	staleAfter  = 15 * time.Minute
	tickNamePre = "watch-tick-"
)

// tickDefaultPath is $XDG_STATE_HOME/agentsagora/watch-tick-<scope> (fallback
// ~/.local/state/agentsagora), scope = the channel or "all". Never the token dir.
func tickDefaultPath(channel string) string {
	scope := channel
	if scope == "" {
		scope = "all"
	}
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			h = "."
		}
		base = filepath.Join(h, ".local", "state")
	}
	return filepath.Join(base, appName, tickNamePre+filepath.Base(scope))
}

// writeTick writes the stamp atomically (temp file in the same dir, then rename).
func writeTick(path string, now time.Time) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".tick-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, err = io.WriteString(f, now.Format(time.RFC3339)+"\n")
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp, 0o644)
	}
	if err == nil {
		err = renameOver(tmp, path)
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}

// runTick writes the stamp now and then every `every` until ctx ends. A write error is
// logged once to errOut; the watch goes on. Returns when ctx ends.
func runTick(ctx context.Context, path string, every time.Duration, now func() time.Time, errOut io.Writer) {
	if path == "" || path == tickOff {
		return
	}
	logged := false
	write := func() {
		if err := writeTick(path, now()); err != nil && !logged {
			logged = true
			fmt.Fprintf(errOut, "a2 watch: tick: %v\n", err)
		}
	}
	write()
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			write()
		}
	}
}

// cmdWatchStatus: a2 watch-status [--channel C] [--tick-file PATH] [--stale 15m].
// Exit 0 live, 1 stale, 2 missing.
func cmdWatchStatus(args []string) error {
	fs := flag.NewFlagSet("watch-status", flag.ContinueOnError)
	channel := fs.String("channel", "", "channel of the watch (default: all)")
	file := fs.String("tick-file", "", "stamp file (default: the watch's default path)")
	stale := fs.Duration("stale", staleAfter, "older than this is stale")
	if err := fs.Parse(args); err != nil {
		return usageError(err.Error())
	}
	path := *file
	if path == "" {
		path = tickDefaultPath(*channel)
	}
	code, line := watchStatus(path, *stale, time.Now())
	fmt.Println(line)
	if code == 0 {
		return nil
	}
	return exitCodeError{code: code}
}

func watchStatus(path string, stale time.Duration, now time.Time) (int, string) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 2, path + " - age=- missing"
	}
	s := string(b)
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return 2, path + " - age=- missing"
	}
	age := now.Sub(ts).Round(time.Second)
	if age > stale {
		return 1, fmt.Sprintf("%s %s age=%s stale", path, s, age)
	}
	return 0, fmt.Sprintf("%s %s age=%s live", path, s, age)
}
