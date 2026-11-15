package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func checkOut(path string, now time.Time) string {
	var b bytes.Buffer
	livenessCheck(&b, path, now)
	return b.String()
}

// The server writes "updated" and "down" every tick; liveness-check prints the open
// incidents from them, nothing when all are alive, and the stopped line when stale.
func TestLivenessCheckFromServerState(t *testing.T) {
	f, fs, _ := liveFixture(t)
	path := filepath.Join(stateDir(), livenessFile)
	if code, _ := send(t, f, "alice", "team", "dave", "secret plan for dave"); code != 200 {
		t.Fatal(code)
	}
	f.sv.live.mu.Lock()
	f.sv.live.m = map[string]*liveMember{}
	f.sv.live.mu.Unlock()
	t0 := time.Now()
	f.sv.live.touch("dave", t0)
	f.sv.live.touch("erin", t0)
	f.sv.liveTick(t0.Add(time.Minute))
	if got := checkOut(path, t0.Add(time.Minute)); got != "" {
		t.Fatalf("all alive, got %q", got)
	}
	// a tick with no change still rewrites the file (heartbeat)
	f.sv.live.touch("erin", t0.Add(9*time.Minute))
	at := t0.Add(10*time.Minute + time.Second)
	f.sv.liveTick(at)
	want := "[a2-liveness] dave no response — last seen " + t0.In(jst).Format("15:04") + ", undelivered 1\n"
	if got := checkOut(path, at.Add(time.Minute)); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	f.sv.liveTick(at.Add(5 * time.Minute))
	if got := checkOut(path, at.Add(7*time.Minute)); got != want {
		t.Fatalf("heartbeat tick: got %q", got)
	}
	if got := checkOut(path, at.Add(9*time.Minute)); !strings.Contains(got, "not updated") || strings.Count(got, "\n") != 1 {
		t.Fatalf("stale file: got %q", got)
	}
	f.sv.live.touch("dave", at.Add(6*time.Minute))
	f.sv.liveTick(at.Add(6 * time.Minute))
	if got := checkOut(path, at.Add(6*time.Minute)); got != "" {
		t.Fatalf("recovered, got %q", got)
	}
	if countWith(fs.got(), "dave no response") != 1 {
		t.Fatalf("webhook path changed: %q", fs.got())
	}
	if b, _ := os.ReadFile(path); strings.Contains(string(b), "secret plan") {
		t.Fatal("message text in the state file")
	}
}

func TestLivenessCheckMissingBadOld(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	stopped := liveCheckStopped + "\n"
	if got := checkOut(filepath.Join(dir, "none.json"), now); got != stopped {
		t.Fatalf("missing: %q", got)
	}
	bad := filepath.Join(dir, "bad.json")
	os.WriteFile(bad, []byte("{not json"), 0o600)
	if got := checkOut(bad, now); got != stopped {
		t.Fatalf("bad json: %q", got)
	}
	if got := checkOut(dir, now); got != stopped {
		t.Fatalf("directory: %q", got)
	}
	// a file from an older server: no "updated", the file time counts, no "down"
	old := filepath.Join(dir, "old.json")
	os.WriteFile(old, []byte(`{"members":{"dave":{"last":"2026-11-12T10:00:00+09:00","down":true}}}`), 0o600)
	if got := checkOut(old, now); got != "" {
		t.Fatalf("fresh old-format file: %q", got)
	}
	os.Chtimes(old, now.Add(-4*time.Minute), now.Add(-4*time.Minute))
	if got := checkOut(old, now); got != stopped {
		t.Fatalf("stale old-format file: %q", got)
	}
	// names that are not member names are skipped (they become external-monitor class keys)
	odd := filepath.Join(dir, "odd.json")
	os.WriteFile(odd, []byte(`{"members":{},"updated":"`+now.Format(time.RFC3339Nano)+`","down":[{"name":"a b","last":"`+now.Format(time.RFC3339)+`","pending":0},{"name":"ok-1","last":"`+now.Format(time.RFC3339)+`","pending":2}]}`), 0o600)
	if got := checkOut(odd, now); got != "[a2-liveness] ok-1 no response — last seen "+now.In(jst).Format("15:04")+", undelivered 2\n" {
		t.Fatalf("odd names: %q", got)
	}
}

func TestLivenessCheckCommand(t *testing.T) {
	t.Setenv(envPrefix+"STATE_DIR", t.TempDir())
	if code := run([]string{"liveness-check"}); code != 0 {
		t.Fatalf("rc %d", code)
	}
	if code := run([]string{"liveness-check", "x"}); code != 2 {
		t.Fatalf("bad args rc %d", code)
	}
}

// Only members in the work channel are watched: a general-only member
// is never reported, a work member still is, and a member that was down and then left
// work stops being reported.
func TestLivenessTeamOnly(t *testing.T) {
	f, fs, _ := liveFixture(t)
	path := filepath.Join(stateDir(), livenessFile)
	t0 := time.Now()
	f.sv.live.touch("erin", t0) // general only
	f.sv.live.touch("dave", t0) // work
	at := t0.Add(11 * time.Minute)
	f.sv.liveTick(at)
	want := "[a2-liveness] dave no response — last seen " + t0.In(jst).Format("15:04") + ", undelivered 0\n"
	if got := checkOut(path, at); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if countWith(fs.got(), "erin") != 0 || countWith(fs.got(), "dave no response") != 1 {
		t.Fatalf("webhook: %q", fs.got())
	}
	// dave leaves work: its open incident disappears from the state file
	old := memberChannels["dave"]
	memberChannels["dave"] = []string{chGeneral}
	t.Cleanup(func() { memberChannels["dave"] = old })
	f.sv.liveTick(at.Add(time.Minute))
	if got := checkOut(path, at.Add(time.Minute)); got != "" {
		t.Fatalf("left work, still reported: %q", got)
	}
	// and back in work while still silent: not alerted again until a new incident
	memberChannels["dave"] = old
	f.sv.liveTick(at.Add(2 * time.Minute))
	if got := checkOut(path, at.Add(2*time.Minute)); got != want {
		t.Fatalf("back in work: got %q", got)
	}
}
