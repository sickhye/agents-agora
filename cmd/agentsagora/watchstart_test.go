package main

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// A first watch (no cursor file) prints nothing old and starts at the newest message.
func TestWatchFirstRunStartsAtNewest(t *testing.T) {
	f := setup(t)
	send(t, f, "alice", "team", "bob", "old one")
	send(t, f, "alice", "general", "all", "old two")
	send(t, f, "alice", "team", "dave", "not for bob") // #3, not visible to bob
	wt, out, errOut := testWatcher(t, f.srv.URL, f.tokens["bob"], f.srv.Client())
	runWatcher(t, wt)
	waitCursorFile(t, wt.cp)
	if n := readCursor(wt.cp); n != 2 {
		t.Fatalf("first-run cursor %d, want 2 (newest visible)", n)
	}
	send(t, f, "alice", "team", "all", "new")
	l := waitLines(t, out, 2, 3*time.Second)
	time.Sleep(300 * time.Millisecond)
	// send-option: one backlog summary line instead of the old messages, then #4
	if l = out.lines(); len(l) != 2 || !strings.HasPrefix(l[0], "[a2] backlog: 2 messages #1-#2") || !strings.HasPrefix(l[1], "[a2] #4 [🔒team]") {
		t.Fatalf("lines %q, want the summary and #4", l)
	}
	if s := errOut.b.String(); s != "" {
		t.Errorf("stderr %q", s)
	}
}

// --from-start keeps the old behaviour: the whole history from id 1.
func TestWatchFromStart(t *testing.T) {
	f := setup(t)
	send(t, f, "alice", "team", "bob", "old one")
	send(t, f, "alice", "general", "all", "old two")
	wt, out, _ := testWatcher(t, f.srv.URL, f.tokens["bob"], f.srv.Client())
	wt.fromStart = true
	runWatcher(t, wt)
	l := waitLines(t, out, 2, 3*time.Second)
	if !strings.HasPrefix(l[0], "[a2] #1 ") || !strings.HasPrefix(l[1], "[a2] #2 ") {
		t.Fatalf("lines %q", l)
	}
}

// An existing cursor file is kept: the watch resumes after it, also without --from-start.
func TestWatchResumesFromExistingCursor(t *testing.T) {
	f := setup(t)
	for _, s := range []string{"a", "b", "c"} {
		send(t, f, "alice", "team", "all", s)
	}
	wt, out, _ := testWatcher(t, f.srv.URL, f.tokens["bob"], f.srv.Client())
	if err := writeCursor(wt.cp, 1); err != nil {
		t.Fatal(err)
	}
	runWatcher(t, wt)
	l := waitLines(t, out, 2, 3*time.Second)
	time.Sleep(200 * time.Millisecond)
	if l = out.lines(); len(l) != 2 || !strings.HasPrefix(l[0], "[a2] #2 ") || !strings.HasPrefix(l[1], "[a2] #3 ") {
		t.Fatalf("lines %q", l)
	}
}

// A one-channel first watch looks only at that channel; a general-only member never gets
// work, and the cursor it writes is the newest general id it can see.
func TestWatchFirstRunChannelAndRights(t *testing.T) {
	f := setup(t)
	send(t, f, "alice", "general", "all", "g1") // #1
	send(t, f, "alice", "team", "all", "c1")    // #2
	send(t, f, "alice", "team", "bob", "c2")    // #3
	wt, out, _ := testWatcher(t, f.srv.URL, f.tokens["bob"], f.srv.Client())
	wt.setChannel("general")
	runWatcher(t, wt)
	waitCursorFile(t, wt.cp)
	if n := readCursor(wt.cp); n != 1 {
		t.Fatalf("general cursor %d, want 1", n)
	}
	if _, err := os.Stat(wt.c.cursor); err == nil {
		t.Error("the all-channel cursor was written by a --channel watch")
	}
	send(t, f, "alice", "team", "all", "c3")    // #4, other channel
	send(t, f, "alice", "general", "all", "g2") // #5
	l := waitLines(t, out, 2, 3*time.Second)
	time.Sleep(200 * time.Millisecond)
	if l = out.lines(); len(l) != 2 || !strings.HasPrefix(l[0], "[a2] backlog: 1 messages #1-#1") || !strings.HasPrefix(l[1], "[a2] #5 [#general]") {
		t.Fatalf("lines %q", l)
	}

	// general-only member with --from-start: still no work line.
	bw, bout, _ := testWatcher(t, f.srv.URL, f.tokens["discord-bridge"], f.srv.Client())
	bw.fromStart = true
	runWatcher(t, bw)
	l = waitLines(t, bout, 2, 3*time.Second)
	time.Sleep(200 * time.Millisecond)
	for _, s := range bout.lines() {
		if strings.Contains(s, "[🔒team]") {
			t.Fatalf("general-only member got work: %q", s)
		}
	}
}

// After the first run the cursor file exists, so a restart prints what came in meanwhile.
func TestWatchRestartAfterFirstRunLosesNothing(t *testing.T) {
	f := setup(t)
	send(t, f, "alice", "team", "all", "old") // #1
	wt, _, _ := testWatcher(t, f.srv.URL, f.tokens["bob"], f.srv.Client())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- wt.run(ctx) }()
	waitCursorFile(t, wt.cp)
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	send(t, f, "alice", "team", "all", "while down") // #2
	w2, out2, _ := testWatcher(t, f.srv.URL, f.tokens["bob"], f.srv.Client())
	w2.c.cursor, w2.cp, w2.last = wt.c.cursor, wt.cp, readCursor(wt.cp)
	runWatcher(t, w2)
	l := waitLines(t, out2, 1, 3*time.Second)
	time.Sleep(200 * time.Millisecond)
	if l = out2.lines(); len(l) != 1 || !strings.HasPrefix(l[0], "[a2] #2 ") {
		t.Fatalf("lines %q", l)
	}
}

func waitCursorFile(t *testing.T, p string) {
	t.Helper()
	end := time.Now().Add(3 * time.Second)
	for time.Now().Before(end) {
		if _, err := os.Stat(p); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("cursor file %s not written", p)
}
