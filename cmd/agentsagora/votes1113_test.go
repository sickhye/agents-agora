package main

// Tests for the send-option items: watch resume cursor (#1), server restart line (#2),
// backlog summary (#3), needs-reply (#4), log-only (#5), status digest (#6), and that a
// plain send/watch is unchanged.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// #1: a re-run prints only what came after the last line it printed, even when a2 inbox
// moved its own cursor meanwhile; nothing printed twice.
func TestWatchOwnCursorResume(t *testing.T) {
	f := setup(t)
	send(t, f, "alice", "team", "all", "one") // #1
	wt, out, _ := testWatcher(t, f.srv.URL, f.tokens["bob"], f.srv.Client())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- wt.run(ctx) }()
	waitCursorFile(t, wt.cp)
	send(t, f, "alice", "team", "all", "two") // #2
	waitLines(t, out, 2, 3*time.Second)       // summary of #1, then #2
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(wt.cp, "-watch-all") || wt.cp == wt.inboxCp {
		t.Fatalf("watch cursor %q (inbox %q)", wt.cp, wt.inboxCp)
	}
	send(t, f, "alice", "team", "all", "three")        // #3
	send(t, f, "alice", "general", "all", "four")      // #4
	if err := writeCursor(wt.inboxCp, 4); err != nil { // a2 inbox read them meanwhile
		t.Fatal(err)
	}
	w2, out2, errOut2 := testWatcher(t, f.srv.URL, f.tokens["bob"], f.srv.Client())
	w2.c.cursor = wt.c.cursor
	w2.setChannel("")
	runWatcher(t, w2)
	l := waitLines(t, out2, 2, 3*time.Second)
	time.Sleep(200 * time.Millisecond)
	if l = out2.lines(); len(l) != 2 || !strings.HasPrefix(l[0], "[a2] #3 ") || !strings.HasPrefix(l[1], "[a2] #4 ") {
		t.Fatalf("re-run lines %q, want #3 #4 only", l)
	}
	if s := strings.Join(errOut2.lines(), "\n"); s != "" {
		t.Errorf("stderr %q", s)
	}
	if n := readCursor(w2.cp); n != 4 {
		t.Errorf("watch cursor %d", n)
	}
}

// #1: first run of a new client after an update: the old shared cursor is the base.
func TestWatchLegacyCursorUpgrade(t *testing.T) {
	f := setup(t)
	for _, s := range []string{"a", "b", "c"} {
		send(t, f, "alice", "team", "all", s)
	}
	wt, out, _ := testWatcher(t, f.srv.URL, f.tokens["bob"], f.srv.Client())
	if err := writeCursor(wt.inboxCp, 1); err != nil {
		t.Fatal(err)
	}
	runWatcher(t, wt)
	l := waitLines(t, out, 2, 3*time.Second)
	time.Sleep(200 * time.Millisecond)
	if l = out.lines(); len(l) != 2 || !strings.HasPrefix(l[0], "[a2] #2 ") || !strings.HasPrefix(l[1], "[a2] #3 ") {
		t.Fatalf("lines %q", l)
	}
	if n := readCursor(wt.inboxCp); n != 3 {
		t.Errorf("inbox cursor %d, want 3 (moved as before)", n)
	}
}

// #3: more than backlogMax waiting messages after the cursor: one summary line, the
// cursor moves to the newest, the a2 inbox cursor stays (a2 inbox shows the full text).
func TestWatchBacklogSummary(t *testing.T) {
	f := setup(t)
	send(t, f, "alice", "team", "all", "1")
	send(t, f, "dave", "team", "all", "2")
	send(t, f, "alice", "team", "bob", "3")
	send(t, f, "alice", "general", "all", "4")
	send(t, f, "dave", "team", "bob", "5")
	wt, out, _ := testWatcher(t, f.srv.URL, f.tokens["bob"], f.srv.Client())
	wt.backlogMax = 3
	if err := writeCursor(wt.cp, 0); err != nil {
		t.Fatal(err)
	}
	runWatcher(t, wt)
	waitLines(t, out, 1, 3*time.Second)
	send(t, f, "alice", "team", "all", "6")
	l := waitLines(t, out, 2, 3*time.Second)
	time.Sleep(200 * time.Millisecond)
	l = out.lines()
	want := "[a2] backlog: 5 messages #1-#5 not printed: alice 3 (#4 #3 #1), dave 2 (#5 #2). Full text: a2 inbox --since 0"
	if len(l) != 2 || l[0] != want || !strings.HasPrefix(l[1], "[a2] #6 ") {
		t.Fatalf("lines %q\nwant %q and #6", l, want)
	}
	if _, err := os.Stat(wt.inboxCp); err == nil {
		t.Error("the a2 inbox cursor moved after a summary")
	}
}

// #2: a server restart (close "server stopping", down a moment, back) gives one stderr line.
func TestWatchServerRestartOneLine(t *testing.T) {
	f := setup(t)
	var cur atomic.Value
	cur.Store(f.sv.handler())
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cur.Load().(http.Handler).ServeHTTP(w, r)
	}))
	t.Cleanup(proxy.Close)
	wt, out, errOut := testWatcher(t, proxy.URL, f.tokens["bob"], proxy.Client())
	runWatcher(t, wt)
	waitCursorFile(t, wt.cp)
	time.Sleep(200 * time.Millisecond)
	cur.Store(http.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	})))
	close(f.sv.quit) // sockets get 1001 "server stopping"
	time.Sleep(500 * time.Millisecond)
	sv2, err := newServer(testMembers, nil, denyFile)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sv2.ws.dropAll(); sv2.ws.wait(5 * time.Second) })
	cur.Store(sv2.handler())
	end := time.Now().Add(5 * time.Second)
	for !strings.Contains(strings.Join(errOut.lines(), "\n"), "reconnected") && time.Now().Before(end) {
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := sv2.send("alice", "team", "all", "after restart", 0); err != nil {
		t.Fatal(err)
	}
	waitLines(t, out, 1, 3*time.Second)
	errs := errOut.lines()
	if len(errs) != 1 || !strings.HasPrefix(errs[0], "[a2] server restart, reconnected after ") || !strings.HasSuffix(errs[0], "s") {
		t.Fatalf("stderr %q, want one restart line", errs)
	}
}

// #2: a plain drop still prints nothing on stdout but the messages; the restart line is
// only for a stopping server.
func TestRestartCloseOnlyForStop(t *testing.T) {
	if !restartClose(&wsCloseError{wsCloseRestart, ""}) || !restartClose(&wsCloseError{wsCloseGoingAway, "server stopping"}) {
		t.Error("stop not seen")
	}
	if restartClose(&wsCloseError{wsCloseGoingAway, "pong timeout"}) || restartClose(nil) || restartClose(os.ErrClosed) {
		t.Error("not a restart")
	}
}

// Plain send is unchanged: no new keys in the answer or the stored line.
func TestPlainSendUnchanged(t *testing.T) {
	f := setup(t)
	code, out := send(t, f, "alice", "team", "bob", "plain")
	if code != 200 {
		t.Fatalf("code %d", code)
	}
	for _, k := range []string{"kind", "reply_by", "queued"} {
		if _, ok := out[k]; ok {
			t.Errorf("answer has %q: %v", k, out)
		}
	}
	b, err := os.ReadFile(filepath.Join(stateDir(), "messages.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if s := string(b); strings.Contains(s, "kind") || strings.Contains(s, "reply_by") || strings.Contains(s, "queued") {
		t.Errorf("stored line %s", s)
	}
	m := Message{ID: 1, Channel: "team", TS: "t", From: "alice", To: "bob", Text: "plain"}
	if watchLine(m) != formatLine(m) || formatLine(m) != "[a2] #1 [🔒team] t alice -> bob: plain" {
		t.Errorf("line %q", watchLine(m))
	}
}

// #5: --log-only is stored with kind log; watch shows one short [log] line.
func TestLogOnly(t *testing.T) {
	f := setup(t)
	long := strings.Repeat("handoff done ", 20) + "\nsecond line"
	code, out := f.req(t, "POST", "/send", f.tokens["alice"], sendReq{To: "all", Channel: "team", Text: long, Kind: kindLog})
	if code != 200 || out["kind"] != kindLog {
		t.Fatalf("%d %v", code, out)
	}
	m, _ := f.sv.store.get(1)
	if m.Kind != kindLog || m.Text != long {
		t.Fatalf("stored %+v", m)
	}
	l := watchLine(m)
	if !strings.HasPrefix(l, "[a2] [log] #1 alice: handoff done") || strings.Contains(l, `\n`) || len([]rune(l)) > len("[a2] [log] #1 alice: ")+logLineMax {
		t.Errorf("watch line %q", l)
	}
	if fl := formatLine(m); !strings.Contains(fl, " [log]: ") || !strings.HasSuffix(fl, `\nsecond line`) {
		t.Errorf("inbox line %q", fl)
	}
	if code, _ := f.req(t, "POST", "/send", f.tokens["alice"], sendReq{To: "all", Channel: "team", Text: "x", Kind: "loud"}); code != 400 {
		t.Errorf("unknown kind: %d", code)
	}
}

// #4: no reply in time: one reminder to each side, once; a reply in the thread stops it.
func TestNeedsReplyReminderOnce(t *testing.T) {
	f := setup(t)
	t0 := time.Date(2026, 11, 13, 21, 0, 0, 0, jst)
	f.sv.now = func() time.Time { return t0 }
	code, out := f.req(t, "POST", "/send", f.tokens["alice"], sendReq{To: "bob", Channel: "team", Text: "please check", NeedsReplySec: 1800})
	if code != 200 || out["reply_by"] != t0.Add(30*time.Minute).Format(time.RFC3339) {
		t.Fatalf("%d %v", code, out)
	}
	// #2: answered in time (reply to a reply in the thread counts too)
	f.req(t, "POST", "/send", f.tokens["alice"], sendReq{To: "dave", Channel: "team", Text: "and you", NeedsReplySec: 600})
	f.req(t, "POST", "/send", f.tokens["dave"], sendReq{To: "alice", Channel: "team", Text: "ok", ReplyTo: 2})
	f.sv.replyTick(t0.Add(10 * time.Minute)) // #1 not due yet, #2 answered
	if n := len(f.sv.store.after(3)); n != 0 {
		t.Fatalf("%d messages before the deadline", n)
	}
	f.sv.replyTick(t0.Add(31 * time.Minute))
	rs := f.sv.store.after(3)
	if len(rs) != 2 {
		t.Fatalf("reminders %+v", rs)
	}
	for i, to := range []string{"bob", "alice"} {
		r := rs[i]
		if r.From != systemSender || r.To != to || r.Kind != kindReminder || r.ReplyTo != 1 || !strings.HasPrefix(r.Text, "#1 ") {
			t.Errorf("reminder %d %+v", i, r)
		}
	}
	f.sv.replyTick(t0.Add(45 * time.Minute))
	if n := len(f.sv.store.after(3)); n != 2 {
		t.Errorf("%d messages, want the 2 reminders only (once)", n)
	}
	l, err := openReplyLog(filepath.Join(stateDir(), replyRemindFile))
	if err != nil || !l.done[1] {
		t.Errorf("mark not kept for a restart: %v %v", l, err)
	}
	for _, bad := range []sendReq{
		{To: "all", Channel: "team", Text: "x", NeedsReplySec: 1800},
		{To: "dave", Channel: "team", Text: "x", NeedsReplySec: 10},
		{To: "dave", Channel: "team", Text: "x", NeedsReplySec: 1800, Kind: kindLog},
		{To: "dave", Channel: "team", Text: "x", NeedsReplySec: 1 << 62},
	} {
		if code, _ := f.req(t, "POST", "/send", f.tokens["alice"], bad); code != 400 {
			t.Errorf("%+v: %d, want 400", bad, code)
		}
	}
}

// #6: status lines of one sender wait and go out as one digest per cycle; a problem or
// block goes out at once (after the waiting lines of its group).
func TestStatusDigest(t *testing.T) {
	f := setup(t)
	t0 := time.Date(2026, 11, 13, 21, 0, 0, 0, jst)
	at := func(m int) { f.sv.now = func() time.Time { return t0.Add(time.Duration(m) * time.Minute) } }
	status := func(who, text string) map[string]any {
		t.Helper()
		code, out := f.req(t, "POST", "/send", f.tokens[who], sendReq{To: "alice", Channel: "team", Text: text, Kind: kindStatus})
		if code != 200 {
			t.Fatalf("%d %v", code, out)
		}
		return out
	}
	at(0)
	if out := status("dave", "build 1/3"); out["queued"] != t0.Add(10*time.Minute).Format(time.RFC3339) || out["id"] != float64(0) {
		t.Fatalf("not queued: %v", out)
	}
	at(2)
	status("dave", "build 2/3")
	status("bob", "idle")
	f.sv.digestTick(t0.Add(9 * time.Minute))
	if n := len(f.sv.store.after(0)); n != 0 {
		t.Fatalf("%d messages before the cycle", n)
	}
	// a restart keeps the waiting lines
	q, err := openDigest(filepath.Join(stateDir(), digestFile), 10*time.Minute)
	if err != nil || len(q.items) != 3 {
		t.Fatalf("file %v %v", q, err)
	}
	at(3)
	if out := status("dave", "OUTCOME: problem tests fail"); out["kind"] != kindStatus || out["queued"] != nil {
		t.Fatalf("urgent was queued: %v", out)
	}
	ms := f.sv.store.after(0)
	if len(ms) != 2 || ms[0].Kind != kindDigest || ms[0].From != "dave" ||
		ms[0].Text != "status digest: 2 lines, 21:00-21:02\n21:00 build 1/3\n21:02 build 2/3" || ms[1].Text != "OUTCOME: problem tests fail" {
		t.Fatalf("messages %+v", ms)
	}
	f.sv.digestTick(t0.Add(12 * time.Minute)) // bob's line waited 10 min
	ms = f.sv.store.after(2)
	if len(ms) != 1 || ms[0].From != "bob" || ms[0].Kind != kindDigest || !strings.HasSuffix(ms[0].Text, "\n21:02 idle") {
		t.Fatalf("messages %+v", ms)
	}
	f.sv.digestTick(t0.Add(30 * time.Minute))
	if n := len(f.sv.store.after(3)); n != 0 {
		t.Errorf("%d extra messages", n)
	}
	// off: at once
	f.sv.digest.every = 0
	if out := status("dave", "plain status"); out["queued"] != nil || out["kind"] != kindStatus {
		t.Errorf("digest off: %v", out)
	}
	for _, s := range []string{"OUTCOME: problem", "BLOCK on review", "now Blocked by CI", "blocked"} {
		if !statusUrgent(s) {
			t.Errorf("%q not urgent", s)
		}
	}
	if statusUrgent("OUTCOME: done") || statusUrgent("block") {
		t.Error("urgent by mistake")
	}
}

// client flags: one kind at a time; --within only with --needs-reply.
func TestSendKindFlags(t *testing.T) {
	var r sendReq
	if err := sendKindFlags(&r, true, true, false, 30*time.Minute, false); err == nil {
		t.Error("two kinds accepted")
	}
	if err := sendKindFlags(&sendReq{}, false, false, false, time.Hour, true); err == nil {
		t.Error("--within alone accepted")
	}
	if err := sendKindFlags(&sendReq{To: "all"}, false, false, true, time.Hour, true); err == nil {
		t.Error("--needs-reply to all accepted")
	}
	r = sendReq{To: "dave"}
	if err := sendKindFlags(&r, false, false, true, 30*time.Minute, false); err != nil || r.NeedsReplySec != 1800 || r.Kind != "" {
		t.Errorf("%v %+v", err, r)
	}
	r = sendReq{To: "dave"}
	if err := sendKindFlags(&r, false, true, false, 30*time.Minute, false); err != nil || r.Kind != kindStatus {
		t.Errorf("%v %+v", err, r)
	}
	if s := sendResult(sendReq{}, Message{ID: 7, To: "dave", Channel: "team"}); s != "sent #7 to dave in 🔒team\n" {
		t.Errorf("plain result %q", s)
	}
	if s := sendResult(sendReq{Kind: kindStatus}, Message{To: "dave", Channel: "team", Kind: kindStatus, Queued: "2026-11-13T21:10:00+09:00"}); !strings.HasPrefix(s, "queued status line for dave in 🔒team") || !strings.Contains(s, "21:10") {
		t.Errorf("queued result %q", s)
	}
}
