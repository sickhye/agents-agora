package main

// a2 liveness-check: for the server machine only. Reads the server's
// stateDir()/liveness.json (liveness.go writes it every minute) and prints one line per
// open incident, for an external monitor script that forwards them to a chat or
// pager:
//
//	[a2-liveness] <member> no response — last seen HH:MM, undelivered N
//
// Nothing when every watched member is alive. When the file is missing, unreadable or its
// last tick ("updated", or the file time for files from older builds) is older than
// liveCheckStale, one line:
//
//	[a2-liveness] a2 server status file not updated (server stopped?)
//
// Read-only: no network, no token, no config file; the file is only read. Exit 0 in both
// cases (exit 2 on bad arguments).

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

const (
	liveCheckStale   = 3 * time.Minute
	liveCheckPrefix  = "[a2-liveness] "
	liveCheckStopped = liveCheckPrefix + "a2 server status file not updated (server stopped?)"
)

func cmdLivenessCheck(args []string) error {
	if len(args) != 0 {
		return usageError("liveness-check takes no arguments")
	}
	livenessCheck(os.Stdout, filepath.Join(stateDir(), livenessFile), time.Now())
	return nil
}

// livenessCheck writes the lines for the state file at path as of now.
func livenessCheck(w io.Writer, path string, now time.Time) {
	st, err := os.Stat(path)
	if err != nil || !st.Mode().IsRegular() {
		fmt.Fprintln(w, liveCheckStopped)
		return
	}
	b, err := os.ReadFile(path)
	var f liveFile
	if err == nil {
		err = json.Unmarshal(b, &f)
	}
	if err != nil {
		fmt.Fprintln(w, liveCheckStopped)
		return
	}
	at := f.Updated
	if at.IsZero() {
		at = st.ModTime() // a file from an older build
	}
	if now.Sub(at) > liveCheckStale {
		fmt.Fprintln(w, liveCheckStopped)
		return
	}
	for _, d := range f.Down {
		if !memberNameRe.MatchString(d.Name) {
			continue // the name is a class key in the external monitor script
		}
		fmt.Fprintf(w, "%s%s no response — last seen %s, undelivered %d\n", liveCheckPrefix, d.Name, d.Last.In(jst).Format("15:04"), d.Pending)
	}
}
