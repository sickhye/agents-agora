package main

// Client self-update.
//
// `a2 update` reads GET /dist/manifest from the configured server (same URL, CA pin and
// token as every other command), compares the entry for its own os/arch with the sha256 of
// its own file, downloads GET /dist/<os>-<arch> into a temp file next to itself, checks size
// and sha256 against the manifest and renames it over itself (Windows: the running .exe is
// renamed to .old first). It never runs the downloaded file for anything but being itself.
//
// Automatic update is OFF by default. With A2_AUTOUPDATE=1 in the environment, send, inbox,
// watch and mcp-stdio also check at most once per 6 h (stamp file in the state dir) and,
// after an update, re-exec themselves with the same arguments. watch and mcp-stdio check
// again every 6 h while running and re-exec between messages. A2_NO_AUTOUPDATE=1 wins over
// A2_AUTOUPDATE=1. `a2 update` always works when you run it yourself.
//
// Trust model: the only check is the sha256 in the manifest the same server publishes. There
// is no signature, so whoever can write the server's dist dir or binary can run code on every
// client that auto-updates. Enable it only for a server you fully trust (see README).

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

const (
	envAutoUpdate    = "A2_AUTOUPDATE"    // "1" turns the automatic client update on (default: off)
	envNoAutoUpdate  = "A2_NO_AUTOUPDATE" // "1" forces it off, even when A2_AUTOUPDATE=1
	envReexeced      = "A2_REEXECED"      // set on the re-exec'd process: skip the start check
	autoCheckEvery   = 6 * time.Hour
	updateStampFile  = "update-check"
	manifestTimeout  = 15 * time.Second
	downloadTimeout  = 3 * time.Minute
	maxDistFileBytes = 200 << 20
)

// updatePending is set by the periodic check of watch / mcp-stdio when a new build is on
// disk; the command re-execs at its next quiet point.
var updatePending atomic.Bool

// startExe is the path of this binary taken before any update renames it.
var startExe string

func (c *client) fetchManifest() (manifest, error) {
	var m manifest
	ctx, cancel := context.WithTimeout(context.Background(), manifestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", c.base+"/dist/manifest", nil)
	if err != nil {
		return m, err
	}
	if err := c.doReq(req, &m); err != nil {
		return m, fmt.Errorf("manifest: %w", err)
	}
	return m, nil
}

// download fetches target into a new temp file in dir and returns its path only when the
// size and sha256 match want; otherwise the temp file is removed.
func (c *client) download(target string, want distFile, dir string) (string, error) {
	if want.Size <= 0 || want.Size > maxDistFileBytes || len(want.SHA256) != 64 {
		return "", errors.New("manifest entry is not usable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), downloadTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", c.base+"/dist/"+target, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("User-Agent", userAgent())
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", serverErr(resp, data)
	}
	f, err := os.CreateTemp(dir, ".a2-update-*")
	if err != nil {
		return "", err
	}
	tmp := f.Name()
	fail := func(err error) (string, error) {
		f.Close()
		os.Remove(tmp)
		return "", err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, want.Size+1))
	if err != nil {
		return fail(err)
	}
	if n != want.Size {
		return fail(fmt.Errorf("download is %d bytes, manifest says %d; not replaced", n, want.Size))
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want.SHA256 {
		return fail(fmt.Errorf("sha256 mismatch (got %s, manifest %s); not replaced", shortBuild(got), shortBuild(want.SHA256)))
	}
	if err := f.Sync(); err != nil {
		return fail(err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return "", err
	}
	return tmp, nil
}

type updateResult struct {
	changed  bool
	from, to string // short build ids
	built    string
}

// applyUpdate brings exe to the manifest's build for this os/arch. Same build: no-op.
func applyUpdate(c *client, exe string, m manifest) (updateResult, error) {
	want, ok := m.Files[osArch()]
	if !ok {
		return updateResult{}, fmt.Errorf("the server has no %s build", osArch())
	}
	cur, _, err := hashFile(exe)
	if err != nil {
		return updateResult{}, err
	}
	res := updateResult{from: shortBuild(cur), to: want.Build, built: want.Built}
	if cur == want.SHA256 {
		return res, nil
	}
	tmp, err := c.download(osArch(), want, filepath.Dir(exe))
	if err != nil {
		return res, err
	}
	if st, err := os.Stat(exe); err == nil {
		os.Chmod(tmp, st.Mode().Perm()|0o100)
	}
	if bt, err := time.Parse(time.RFC3339, want.Built); err == nil {
		os.Chtimes(tmp, bt, bt) // `a2 version` then shows the server's build time
	}
	if err := replaceExe(exe, tmp); err != nil {
		os.Remove(tmp)
		return res, fmt.Errorf("replace %s: %w", exe, err)
	}
	res.changed = true
	return res, nil
}

// renameAside is the Windows replace: a running .exe cannot be overwritten or deleted, but
// it can be renamed. exe goes to exe.old (or exe.old-<n> while an older one is still in
// use), then tmp goes to exe; on failure exe is put back.
func renameAside(exe, tmp string, now time.Time) error {
	old := exe + ".old"
	if err := os.Remove(old); err != nil && !errors.Is(err, os.ErrNotExist) {
		old = exe + ".old-" + strconv.FormatInt(now.UnixNano(), 10)
	}
	if err := os.Rename(exe, old); err != nil {
		return err
	}
	if err := os.Rename(tmp, exe); err != nil {
		if rerr := os.Rename(old, exe); rerr != nil {
			return fmt.Errorf("%v; and putting the old binary back failed: %v (it is at %s)", err, rerr, old)
		}
		return err
	}
	return nil
}

// removeAside deletes exe.old and exe.old-* left by earlier Windows updates (best effort;
// a file still in use stays until a later run).
func removeAside(exe string) {
	os.Remove(exe + ".old")
	if ms, err := filepath.Glob(exe + ".old-*"); err == nil {
		for _, m := range ms {
			os.Remove(m)
		}
	}
}

func cmdUpdate(args []string) error {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	check := fs.Bool("check", false, "only report whether an update is available")
	if err := fs.Parse(args); err != nil {
		return usageError(err.Error())
	}
	if fs.NArg() != 0 {
		return usageError("update takes no arguments besides --check")
	}
	exe, err := selfPath()
	if err != nil {
		return err
	}
	cleanupOld(exe)
	c, err := newClient()
	if err != nil {
		return err
	}
	m, err := c.fetchManifest()
	if err != nil {
		return err
	}
	want, ok := m.Files[osArch()]
	if !ok {
		return fmt.Errorf("the server has no %s build", osArch())
	}
	cur, _, err := hashFile(exe)
	if err != nil {
		return err
	}
	if cur == want.SHA256 {
		fmt.Printf("up to date: build %s (built %s, %s)\n", shortBuild(cur), want.Built, osArch())
		return nil
	}
	if *check {
		fmt.Printf("update available: build %s -> %s (built %s, %s); run: a2 update\n", shortBuild(cur), want.Build, want.Built, osArch())
		return nil
	}
	res, err := applyUpdate(c, exe, m)
	if err != nil {
		return err
	}
	fmt.Printf("updated %s: build %s -> %s (built %s); the next a2 command runs the new build\n", exe, res.from, res.to, res.built)
	return nil
}

func autoUpdateOff() bool {
	return os.Getenv(envNoAutoUpdate) == "1" || os.Getenv(envAutoUpdate) != "1"
}

func updateStampPath() string { return filepath.Join(stateDir(), updateStampFile) }

// claimCheck returns true (and moves the stamp to now) when the last check is 6 h old or
// more; the stamp moves even if the check then fails, so a broken server costs one try per 6 h.
func claimCheck(now time.Time) bool {
	p := updateStampPath()
	if st, err := os.Stat(p); err == nil && now.Sub(st.ModTime()) < autoCheckEvery && st.ModTime().Before(now.Add(time.Minute)) {
		return false
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return false
	}
	if err := writeFile0600(p, []byte(now.In(jst).Format(time.RFC3339)+"\n")); err != nil {
		return false
	}
	os.Chtimes(p, now, now)
	return true
}

// autoCheck runs one automatic check (if due) and reports whether exe was replaced.
func autoCheck(exe string, now time.Time) bool {
	if !claimCheck(now) {
		return false
	}
	c, err := newClient()
	if err != nil {
		return false // the command itself reports the config problem
	}
	m, err := c.fetchManifest()
	if err == nil {
		var res updateResult
		res, err = applyUpdate(c, exe, m)
		if err == nil && res.changed {
			fmt.Fprintf(os.Stderr, "a2: notice: updated itself, build %s -> %s (automatic update is on because %s=1)\n", res.from, res.to, envAutoUpdate)
			return true
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "a2: notice: update check failed (the command runs anyway):", err)
	}
	return false
}

// autoUpdate runs before send, inbox, watch and mcp-stdio. After an update it re-execs the
// new binary with the same arguments; watch and mcp-stdio also get the 6 h periodic check.
func autoUpdate(args []string) {
	if autoUpdateOff() {
		return
	}
	exe, err := selfPath()
	if err != nil {
		return
	}
	startExe = exe
	cleanupOld(exe)
	if os.Getenv(envReexeced) == "" && autoCheck(exe, time.Now()) {
		reexec(args)
	}
	if args[0] == "watch" || args[0] == "mcp-stdio" {
		go periodicUpdate(exe, autoCheckEvery)
	}
}

// periodicUpdate marks updatePending when this binary was replaced on disk, by its own
// check or by anyone else (e.g. a rebuild on the server host or another a2 process).
func periodicUpdate(exe string, every time.Duration) {
	startSHA, _, err := hashFile(exe)
	if err != nil {
		return
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for range t.C {
		autoCheck(exe, time.Now())
		if sha, _, err := hashFile(exe); err == nil && sha != startSHA {
			updatePending.Store(true)
			return
		}
	}
}

// reexecIfPending is called by watch and mcp-stdio at a quiet point.
func reexecIfPending(args []string) {
	if updatePending.Load() {
		reexec(args)
		updatePending.Store(false) // re-exec failed (notice printed): keep running
	}
}

// reexecEnv is the env of the re-exec after an update. reexec gets the args from the command
// on, so a global --token-file goes through the env (same meaning; the flag already wins over
// any env value, so replacing AGENTSAGORA_TOKEN_FILE changes nothing else).
func reexecEnv() []string { return reexecEnvFrom(os.Environ()) }

func reexecEnvFrom(environ []string) []string {
	env := append(environ[:len(environ):len(environ)], envReexeced+"=1")
	if tokenFileFlag == "" {
		return env
	}
	key := envPrefix + "TOKEN_FILE="
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if !strings.HasPrefix(kv, key) {
			out = append(out, kv)
		}
	}
	return append(out, key+tokenFileFlag)
}

func reexecPath() string {
	if startExe != "" {
		return startExe
	}
	p, _ := selfPath()
	return p
}
