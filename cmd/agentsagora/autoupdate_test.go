package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func sha(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

// distFixture: a server with a temp dist dir holding two of the four targets.
func distFixture(t *testing.T) (*fixture, string) {
	t.Helper()
	f := setup(t)
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "agentsagora-darwin-arm64"), []byte("mac arm build"), 0o755)
	os.WriteFile(filepath.Join(dir, "agentsagora"), []byte("linux build"), 0o755)
	os.WriteFile(filepath.Join(dir, "secret.txt"), []byte("not for clients"), 0o600)
	f.sv.dist = newDistCache(dir)
	return f, dir
}

func getRaw(t *testing.T, f *fixture, path, tok string) (int, []byte) {
	t.Helper()
	r, _ := http.NewRequest("GET", f.srv.URL+path, nil)
	if tok != "" {
		r.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func TestDistAuthAndFixedList(t *testing.T) {
	f, _ := distFixture(t)
	tok := f.tokens["frank"] // any member token, general-only members included
	for _, p := range []string{"/dist/manifest", "/dist/darwin-arm64", "/dist/nope", "/dist/a/b"} {
		if code, _ := getRaw(t, f, p, ""); code != 401 {
			t.Errorf("%s without token: %d, want 401", p, code)
		}
		if code, _ := getRaw(t, f, p, "sc_wrong"); code != 401 {
			t.Errorf("%s with a bad token: %d, want 401", p, code)
		}
	}
	code, b := getRaw(t, f, "/dist/manifest", tok)
	if code != 200 {
		t.Fatalf("manifest: %d %s", code, b)
	}
	var m manifest
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if got := m.Files["darwin-arm64"]; got.SHA256 != sha([]byte("mac arm build")) || got.Size != 13 || got.Path != "/dist/darwin-arm64" {
		t.Errorf("darwin-arm64 entry %+v", got)
	}
	if _, ok := m.Files["windows-amd64"]; ok || len(m.Files) != 2 {
		t.Errorf("manifest lists files that are not built: %+v", m.Files)
	}
	if lf := m.Files["linux-amd64"]; m.Version != lf.Build+"@"+lf.Built || m.Server.App != appName {
		t.Errorf("version %q server %+v", m.Version, m.Server)
	}
	if code, b := getRaw(t, f, "/dist/darwin-arm64", tok); code != 200 || string(b) != "mac arm build" {
		t.Errorf("download: %d %q", code, b)
	}
	for _, p := range []string{"/dist/nope", "/dist/secret.txt", "/dist/agentsagora", "/dist/a/b", "/dist/windows-amd64", "/dist/..%2fsecret.txt"} {
		if code, b := getRaw(t, f, p, tok); code != 404 || strings.Contains(string(b), "not for clients") {
			t.Errorf("%s: %d %q, want 404", p, code, b)
		}
	}
	a, _ := os.ReadFile(filepath.Join(stateDir(), "audit.jsonl"))
	if !strings.Contains(string(a), `"action":"dist-get"`) {
		t.Error("no dist-get audit line")
	}
}

// updateServer serves a manifest for this os/arch and counts downloads.
func updateServer(t *testing.T, body []byte, manifestSHA string) (*client, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("GET /dist/manifest", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(401)
			return
		}
		writeJSON(w, 200, manifest{App: appName, Files: map[string]distFile{osArch(): {Build: shortBuild(manifestSHA), SHA256: manifestSHA, Size: int64(len(body)), Built: "2026-11-12T12:00:00+09:00"}}})
	})
	mux.HandleFunc("GET /dist/{target}", func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.PathValue("target") != osArch() {
			w.WriteHeader(404)
			return
		}
		w.Write(body)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &client{base: srv.URL, token: "tok", http: srv.Client()}, &hits
}

func exeIn(t *testing.T, content string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	exe := filepath.Join(dir, "a2")
	if err := os.WriteFile(exe, []byte(content), 0o751); err != nil {
		t.Fatal(err)
	}
	return dir, exe
}

func leftovers(t *testing.T, dir string) []string {
	t.Helper()
	ms, _ := filepath.Glob(filepath.Join(dir, ".a2-update-*"))
	return ms
}

func TestUpdateShaMismatchKeepsBinary(t *testing.T) {
	newBin := []byte("new build")
	c, _ := updateServer(t, newBin, sha([]byte("something else!!")))
	dir, exe := exeIn(t, "old build")
	m, err := c.fetchManifest()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := applyUpdate(c, exe, m); err == nil || !strings.Contains(err.Error(), "not replaced") {
		t.Fatalf("want a mismatch error, got %v", err)
	}
	if b, _ := os.ReadFile(exe); string(b) != "old build" {
		t.Errorf("binary changed to %q", b)
	}
	if l := leftovers(t, dir); len(l) != 0 {
		t.Errorf("temp files left: %v", l)
	}
	// a short download (size differs) is refused too
	c2, _ := updateServer(t, newBin[:3], sha(newBin))
	m2, _ := c2.fetchManifest()
	m2.Files[osArch()] = distFile{SHA256: sha(newBin), Size: int64(len(newBin))}
	if _, err := applyUpdate(c2, exe, m2); err == nil {
		t.Error("short download accepted")
	}
	if b, _ := os.ReadFile(exe); string(b) != "old build" {
		t.Errorf("binary changed to %q", b)
	}
}

func TestUpdateSameVersionNoop(t *testing.T) {
	c, hits := updateServer(t, []byte("same build"), sha([]byte("same build")))
	_, exe := exeIn(t, "same build")
	st0, _ := os.Stat(exe)
	m, err := c.fetchManifest()
	if err != nil {
		t.Fatal(err)
	}
	res, err := applyUpdate(c, exe, m)
	if err != nil || res.changed {
		t.Fatalf("res %+v err %v", res, err)
	}
	if hits.Load() != 0 {
		t.Errorf("downloaded %d times for the same build", hits.Load())
	}
	st1, _ := os.Stat(exe)
	if !os.SameFile(st0, st1) || !st0.ModTime().Equal(st1.ModTime()) {
		t.Error("binary was touched")
	}
}

func TestUpdateReplacesAtomically(t *testing.T) {
	newBin := []byte("brand new build")
	c, _ := updateServer(t, newBin, sha(newBin))
	dir, exe := exeIn(t, "old build")
	st0, _ := os.Stat(exe)
	m, _ := c.fetchManifest()
	res, err := applyUpdate(c, exe, m)
	if err != nil || !res.changed || res.to != shortBuild(sha(newBin)) {
		t.Fatalf("res %+v err %v", res, err)
	}
	b, _ := os.ReadFile(exe)
	st1, _ := os.Stat(exe)
	if string(b) != string(newBin) {
		t.Errorf("content %q", b)
	}
	if os.SameFile(st0, st1) {
		t.Error("replaced in place (want a new file renamed over the old one)")
	}
	if st1.Mode().Perm() != 0o751 {
		t.Errorf("mode %v, want the old mode 0751", st1.Mode().Perm())
	}
	if want, _ := time.Parse(time.RFC3339, "2026-11-12T12:00:00+09:00"); !st1.ModTime().Equal(want) {
		t.Errorf("mtime %v, want the build time from the manifest", st1.ModTime())
	}
	if l := leftovers(t, dir); len(l) != 0 {
		t.Errorf("temp files left: %v", l)
	}
}

// The Windows path (rename the running exe aside) is plain file renames, so it runs here
// too; replaceExe itself is compiled for windows by `GOOS=windows go vet`.
func TestRenameAside(t *testing.T) {
	dir, exe := exeIn(t, "running")
	tmp := filepath.Join(dir, ".a2-update-1")
	os.WriteFile(tmp, []byte("new"), 0o755)
	if err := renameAside(exe, tmp, time.Unix(1, 0)); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(exe); string(b) != "new" {
		t.Errorf("exe %q", b)
	}
	if b, _ := os.ReadFile(exe + ".old"); string(b) != "running" {
		t.Errorf(".old %q", b)
	}
	// .old that cannot be removed (here: a non-empty dir) -> a numbered name is used
	os.Remove(exe + ".old")
	os.MkdirAll(filepath.Join(exe+".old", "busy"), 0o700)
	tmp2 := filepath.Join(dir, ".a2-update-2")
	os.WriteFile(tmp2, []byte("newer"), 0o755)
	if err := renameAside(exe, tmp2, time.Unix(2, 0)); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(exe + ".old-2000000000"); string(b) != "new" {
		t.Errorf("numbered old %q", b)
	}
	// failure puts the old binary back
	if err := renameAside(exe, filepath.Join(dir, "missing"), time.Unix(3, 0)); err == nil {
		t.Error("missing tmp accepted")
	}
	if b, _ := os.ReadFile(exe); string(b) != "newer" {
		t.Errorf("exe after failed replace %q", b)
	}
	removeAside(exe)
	if _, err := os.Stat(exe + ".old-2000000000"); err == nil {
		t.Error("numbered old not removed")
	}
}

func TestClaimCheckEverySixHours(t *testing.T) {
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, jst)
	if !claimCheck(now) {
		t.Fatal("first check not due")
	}
	if claimCheck(now.Add(5 * time.Hour)) {
		t.Error("checked again after 5 h")
	}
	if !claimCheck(now.Add(6*time.Hour + time.Second)) {
		t.Error("not due after 6 h")
	}
	if st, _ := os.Stat(updateStampPath()); st.Mode().Perm() != 0o600 {
		t.Errorf("stamp mode %v", st.Mode().Perm())
	}
}

func TestAutoUpdateOffByDefault(t *testing.T) {
	t.Setenv(envNoAutoUpdate, "")
	t.Setenv(envAutoUpdate, "")
	if !autoUpdateOff() {
		t.Error("automatic update is on without opt-in")
	}
	os.Remove(updateStampPath())
	autoUpdate([]string{"send"})
	if _, err := os.Stat(updateStampPath()); err == nil {
		t.Error("checked without opt-in")
	}
	t.Setenv(envAutoUpdate, "1")
	if autoUpdateOff() {
		t.Error("A2_AUTOUPDATE=1 did not turn it on")
	}
}

func TestAutoUpdateOptOut(t *testing.T) {
	t.Setenv(envAutoUpdate, "1")
	t.Setenv(envNoAutoUpdate, "1")
	os.Remove(updateStampPath())
	autoUpdate([]string{"send"})
	if _, err := os.Stat(updateStampPath()); err == nil {
		t.Error("opt-out still checked")
	}
}

func TestSelfUpdaterDebounce(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "agentsagora")
	os.WriteFile(p, []byte("running build"), 0o755)
	t0 := time.Now().Add(-time.Hour)
	os.Chtimes(p, t0, t0)
	clock := t0.Add(10 * time.Minute)
	u := newSelfUpdater(p, sha([]byte("running build")), func() time.Time { return clock })
	checks := 0
	u.check = func(string) (string, error) { checks++; return "agentsagora new", nil }
	tick := func(d time.Duration) bool { clock = clock.Add(d); _, _, ok := u.step(); return ok }

	if tick(time.Second) {
		t.Fatal("unchanged file picked up")
	}
	write := func(s string) {
		tmp := p + ".new"
		os.WriteFile(tmp, []byte(s), 0o755)
		os.Chtimes(tmp, clock, clock)
		os.Rename(tmp, p)
	}
	write("half")
	for i := 0; i < 14; i++ { // 28 s of polls
		if tick(2 * time.Second) {
			t.Fatal("picked up within 30 s of the write")
		}
	}
	write("whole build") // the build writes again: the 30 s start over
	for i := 0; i < 14; i++ {
		if tick(2 * time.Second) {
			t.Fatal("picked up within 30 s of the second write")
		}
	}
	if !tick(4 * time.Second) {
		t.Fatal("not picked up 30 s after the last write")
	}
	if checks != 1 {
		t.Errorf("version check ran %d times", checks)
	}
	if tick(time.Minute) {
		t.Error("same file picked up twice")
	}

	// a file equal to the running build is not an update
	u2 := newSelfUpdater(p, sha([]byte("whole build")), func() time.Time { return clock })
	u2.check = u.check
	write("whole build")
	clock = clock.Add(time.Minute)
	u2.step()
	clock = clock.Add(time.Minute)
	if _, _, ok := u2.step(); ok {
		t.Error("same build counted as an update")
	}

	// a binary that fails `version` is refused once and not retried
	u3 := newSelfUpdater(p, "other", func() time.Time { return clock })
	n3 := 0
	u3.check = func(string) (string, error) { n3++; return "", io.ErrUnexpectedEOF }
	u3.logf = func(string, ...any) {}
	write("broken build")
	for i := 0; i < 60; i++ {
		clock = clock.Add(2 * time.Second)
		if _, _, ok := u3.step(); ok {
			t.Fatal("broken build accepted")
		}
	}
	if n3 != 1 {
		t.Errorf("broken build checked %d times, want 1", n3)
	}
}

func TestVersionCheckExec(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("A2_TEST_HELPER", "version")
	if v, err := versionCheckTimeout(exe, 10*time.Second); err != nil || !strings.HasPrefix(v, "agentsagora ") {
		t.Errorf("good binary: %q %v", v, err)
	}
	t.Setenv("A2_TEST_HELPER", "bad")
	if _, err := versionCheckTimeout(exe, 10*time.Second); err == nil {
		t.Error("unexpected answer accepted")
	}
	t.Setenv("A2_TEST_HELPER", "hang")
	start := time.Now()
	if _, err := versionCheckTimeout(exe, 300*time.Millisecond); err == nil || !strings.Contains(err.Error(), "did not answer") {
		t.Errorf("hang: %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Error("timeout not enforced")
	}
	if _, err := versionCheckTimeout(filepath.Join(t.TempDir(), "missing"), time.Second); err == nil {
		t.Error("missing binary accepted")
	}
}

// watchSelf records the update (audit line, last-update file) and the long-poll inbox
// answers at once when the server goes down.
func TestWatchSelfAndShutdownWakesLongPoll(t *testing.T) {
	f := setup(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "agentsagora")
	os.WriteFile(p, []byte("new build"), 0o755)
	old := time.Now().Add(-time.Hour)
	os.Chtimes(p, old, old)
	u := newSelfUpdater(p, "running", time.Now)
	u.last, u.checked = nil, nil // as if the file changed after start
	u.check = func(string) (string, error) { return "agentsagora next", nil }
	u.debounce = 0
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if !f.sv.watchSelf(ctx, u, 10*time.Millisecond) {
		t.Fatal("update not detected")
	}
	a, _ := os.ReadFile(filepath.Join(stateDir(), "audit.jsonl"))
	if !strings.Contains(string(a), `"action":"self-update"`) {
		t.Error("no self-update audit line")
	}
	lu, ok := readLastUpdate()
	if !ok || lu.To != shortBuild(sha([]byte("new build"))) || lu.Version != "agentsagora next" {
		t.Errorf("last update %+v", lu)
	}
	if st := f.sv.serverStatus(); st.LastUpdate == nil {
		t.Error("server status has no last update")
	}

	done := make(chan int, 1)
	go func() {
		code, _ := f.req(t, "GET", "/inbox?wait=60", f.tokens["alice"], nil)
		done <- code
	}()
	time.Sleep(200 * time.Millisecond)
	close(f.sv.quit)
	select {
	case code := <-done:
		if code != 200 {
			t.Errorf("long-poll answered %d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("long-poll still held after quit")
	}
}
