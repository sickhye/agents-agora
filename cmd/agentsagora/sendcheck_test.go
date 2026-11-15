package main

// Tests for send-check (sendcheck.go): away notice at send time, undelivered notice to the
// sender, inbox filters. Temp dirs and the local test server only.

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// setLast sets the last contact of members (base names); a zero time forgets the member.
func setLast(sv *server, last map[string]time.Time) {
	sv.live.mu.Lock()
	defer sv.live.mu.Unlock()
	sv.live.m = map[string]*liveMember{}
	for m, t := range last {
		if !t.IsZero() {
			sv.live.m[m] = &liveMember{Last: t}
		}
	}
}

func awayOf(out map[string]any) string { s, _ := out["away"].(string); return s }

func TestAwayNoticeAtSend(t *testing.T) {
	f := setup(t)
	now := time.Now()
	old := now.Add(-30 * time.Minute)
	setLast(f.sv, map[string]time.Time{"dave": now, "bob": now, "carol": now})
	code, out := send(t, f, "alice", "team", "dave", "hello")
	if code != 200 || awayOf(out) != "" {
		t.Fatalf("alive recipient: %d %v", code, out)
	}
	setLast(f.sv, map[string]time.Time{"dave": old, "bob": now, "carol": now})
	code, out = send(t, f, "alice", "team", "dave", "are you there")
	want := "[a2] notice: dave no response — last seen " + lastSeenText(old, time.Now()) + " (the message is kept and will be delivered when they connect)"
	if code != 200 || awayOf(out) != want {
		t.Fatalf("not alive: %d away %q want %q", code, awayOf(out), want)
	}
	// still stored and delivered
	_, in := f.req(t, "GET", "/inbox?since=0", f.tokens["dave"], nil)
	if ms := msgs(t, in); len(ms) != 2 || ms[1]["text"] != "are you there" || ms[1]["away"] != nil {
		t.Fatalf("dave inbox %v", ms)
	}
	// never seen
	setLast(f.sv, map[string]time.Time{"dave": now, "carol": now})
	_, out = send(t, f, "alice", "team", "bob", "x")
	if a := awayOf(out); a != "[a2] notice: bob no response — no last-seen record (the message is kept and will be delivered when they connect)" {
		t.Fatalf("never seen: %q", a)
	}
	// to=all work: only work readers; general-only members never appear
	setLast(f.sv, map[string]time.Time{"bob": now, "carol": now, "dave": old})
	_, out = send(t, f, "alice", "team", "all", "standup")
	a := awayOf(out)
	if !strings.HasPrefix(a, "[a2] notice: no response from 1 — dave (last seen ") || !strings.HasSuffix(a, awayTail) {
		t.Fatalf("to=all work: %q", a)
	}
	for _, g := range []string{"discord-bridge", "erin", "frank", "alice"} {
		if strings.Contains(a, g) {
			t.Errorf("to=all work names %s: %q", g, a)
		}
	}
	setLast(f.sv, map[string]time.Time{"bob": now, "carol": now, "dave": now})
	if _, out = send(t, f, "alice", "team", "all", "all here"); awayOf(out) != "" {
		t.Errorf("all alive: %q", awayOf(out))
	}
	// to=all general: general-only members are recipients there
	setLast(f.sv, map[string]time.Time{"bob": now, "carol": now, "dave": now, "erin": now, "frank": now})
	_, out = send(t, f, "alice", "general", "all", "lunch?")
	if a := awayOf(out); !strings.Contains(a, "no response from 1 — discord-bridge (no last-seen record)") {
		t.Errorf("to=all general: %q", a)
	}
	// MCP send: same text
	setLast(f.sv, map[string]time.Time{"dave": old})
	res, isErr := toolCall(t, f, "alice", "send", map[string]any{"channel": "team", "to": "dave", "text": "mcp"})
	if isErr || res["away"] != want {
		t.Errorf("mcp away %v want %q", res, want)
	}
	// CLI: one extra line after "sent #N"
	t.Setenv("AGENTSAGORA_URL", f.srv.URL)
	t.Setenv("AGENTSAGORA_TOKEN_FILE", filepath.Join(configDir(), "token-alice"))
	setLast(f.sv, map[string]time.Time{"dave": old})
	var rc int
	got := captureStdout(t, func() { rc = run([]string{"send", "--channel", "team", "--to", "dave", "cli"}) })
	lines := strings.Split(strings.TrimSpace(got), "\n")
	if rc != 0 || len(lines) != 2 || !strings.HasPrefix(lines[0], "sent #") || lines[1] != want {
		t.Fatalf("cli exit %d out %q", rc, got)
	}
	setLast(f.sv, map[string]time.Time{"dave": now})
	got = captureStdout(t, func() { rc = run([]string{"send", "--channel", "team", "--to", "dave", "cli2"}) })
	if rc != 0 || strings.Count(strings.TrimSpace(got), "\n") != 0 {
		t.Fatalf("cli alive: exit %d out %q", rc, got)
	}
}

func TestLastSeenText(t *testing.T) {
	now := time.Date(2026, 11, 12, 20, 0, 0, 0, jst)
	if s := lastSeenText(now.Add(-time.Hour), now); s != "19:00" {
		t.Errorf("today %q", s)
	}
	if s := lastSeenText(now.Add(-24*time.Hour), now); s != "11-11 20:00" {
		t.Errorf("earlier day %q", s)
	}
}

// systemNotices returns the a2-system messages to who, read from the store.
func systemNotices(f *fixture, who string) []Message {
	var out []Message
	for _, m := range f.sv.store.after(0) {
		if m.From == systemSender && m.To == who {
			out = append(out, m)
		}
	}
	return out
}

func TestUndeliveredNoticeOnce(t *testing.T) {
	f := setup(t)
	send(t, f, "alice", "team", "dave", "before the feature") // #1: never checked
	if err := f.sv.enableUndelivered(10 * time.Minute); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(stateDir(), noticesFile)
	if st, err := os.Stat(p); err != nil || wrongMode(st, 0o600) {
		t.Fatalf("notices file %v %v", st, err)
	}
	_, o2 := send(t, f, "alice", "team", "dave", "dm") // #2: delivered below
	_, o3 := send(t, f, "alice", "team", "all", "all")
	_, o4 := send(t, f, "dave", "team", "alice", "from dave")
	id2, id3 := msgID(t, o2), msgID(t, o3)
	_ = o4
	fetchInbox(t, f, "dave")
	t0 := time.Now()
	setLast(f.sv, map[string]time.Time{"carol": t0})
	f.sv.undeliveredTick(t0.Add(5 * time.Minute))
	if n := len(systemNotices(f, "alice")); n != 0 {
		t.Fatalf("notice before 10 min: %d", n)
	}
	f.sv.undeliveredTick(t0.Add(11 * time.Minute))
	ns := systemNotices(f, "alice")
	want := fmt.Sprintf("#%d undelivered 2/3 (10 min elapsed): bob (long absent), carol", id3)
	if len(ns) != 1 || ns[0].Text != want || ns[0].Channel != "team" {
		t.Fatalf("notices %+v, want %q", ns, want)
	}
	_ = id2
	// dave's message to alice (#4) is pending for alice, so dave gets one too
	if wn := systemNotices(f, "dave"); len(wn) != 1 || !strings.HasSuffix(wn[0].Text, "undelivered 1/1 (10 min elapsed): alice (long absent)") {
		t.Fatalf("dave notices %+v", wn)
	}
	// delivered like a normal DM, same line format
	_, in := f.req(t, "GET", "/inbox?since=0&from=a2-system", f.tokens["alice"], nil)
	ms := msgs(t, in)
	if len(ms) != 1 || ms[0]["from"] != systemSender || ms[0]["to"] != "alice" {
		t.Fatalf("alice inbox %v", ms)
	}
	line := formatLine(ns[0])
	if !strings.HasPrefix(line, fmt.Sprintf("[a2] #%d [🔒team] ", ns[0].ID)) || !strings.HasSuffix(line, " a2-system -> alice: "+want) {
		t.Errorf("line %q", line)
	}
	// once: later ticks (the notices themselves are pending now) add nothing
	for i := 2; i < 6; i++ {
		f.sv.undeliveredTick(t0.Add(time.Duration(i) * 11 * time.Minute))
	}
	if n := len(systemNotices(f, "alice")) + len(systemNotices(f, "dave")); n != 2 {
		t.Fatalf("notices after more ticks: %d", n)
	}
	// restart keeps the mark
	restartFixture(t, f)
	if err := f.sv.enableUndelivered(10 * time.Minute); err != nil {
		t.Fatal(err)
	}
	f.sv.undeliveredTick(t0.Add(2 * time.Hour))
	if n := len(systemNotices(f, "alice")) + len(systemNotices(f, "dave")); n != 2 {
		t.Fatalf("notices after restart: %d", n)
	}
	// a new message after the restart still gets its notice
	_, o := send(t, f, "alice", "team", "bob", "new")
	f.sv.undeliveredTick(time.Now().Add(11 * time.Minute))
	ns = systemNotices(f, "alice")
	if len(ns) != 2 || !strings.HasPrefix(ns[1].Text, fmt.Sprintf("#%d undelivered 1/1", msgID(t, o))) {
		t.Fatalf("after restart %+v", ns)
	}
	b, _ := os.ReadFile(p)
	if strings.Contains(string(b), "dm") || strings.Contains(string(b), "undelivered") {
		t.Errorf("notices file holds text: %s", b)
	}
	// the first message (before the feature) never got one
	for _, n := range append(systemNotices(f, "alice"), systemNotices(f, "dave")...) {
		if strings.HasPrefix(n.Text, "#1 ") {
			t.Errorf("backfilled %q", n.Text)
		}
	}
}

func TestUndeliveredOff(t *testing.T) {
	f := setup(t)
	if err := f.sv.enableUndelivered(0); err != nil {
		t.Fatal(err)
	}
	send(t, f, "alice", "team", "dave", "dm")
	f.sv.undeliveredTick(time.Now().Add(time.Hour))
	if n := len(systemNotices(f, "alice")); n != 0 {
		t.Fatalf("off: %d notices", n)
	}
	if _, err := os.Stat(filepath.Join(stateDir(), noticesFile)); !os.IsNotExist(err) {
		t.Errorf("off: notices file made (%v)", err)
	}
	if code := run([]string{"serve", "--undelivered-after", "-1"}); code != 2 {
		t.Errorf("negative minutes exit %d", code)
	}
}

func TestSystemSenderReserved(t *testing.T) {
	setup(t)
	if code := run([]string{"token", "new", systemSender}); code != 2 {
		t.Errorf("token new a2-system exit %d", code)
	}
	if code := run([]string{"token", "rename", "dave", systemSender}); code == 0 {
		t.Errorf("rename to a2-system allowed")
	}
	if err := checkMemberName(systemSender); err == nil {
		t.Errorf("checkMemberName accepts a2-system")
	}
	if _, err := newServer([]string{"alice", systemSender}, nil, denyFile); err == nil {
		t.Errorf("newServer accepts a2-system")
	}
	if n := currentNames().current("dave"); n != "dave" {
		t.Errorf("dave renamed to %s", n)
	}
}

func TestParseSinceTime(t *testing.T) {
	now := time.Date(2026, 11, 12, 1, 0, 0, 0, time.UTC) // 10:00 JST
	got, err := parseSinceTime("09:30", now)
	if err != nil || !got.Equal(time.Date(2026, 11, 12, 9, 30, 0, 0, jst)) {
		t.Errorf("HH:MM %v %v", got, err)
	}
	if got, err := parseSinceTime("2026-11-12T09:30:00+09:00", now); err != nil || got.In(jst).Hour() != 9 {
		t.Errorf("RFC3339 %v %v", got, err)
	}
	for _, bad := range []string{"9:30", "25:00", "yesterday", "2026-11-12"} {
		if _, err := parseSinceTime(bad, now); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

// inboxIDs is the ids dave gets from /inbox with query q (since=0 added).
func inboxIDs(t *testing.T, f *fixture, who, q string) (int, []int64) {
	t.Helper()
	code, out := f.req(t, "GET", "/inbox?since=0&"+q, f.tokens[who], nil)
	var ids []int64
	for _, m := range msgs(t, out) {
		ids = append(ids, int64(m["id"].(float64)))
	}
	if code == 200 && q != "" && out["filtered"] != true {
		t.Errorf("%s: no filtered mark", q)
	}
	return code, ids
}

func TestInboxFilters(t *testing.T) {
	f := setup(t)
	t0 := time.Date(2026, 11, 12, 9, 0, 0, 0, jst)
	at := func(i int) time.Time { return t0.Add(time.Duration(i) * time.Minute) }
	put := func(i int, ch, from, to, text string, re int64) {
		if _, err := f.sv.store.appendReply(ch, from, to, text, "", re, at(i)); err != nil {
			t.Fatal(err)
		}
	}
	put(1, "team", "alice", "all", "root", 0)    // #1
	put(2, "team", "bob", "dave", "dm", 0)       // #2
	put(3, "team", "alice", "all", "re root", 1) // #3
	put(4, "general", "bob", "all", "gen", 0)    // #4
	put(5, "general", "erin", "all", "gen2", 0)  // #5
	put(6, "team", "alice", "bob", "hidden", 1)  // #6: not for dave
	put(7, "team", "carol", "all", "re 3", 3)    // #7: reply_to chain 7 -> 3 -> 1
	put(8, "team", "bob", "alice", "hidden2", 0) // #8: not for dave
	eq := func(name string, got []int64, want ...int64) {
		t.Helper()
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("%s: got %v want %v", name, got, want)
		}
	}
	_, all := inboxIDs(t, f, "dave", "")
	eq("all", all, 1, 2, 3, 4, 5, 7)
	_, ids := inboxIDs(t, f, "dave", "thread=1")
	eq("thread", ids, 1, 3, 7)
	_, ids = inboxIDs(t, f, "dave", "thread=3")
	eq("thread of a reply", ids, 3, 7)
	_, ids = inboxIDs(t, f, "dave", "thread=6") // a message dave cannot see: nothing, no error
	eq("hidden thread", ids)
	_, ids = inboxIDs(t, f, "dave", "from=alice")
	eq("from", ids, 1, 3)
	_, ids = inboxIDs(t, f, "dave", "since_time="+url.QueryEscape(at(4).Format(time.RFC3339)))
	eq("since_time", ids, 4, 5, 7)
	_, ids = inboxIDs(t, f, "dave", "since_time="+url.QueryEscape(at(2).Format(time.RFC3339))+"&from=alice&thread=1")
	eq("combined", ids, 3)
	_, ids = inboxIDs(t, f, "dave", "from=bob&channel=general")
	eq("from+channel", ids, 4)
	// no leak across channels: a general-only member never sees work, even by thread
	_, ids = inboxIDs(t, f, "erin", "thread=1")
	eq("erin thread", ids)
	_, ids = inboxIDs(t, f, "erin", "from=alice")
	eq("erin from", ids)
	_, ids = inboxIDs(t, f, "erin", "from=bob")
	eq("erin from bob", ids, 4)
	if code, _ := inboxIDs(t, f, "erin", "channel=team&thread=1"); code != 403 {
		t.Errorf("erin work: %d", code)
	}
	for _, q := range []string{"from=nobody", "thread=x", "thread=-1", "since_time=noon"} {
		if code, _ := inboxIDs(t, f, "dave", q); code != 400 {
			t.Errorf("%s: %d", q, code)
		}
	}
	// MCP inbox: same filters
	res, isErr := toolCall(t, f, "dave", "inbox", map[string]any{"thread": 1, "from": "carol"})
	if ms, _ := res["messages"].([]any); isErr || len(ms) != 1 || ms[0].(map[string]any)["text"] != "re 3" {
		t.Errorf("mcp %v", res)
	}
	res, _ = toolCall(t, f, "dave", "inbox", map[string]any{"since_time": at(5).Format(time.RFC3339)})
	if ms, _ := res["messages"].([]any); len(ms) != 2 {
		t.Errorf("mcp since_time %v", res)
	}
	if res, isErr := toolCall(t, f, "erin", "inbox", map[string]any{"thread": 1}); isErr || len(res["messages"].([]any)) != 0 {
		t.Errorf("mcp erin %v", res)
	}
	// CLI: same line format, cursor not moved
	t.Setenv("AGENTSAGORA_URL", f.srv.URL)
	t.Setenv("AGENTSAGORA_TOKEN_FILE", filepath.Join(configDir(), "token-dave"))
	var rc int
	got := captureStdout(t, func() { rc = run([]string{"inbox", "--thread", "1", "--from", "alice"}) })
	lines := strings.Split(strings.TrimSpace(got), "\n")
	if rc != 0 || len(lines) != 2 || !strings.HasPrefix(lines[0], "[a2] #1 [🔒team] ") || !strings.HasSuffix(lines[1], "alice -> all (re #1): re root") {
		t.Fatalf("cli exit %d %q", rc, got)
	}
	got = captureStdout(t, func() { rc = run([]string{"inbox", "--since", "4"}) })
	if rc != 0 || !strings.HasPrefix(got, "[a2] #5 ") || strings.Count(got, "\n") != 2 {
		t.Errorf("cli --since id: %d %q", rc, got)
	}
	got = captureStdout(t, func() { rc = run([]string{"inbox", "--since", at(7).Format(time.RFC3339)}) })
	if rc != 0 || !strings.HasPrefix(got, "[a2] #7 ") {
		t.Errorf("cli --since time: %d %q", rc, got)
	}
	got = captureStdout(t, func() { rc = run([]string{"inbox"}) }) // cursor still at 0
	if rc != 0 || strings.Count(got, "\n") != 6 {
		t.Errorf("cursor moved by a filter: %q", got)
	}
	if rc = run([]string{"inbox", "--since", "noon"}); rc != 2 {
		t.Errorf("bad --since exit %d", rc)
	}
}

func TestUndeliveredSkip(t *testing.T) {
	f := setup(t)
	if err := f.sv.enableUndelivered(10 * time.Minute); err != nil {
		t.Fatal(err)
	}
	f.sv.undelivSkip = map[string]bool{"bob": true, "dave": true}
	send(t, f, "alice", "team", "dave", "only skipped")
	_, o2 := send(t, f, "alice", "team", "all", "mixed")
	id2 := msgID(t, o2)
	send(t, f, "dave", "team", "alice", "to alice") // sender dave is not skipped as sender
	t0 := time.Now()
	setLast(f.sv, map[string]time.Time{"carol": t0})
	f.sv.undeliveredTick(t0.Add(11 * time.Minute))
	ns := systemNotices(f, "alice")
	want := fmt.Sprintf("#%d undelivered 1/1 (10 min elapsed): carol", id2)
	_ = want
	if len(ns) != 1 || !strings.HasPrefix(ns[0].Text, fmt.Sprintf("#%d undelivered ", id2)) ||
		strings.Contains(ns[0].Text, "bob") || strings.Contains(ns[0].Text, "dave") || !strings.Contains(ns[0].Text, "carol") {
		t.Fatalf("alice notices %+v", ns)
	}
	if !strings.Contains(ns[0].Text, "undelivered 1/1 ") {
		t.Errorf("count not reduced: %q", ns[0].Text)
	}
	if wn := systemNotices(f, "dave"); len(wn) != 1 {
		t.Fatalf("dave (not a skipped recipient of its own message) notices %+v", wn)
	}
}
