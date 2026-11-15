package main

// Server self-update: the server watches its own binary on disk (bin/agentsagora;
// bin/a2 is a symlink to it). When the file has changed, has been quiet for 30 s (a build in
// progress is never picked up), differs from the running binary and its `version` command
// answers within 10 s, the server stops taking new requests, lets in-flight ones finish
// (10 s), writes one audit line and exits with selfUpdateExit. systemd (Restart=always)
// starts the new binary. Viewer UI sessions are on disk (viewsess.go), so nobody is
// logged out.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	selfUpdateExit     = 75 // EX_TEMPFAIL; the unit restarts on any exit (Restart=always)
	selfUpdateDebounce = 30 * time.Second
	selfUpdatePoll     = 2 * time.Second
	selfCheckTimeout   = 10 * time.Second
	shutdownGrace      = 10 * time.Second
	lastUpdateFile     = "last-update.json"
)

// exitCodeError makes run() exit with a given code after printing msg.
type exitCodeError struct {
	code int
	msg  string
}

func (e exitCodeError) Error() string { return e.msg }

type selfUpdater struct {
	path       string // binary on disk
	runningSHA string
	debounce   time.Duration
	now        func() time.Time
	check      func(path string) (string, error)
	logf       func(format string, a ...any)
	audit      *auditLog // refusals get one audit line (nil: none)

	last       os.FileInfo // last stat seen
	lastChange time.Time   // when last changed (zero: unchanged since start)
	checked    os.FileInfo // the file state already judged (running binary or refused)
}

func newSelfUpdater(path, runningSHA string, now func() time.Time) *selfUpdater {
	u := &selfUpdater{path: path, runningSHA: runningSHA, debounce: selfUpdateDebounce, now: now, check: versionCheck,
		logf: func(f string, a ...any) { fmt.Fprintf(os.Stderr, "a2: self-update: "+f+"\n", a...) }}
	if st, err := statNow(path); err == nil {
		u.last, u.checked = st, st
	}
	return u
}

// statNow is os.Stat with the file identity read at once. On Windows os.Stat reads the file
// ID lazily, by path, at the first os.SameFile, so a FileInfo kept across a rename-over would
// describe the new file and sameState would miss the change. Elsewhere the extra call is a no-op.
func statNow(path string) (os.FileInfo, error) {
	st, err := os.Stat(path)
	if err == nil {
		os.SameFile(st, st) // loads the Windows file ID now
	}
	return st, err
}

func sameState(a, b os.FileInfo) bool {
	return a != nil && b != nil && os.SameFile(a, b) && a.Size() == b.Size() && a.ModTime().Equal(b.ModTime())
}

// step is one poll. It returns the new file's sha256 and version line when the server
// should restart into it.
func (u *selfUpdater) step() (sha, version string, ready bool) {
	st, err := statNow(u.path)
	if err != nil || !st.Mode().IsRegular() {
		return "", "", false // mid-rename or deleted: wait
	}
	now := u.now()
	if !sameState(st, u.last) {
		u.last, u.lastChange = st, now
		return "", "", false
	}
	if sameState(st, u.checked) {
		return "", "", false
	}
	if now.Sub(u.lastChange) < u.debounce || now.Sub(st.ModTime()) < u.debounce {
		return "", "", false
	}
	u.checked = st
	sha, _, err = hashFile(u.path)
	if err != nil {
		u.logf("hash %s: %v", u.path, err)
		return "", "", false
	}
	if sha == u.runningSHA {
		return "", "", false
	}
	v, err := u.check(u.path)
	if err != nil {
		u.logf("new binary %s refused (kept running %s): %v", shortBuild(sha), shortBuild(u.runningSHA), err)
		u.audit.write(auditEntry{Remote: "local", Action: "self-update-refused", Target: shortBuild(u.runningSHA) + "->" + shortBuild(sha)})
		return "", "", false
	}
	return sha, v, true
}

// versionCheck runs `<path> version` (10 s) and wants one line starting with "agentsagora ".
func versionCheck(path string) (string, error) {
	return versionCheckTimeout(path, selfCheckTimeout)
}

func versionCheckTimeout(path string, d time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "version")
	cmd.WaitDelay = time.Second
	var out bytes.Buffer
	cmd.Stdout = &limitedBuf{b: &out, n: 4096}
	err := cmd.Run()
	if ctx.Err() != nil {
		return "", fmt.Errorf("`version` did not answer in %s", d)
	}
	if err != nil {
		return "", fmt.Errorf("`version` failed: %v", err)
	}
	line := strings.TrimSpace(out.String())
	if !strings.HasPrefix(line, appName+" ") || strings.Contains(line, "\n") {
		return "", errors.New("`version` printed an unexpected answer")
	}
	return line, nil
}

type limitedBuf struct {
	b *bytes.Buffer
	n int
}

func (l *limitedBuf) Write(p []byte) (int, error) {
	if room := l.n - l.b.Len(); room > 0 {
		l.b.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

type lastUpdate struct {
	At      string `json:"at"`
	From    string `json:"from"`
	To      string `json:"to"`
	Version string `json:"version"`
}

func lastUpdatePath() string { return filepath.Join(stateDir(), lastUpdateFile) }

func readLastUpdate() (lastUpdate, bool) {
	var lu lastUpdate
	b, err := os.ReadFile(lastUpdatePath())
	if err != nil || json.Unmarshal(b, &lu) != nil {
		return lastUpdate{}, false
	}
	return lu, true
}

func writeLastUpdate(lu lastUpdate) error {
	if err := os.MkdirAll(stateDir(), 0o700); err != nil {
		return err
	}
	b, _ := json.Marshal(lu)
	return writeFile0600(lastUpdatePath(), append(b, '\n'))
}

// watchSelf polls until ctx ends or a new binary is ready; it then records the update and
// returns true.
func (sv *server) watchSelf(ctx context.Context, u *selfUpdater, every time.Duration) bool {
	u.audit = sv.audit
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-t.C:
		}
		sha, v, ok := u.step()
		if !ok {
			continue
		}
		from, to := shortBuild(u.runningSHA), shortBuild(sha)
		sv.audit.write(auditEntry{Remote: "local", Action: "self-update", Target: from + "->" + to})
		if err := writeLastUpdate(lastUpdate{At: sv.now().In(jst).Format(time.RFC3339), From: from, To: to, Version: v}); err != nil {
			u.logf("last-update: %v", err)
		}
		u.logf("restarting into build %s (%s)", to, v)
		return true
	}
}
