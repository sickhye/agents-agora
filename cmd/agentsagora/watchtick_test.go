package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for i := 0; i < 300; i++ {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timeout: %s", what)
}

func TestWatchTickWrittenAtStartAndOnTick(t *testing.T) {
	f := setup(t)
	wt, _, errOut := testWatcher(t, f.srv.URL, f.tokens["bob"], f.srv.Client())
	p := filepath.Join(t.TempDir(), "sub", "watch-tick-all")
	var n atomic.Int64
	base := time.Date(2026, 11, 15, 9, 0, 0, 0, time.FixedZone("JST", 9*3600))
	wt.tickPath, wt.tickEvery = p, 20*time.Millisecond
	wt.tickNow = func() time.Time { return base.Add(time.Duration(n.Add(1)) * time.Minute) }
	runWatcher(t, wt)
	waitFor(t, "first stamp", func() bool { _, err := os.Stat(p); return err == nil })
	b, _ := os.ReadFile(p)
	if _, err := time.Parse(time.RFC3339, strings.TrimSuffix(string(b), "\n")); err != nil || !strings.HasSuffix(string(b), "\n") || !strings.Contains(string(b), "+09:00") {
		t.Fatalf("stamp %q", b)
	}
	st, _ := os.Stat(p)
	if st.Mode().Perm() != 0o644 {
		t.Errorf("mode %v", st.Mode())
	}
	first := string(b)
	waitFor(t, "rewrite", func() bool { b, _ := os.ReadFile(p); return string(b) != first })
	if s := errOut.b.String(); s != "" {
		t.Errorf("stderr %q", s)
	}
}

func TestWatchTickOffWritesNothing(t *testing.T) {
	f := setup(t)
	wt, _, _ := testWatcher(t, f.srv.URL, f.tokens["bob"], f.srv.Client())
	d := t.TempDir()
	wt.tickPath, wt.tickEvery = tickOff, 5*time.Millisecond
	runWatcher(t, wt)
	time.Sleep(100 * time.Millisecond)
	if ents, _ := os.ReadDir(d); len(ents) != 0 {
		t.Fatalf("files %v", ents)
	}
	if _, err := os.Stat(tickOff); err == nil {
		t.Fatal("wrote a file named off")
	}
}

func TestTickWriteErrorLoggedOnce(t *testing.T) {
	d := t.TempDir()
	blocker := filepath.Join(d, "f")
	os.WriteFile(blocker, nil, 0o644)
	var buf syncBuf
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	runTick(ctx, filepath.Join(blocker, "x"), 10*time.Millisecond, time.Now, &buf)
	if got := strings.Count(buf.b.String(), "a2 watch: tick: "); got != 1 {
		t.Fatalf("logged %d times: %q", got, buf.b.String())
	}
}

func TestWatchStatusCodes(t *testing.T) {
	now := time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC)
	p := filepath.Join(t.TempDir(), "tick")
	if c, l := watchStatus(p, 15*time.Minute, now); c != 2 || !strings.HasSuffix(l, "missing") {
		t.Fatalf("missing: %d %q", c, l)
	}
	os.WriteFile(p, []byte(now.Add(-time.Minute).Format(time.RFC3339)+"\n"), 0o644)
	if c, l := watchStatus(p, 15*time.Minute, now); c != 0 || !strings.HasSuffix(l, "age=1m0s live") {
		t.Fatalf("live: %d %q", c, l)
	}
	os.WriteFile(p, []byte(now.Add(-20*time.Minute).Format(time.RFC3339)+"\n"), 0o644)
	if c, l := watchStatus(p, 15*time.Minute, now); c != 1 || !strings.HasSuffix(l, "stale") {
		t.Fatalf("stale: %d %q", c, l)
	}
}

func TestWatchStatusCommandExit(t *testing.T) {
	p := filepath.Join(t.TempDir(), "none")
	if code := run([]string{"watch-status", "--tick-file", p}); code != 2 {
		t.Fatalf("exit %d", code)
	}
	writeTick(p, time.Now())
	if code := run([]string{"watch-status", "--tick-file", p}); code != 0 {
		t.Fatalf("exit %d", code)
	}
}

func TestTickDefaultPath(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "/x/state")
	if got := tickDefaultPath(""); got != "/x/state/agentsagora/watch-tick-all" {
		t.Fatal(got)
	}
	if got := tickDefaultPath("team"); got != "/x/state/agentsagora/watch-tick-team" {
		t.Fatal(got)
	}
}
