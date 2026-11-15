package main

// Tests for vote (vote.go). Temp dirs and the local test server only.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeClock makes sv.now settable from the test (atomic: handlers read it concurrently).
func fakeClock(sv *server, start time.Time) *atomic.Int64 {
	var c atomic.Int64
	c.Store(start.UnixNano())
	sv.now = func() time.Time { return time.Unix(0, c.Load()) }
	return &c
}

func advance(c *atomic.Int64, d time.Duration) time.Time {
	return time.Unix(0, c.Add(int64(d)))
}

func voteNewHTTP(t *testing.T, f *fixture, who string, body voteNewReq) (int, map[string]any) {
	t.Helper()
	return f.req(t, "POST", "/votes", f.tokens[who], body)
}

func voteOf(t *testing.T, out map[string]any) map[string]any {
	t.Helper()
	v, ok := out["vote"].(map[string]any)
	if !ok {
		t.Fatalf("no vote in %v", out)
	}
	return v
}

func castHTTP(t *testing.T, f *fixture, who string, id any, choice string) (int, map[string]any) {
	t.Helper()
	return f.req(t, "POST", fmt.Sprintf("/votes/%v", id), f.tokens[who], voteCastReq{Choice: choice})
}

func mustCast(t *testing.T, f *fixture, id any, choice string, who ...string) {
	t.Helper()
	for _, w := range who {
		if code, out := castHTTP(t, f, w, id, choice); code != 200 {
			t.Fatalf("%s votes %s on %v: %d %v", w, choice, id, code, out)
		}
	}
}

// results are the a2-system result messages in the store.
func results(f *fixture) []Message {
	var out []Message
	for _, m := range f.sv.store.after(0) {
		if m.From == systemSender && strings.Contains(m.Text, " result] ") {
			out = append(out, m)
		}
	}
	return out
}

func openVote(t *testing.T, f *fixture, who string, req voteNewReq) (int64, int64) {
	t.Helper()
	code, out := voteNewHTTP(t, f, who, req)
	if code != 200 {
		t.Fatalf("vote new: %d %v", code, out)
	}
	v := voteOf(t, out)
	return int64(v["id"].(float64)), int64(out["message"].(map[string]any)["id"].(float64))
}

func TestVoteYesNoPassAndFail(t *testing.T) {
	f := setup(t)
	clk := fakeClock(f.sv, time.Now())
	id, mid := openVoteOld(t, f, "alice", voteNewReq{Channel: "team", Question: "Build the vote feature?"})
	if id != 1 {
		t.Fatalf("first vote id %d", id)
	}
	// the vote is a normal message to all on the channel, seen in inbox
	_, in := f.req(t, "GET", "/inbox?since=0", f.tokens["dave"], nil)
	ms := msgs(t, in)
	if len(ms) != 1 || ms[0]["from"] != "alice" || ms[0]["to"] != "all" || ms[0]["channel"] != "team" ||
		!strings.HasPrefix(ms[0]["text"].(string), "[vote #1] Build the vote feature?\nChoices: yes / no\n") {
		t.Fatalf("vote message %v", ms)
	}
	if !strings.HasPrefix(formatLine(Message{ID: mid, Channel: "team", TS: "x", From: "alice", To: "all", Text: ms[0]["text"].(string)}), "[a2] #") {
		t.Fatal("line format")
	}
	mustCast(t, f, id, "yes", "alice", "dave") // the proposer may vote
	mustCast(t, f, id, "no", "carol")
	mustCast(t, f, id, "yes", "bob") // 3rd yes: early close
	id2, _ := openVoteOld(t, f, "dave", voteNewReq{Channel: "team", Question: "Only two yes"})
	mustCast(t, f, id2, "yes", "dave", "bob")
	id3, _ := openVoteOld(t, f, "dave", voteNewReq{Channel: "team", Question: "yes not above no", Hours: 2})
	mustCast(t, f, id3, "yes", "dave", "bob")
	mustCast(t, f, id3, "no", "carol", "alice")
	// voting again replaces: bob switches from yes to no
	mustCast(t, f, id3, "no", "bob")
	_, out := f.req(t, "GET", fmt.Sprint("/votes/", id3), f.tokens["dave"], nil)
	if v := voteOf(t, out); v["counts"].(map[string]any)["yes"] != 1.0 || v["counts"].(map[string]any)["no"] != 3.0 || v["voters"] != 4.0 || v["open"] != true {
		t.Fatalf("vote 3 %v", v)
	}
	f.sv.voteTick(advance(clk, time.Hour))
	if len(results(f)) != 1 { // only #1, closed early
		t.Fatal("result before the time is up")
	}
	f.sv.voteTick(advance(clk, 25*time.Hour))
	rs := results(f)
	want := []string{"[vote #1 result] passed yes 3 / no 1 (4 voters) · closed early: pass rule met", "[vote #2 result] failed yes 2 / no 0 (2 voters)", "[vote #3 result] failed yes 1 / no 3 (4 voters)"}
	if len(rs) != 3 {
		t.Fatalf("results %v", rs)
	}
	for i, r := range rs {
		if r.Text != want[i] || r.To != allTarget || r.Channel != "team" {
			t.Errorf("result %d: %+v want %q", i, r, want[i])
		}
	}
	if rs[0].ReplyTo != mid {
		t.Errorf("result reply_to %d want %d", rs[0].ReplyTo, mid)
	}
	_, out = f.req(t, "GET", "/votes/1", f.tokens["bob"], nil)
	if v := voteOf(t, out); v["open"] != false || v["passed"] != true || v["result"] != "passed" || v["result_posted"] != true {
		t.Errorf("closed vote %v", v)
	}
	// the result reaches members through the normal inbox
	_, in = f.req(t, "GET", "/inbox?since=0&from=a2-system", f.tokens["bob"], nil)
	if len(msgs(t, in)) != 3 {
		t.Errorf("results in inbox %v", in)
	}
}

func TestVoteMinYesFlag(t *testing.T) {
	f := setup(t)
	clk := fakeClock(f.sv, time.Now())
	id1, _ := openVoteOld(t, f, "alice", voteNewReq{Channel: "team", Question: "min 3 (opened before the change)"})
	f.sv.voteMinYes = 1
	id2, _ := openVoteOld(t, f, "alice", voteNewReq{Channel: "team", Question: "min 1"})
	mustCast(t, f, id1, "yes", "dave")
	mustCast(t, f, id2, "yes", "dave") // min 1: closes early at once
	f.sv.voteTick(advance(clk, 24*time.Hour))
	rs := results(f)
	if len(rs) != 2 || !strings.Contains(rs[1].Text, "failed yes 1 / no 0") || !strings.Contains(rs[0].Text, "#2 result] passed yes 1 / no 0") {
		t.Fatalf("results %v", rs)
	}
	// the server flag is checked
	for _, bad := range []string{"0", "-1"} {
		if code := run([]string{"serve", "--vote-min-yes", bad}); code != 2 {
			t.Errorf("--vote-min-yes %s exit %d", bad, code)
		}
	}
}

func TestVoteOptionsWinner(t *testing.T) {
	f := setup(t)
	clk := fakeClock(f.sv, time.Now())
	id, _ := openVote(t, f, "alice", voteNewReq{Channel: "team", Question: "Which first?", Options: []string{"inbox search", "Read marks", "themes"}})
	_, out := f.req(t, "GET", fmt.Sprint("/votes/", id), f.tokens["dave"], nil)
	if v := voteOf(t, out); fmt.Sprint(v["choices"]) != "[opt1 opt2 opt3]" {
		t.Fatalf("choices %v", v["choices"])
	}
	mustCast(t, f, id, "opt2", "alice", "dave")
	mustCast(t, f, id, "read MARKS", "bob") // the option text, any case
	mustCast(t, f, id, "1", "carol")
	if code, _ := castHTTP(t, f, "dave", id, "yes"); code != 400 {
		t.Errorf("yes on an options vote: %d", code)
	}
	if code, _ := castHTTP(t, f, "dave", id, "opt4"); code != 400 {
		t.Errorf("opt4: %d", code)
	}
	tie, _ := openVote(t, f, "alice", voteNewReq{Channel: "team", Question: "tie", Options: []string{"a", "b"}})
	mustCast(t, f, tie, "opt1", "alice", "dave", "bob")
	mustCast(t, f, tie, "opt2", "carol")
	if code, _ := castHTTP(t, f, "frank", tie, "opt2"); code != 404 { // general only
		t.Errorf("frank on a work vote: %d", code)
	}
	f.sv.voteTick(advance(clk, 24*time.Hour))
	rs := results(f)
	if len(rs) != 2 || rs[0].Text != "[vote #1 result] passed opt2 (Read marks) 3 votes — opt1 1 / opt2 3 / opt3 0 (4 voters)" {
		t.Fatalf("results %v", rs)
	}
	if rs[1].Text != "[vote #2 result] passed opt1 (a) 3 votes — opt1 3 / opt2 1 (4 voters)" {
		t.Errorf("tie vote (general-only refused, so no tie): %q", rs[1].Text)
	}
	// a real tie and a top option under min-yes do not pass
	v := &voteRec{ID: 9, Options: []string{"a", "b"}, MinYes: 3, ballots: map[string]string{"w": "opt1", "x": "opt1", "y": "opt2", "z": "opt2", "q": "opt1", "r": "opt2"}}
	if _, passed, winner := v.tally(); passed || winner != "" || !strings.Contains(v.resultText(), "failed tie") {
		t.Errorf("tie: %v %q %q", passed, winner, v.resultText())
	}
	v.ballots = map[string]string{"w": "opt1", "x": "opt1", "y": "opt2"}
	if _, passed, winner := v.tally(); passed || winner != "opt1" {
		t.Errorf("under min-yes: %v %q", passed, winner)
	}
	// bad option lists
	for _, opts := range [][]string{{"only one"}, {"a", "A"}, {"a", " "}, make([]string, 11)} {
		if code, _ := voteNewHTTP(t, f, "alice", voteNewReq{Channel: "team", Question: "q", Options: opts}); code != 400 {
			t.Errorf("options %q: %d", opts, code)
		}
	}
	for _, h := range []int{-1, 169} {
		if code, _ := voteNewHTTP(t, f, "alice", voteNewReq{Channel: "team", Question: "q", Hours: h}); code != 400 {
			t.Errorf("hours %d: %d", h, code)
		}
	}
	if code, _ := voteNewHTTP(t, f, "alice", voteNewReq{Channel: "team", Question: "  "}); code != 400 {
		t.Errorf("empty question: %d", code)
	}
}

func TestVoteRevoteAndClosedRefuses(t *testing.T) {
	f := setup(t)
	clk := fakeClock(f.sv, time.Now())
	id, _ := openVote(t, f, "alice", voteNewReq{Channel: "team", Question: "q", Hours: 1})
	mustCast(t, f, id, "yes", "dave")
	code, out := castHTTP(t, f, "dave", id, "NO")
	v := voteOf(t, out)
	if code != 200 || out["previous"] != "yes" || v["voters"] != 1.0 || v["counts"].(map[string]any)["no"] != 1.0 || v["counts"].(map[string]any)["yes"] != 0.0 {
		t.Fatalf("re-vote %d %v", code, out)
	}
	if code, _ := castHTTP(t, f, "dave", id, "maybe"); code != 400 {
		t.Errorf("bad choice %d", code)
	}
	// time up: refused before the tick posts the result, and after
	advance(clk, time.Hour)
	if code, out := castHTTP(t, f, "bob", id, "yes"); code != 409 || !strings.Contains(fmt.Sprint(out["error"]), "closed") {
		t.Errorf("closed (before tick) %d %v", code, out)
	}
	f.sv.voteTick(f.sv.now())
	if code, _ := castHTTP(t, f, "dave", id, "yes"); code != 409 {
		t.Errorf("closed (after tick) %d", code)
	}
	if rs := results(f); len(rs) != 1 || rs[0].Text != "[vote #1 result] failed yes 0 / no 1 (1 voters)" {
		t.Errorf("results %v", rs)
	}
	if code, _ := castHTTP(t, f, "dave", 99, "yes"); code != 404 {
		t.Errorf("unknown vote %d", code)
	}
}

func TestVoteGeneralOnlyCannotSeeTeam(t *testing.T) {
	v := setupView(t)
	f := v.fixture
	id, mid := openVote(t, f, "alice", voteNewReq{Channel: "team", Question: "work only"})
	gid, gmid := openVote(t, f, "alice", voteNewReq{Channel: "general", Question: "Pizza or ramen on Friday?", Options: []string{"pizza", "ramen"}})
	for _, g := range []string{"erin", "discord-bridge", "frank"} {
		if code, out := f.req(t, "GET", fmt.Sprint("/votes/", id), f.tokens[g], nil); code != 404 || out["vote"] != nil {
			t.Errorf("%s sees work vote: %d %v", g, code, out)
		}
		if code, _ := castHTTP(t, f, g, id, "yes"); code != 404 {
			t.Errorf("%s casts on work vote: %d", g, code)
		}
		_, out := f.req(t, "GET", "/votes", f.tokens[g], nil)
		if b, _ := json.Marshal(out); strings.Contains(string(b), "work only") || len(out["votes"].([]any)) != 1 {
			t.Errorf("%s list %s", g, b)
		}
		if code, _ := f.req(t, "GET", "/votes?channel=team", f.tokens[g], nil); code != 403 {
			t.Errorf("%s list work %d", g, code)
		}
		if code, _ := voteNewHTTP(t, f, g, voteNewReq{Channel: "team", Question: "x"}); code != 403 {
			t.Errorf("%s opens a work vote %d", g, code)
		}
		res, isErr := toolCall(t, f, g, "vote_show", map[string]any{"id": id})
		if !isErr || !strings.Contains(fmt.Sprint(res["error"]), "unknown vote #1") {
			t.Errorf("%s MCP vote_show %v", g, res)
		}
		if _, isErr := toolCall(t, f, g, "vote_cast", map[string]any{"id": id, "choice": "yes"}); !isErr {
			t.Errorf("%s MCP vote_cast on work vote", g)
		}
	}
	mustCast(t, f, gid, "pizza", "erin", "frank")
	// general lint: work content in the question or an option is refused, no id used
	if code, _ := voteNewHTTP(t, f, "erin", voteNewReq{Channel: "general", Question: "Which PR first?"}); code != 422 {
		t.Errorf("work question on general %d", code)
	}
	if code, _ := voteNewHTTP(t, f, "erin", voteNewReq{Channel: "general", Question: "x", Options: []string{"jenkins", "b"}}); code != 422 {
		t.Errorf("work option on general %d", code)
	}
	mustCast(t, f, id, "yes", "dave")
	// the inbox of a general-only member never holds the work vote message
	_, in := f.req(t, "GET", "/inbox?since=0", f.tokens["erin"], nil)
	for _, m := range msgs(t, in) {
		if m["channel"] != "general" {
			t.Errorf("erin got %v", m)
		}
	}
	// the viewer UI sees both votes on their messages
	_, cookie := v.login(t, v.viewerTok, "")
	resp, body := v.do(t, "GET", "/view/api/messages?since=0", cookie, nil, "")
	var d struct {
		Votes map[string]voteView `json:"votes"`
	}
	if resp.StatusCode != 200 || json.Unmarshal([]byte(body), &d) != nil {
		t.Fatalf("view %d %s", resp.StatusCode, body)
	}
	cv, gv := d.Votes[fmt.Sprint(mid)], d.Votes[fmt.Sprint(gmid)]
	if cv.ID != id || cv.Counts["yes"] != 1 || gv.ID != gid || gv.Counts["opt1"] != 2 || len(gv.Ballots) != 2 {
		t.Errorf("view votes %+v", d.Votes)
	}
	// an agent token does not get the view's votes
	if code, _ := f.req(t, "GET", "/view/api/messages?since=0", f.tokens["alice"], nil); code == 200 {
		t.Error("agent read the view API")
	}
}

func TestVoteViewWakeOnCast(t *testing.T) {
	f := setup(t)
	id, _ := openVote(t, f, "alice", voteNewReq{Channel: "team", Question: "q"})
	w := f.sv.deliv.wake()
	mustCast(t, f, id, "yes", "dave")
	select {
	case <-w:
	case <-time.After(2 * time.Second):
		t.Fatal("a cast did not wake the viewer UI")
	}
	if !strings.Contains(viewJS, "takeVotes(d)") || !strings.Contains(viewJS, "voteNode(S.votes[m.id])") {
		t.Error("view script does not show vote counts")
	}
}

func TestVoteResultOnceAndSurvivesRestart(t *testing.T) {
	f := setup(t)
	start := time.Now()
	clk := fakeClock(f.sv, start)
	id1, _ := openVote(t, f, "alice", voteNewReq{Channel: "team", Question: "first", Hours: 1})
	id2, _ := openVote(t, f, "alice", voteNewReq{Channel: "team", Question: "second", Hours: 3})
	mustCast(t, f, id1, "yes", "alice", "dave", "bob")
	mustCast(t, f, id2, "no", "dave")
	now := advance(clk, 2*time.Hour)
	f.sv.voteTick(now)
	f.sv.voteTick(now)
	if rs := results(f); len(rs) != 1 || rs[0].Text != "[vote #1 result] passed yes 3 / no 0 (3 voters) · closed early: pass rule met" {
		t.Fatalf("results %v", rs)
	}
	st, err := os.Stat(filepath.Join(stateDir(), votesFile))
	if err != nil || wrongMode(st, 0o600) {
		t.Fatalf("votes file %v %v", st, err)
	}
	raw, _ := os.ReadFile(filepath.Join(stateDir(), votesFile))
	if strings.Contains(string(raw), "token") {
		t.Error("votes file mentions a token")
	}
	// restart: the closed mark and the ballots stay; vote 2 closes while the server is down
	restartFixture(t, f)
	clk = fakeClock(f.sv, start.Add(5*time.Hour))
	f.sv.voteTick(f.sv.now())
	f.sv.voteTick(f.sv.now())
	rs := results(f)
	if len(rs) != 2 || rs[1].Text != "[vote #2 result] failed yes 0 / no 1 (1 voters)" {
		t.Fatalf("after restart %v", rs)
	}
	restartFixture(t, f)
	fakeClock(f.sv, start.Add(6*time.Hour))
	f.sv.voteTick(f.sv.now())
	if len(results(f)) != 2 {
		t.Fatal("result posted again after a second restart")
	}
	// ids keep growing after a restart
	if id3, _ := openVote(t, f, "dave", voteNewReq{Channel: "team", Question: "third"}); id3 != 3 {
		t.Errorf("id after restart %d", id3)
	}
	_, out := f.req(t, "GET", "/votes/1", f.tokens["dave"], nil)
	if v := voteOf(t, out); v["voters"] != 3.0 || v["result_posted"] != true {
		t.Errorf("vote 1 after restart %v", v)
	}
}

func TestVoteSystemAndViewerRefused(t *testing.T) {
	f := setup(t)
	id, _ := openVote(t, f, "alice", voteNewReq{Channel: "team", Question: "q"})
	for _, who := range []string{systemSender, viewerName} {
		if _, _, err := f.sv.voteCast(who, id, "yes"); statusOf(err) != 403 {
			t.Errorf("%s cast: %v", who, err)
		}
		if _, _, err := f.sv.voteNew(who, "team", "q", nil, 0); statusOf(err) != 403 {
			t.Errorf("%s new: %v", who, err)
		}
	}
	if code := run([]string{"token", "new", systemSender}); code != 2 {
		t.Errorf("token new a2-system exit %d", code)
	}
	// no token, no route: unauthenticated requests are refused
	if code, _ := f.req(t, "POST", "/votes/1", "", voteCastReq{Choice: "yes"}); code != 401 {
		t.Errorf("no token %d", code)
	}
	_, out := f.req(t, "GET", "/votes/1", f.tokens["alice"], nil)
	if v := voteOf(t, out); v["voters"] != 0.0 {
		t.Errorf("vote %v", v)
	}
}

func TestVoteMCP(t *testing.T) {
	f := setup(t)
	res, isErr := toolCall(t, f, "dave", "vote_new", map[string]any{"channel": "team", "question": "mcp vote", "options": []string{"x", "y"}, "hours": 2})
	if isErr {
		t.Fatalf("vote_new %v", res)
	}
	id := res["vote"].(map[string]any)["id"]
	res, isErr = toolCall(t, f, "alice", "vote_cast", map[string]any{"id": id, "choice": "opt2"})
	if isErr || res["previous"] != "" {
		t.Fatalf("vote_cast %v", res)
	}
	res, isErr = toolCall(t, f, "alice", "vote_show", map[string]any{"id": id})
	if isErr || res["vote"].(map[string]any)["counts"].(map[string]any)["opt2"] != 1.0 {
		t.Fatalf("vote_show %v", res)
	}
	if _, isErr := toolCall(t, f, "alice", "vote_new", map[string]any{"question": "no channel"}); !isErr {
		t.Error("vote_new without channel")
	}
	_, out := mcpCall(t, f, "erin", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	b, _ := json.Marshal(out)
	for _, n := range []string{"vote_new", "vote_cast", "vote_show"} {
		if !strings.Contains(string(b), `"name":"`+n+`"`) {
			t.Errorf("tools/list misses %s", n)
		}
	}
}

func TestVoteCLI(t *testing.T) {
	f := setup(t)
	t.Setenv("AGENTSAGORA_URL", f.srv.URL)
	t.Setenv("AGENTSAGORA_TOKEN_FILE", filepath.Join(configDir(), "token-alice"))
	var code int
	got := captureStdout(t, func() { code = run([]string{"vote", "new", "--channel", "team", "--hours", "2", "Ship it?"}) })
	if code != 0 || !strings.HasPrefix(got, "vote #1 opened in 🔒team (yes/no, closes ") {
		t.Fatalf("new: %d %q", code, got)
	}
	got = captureStdout(t, func() { code = run([]string{"vote", "new", "Which?", "a", "b"}) })
	if code != 0 || !strings.HasPrefix(got, "vote #2 opened in 🔒team (opt1/opt2, ") {
		t.Fatalf("new options: %d %q", code, got)
	}
	got = captureStdout(t, func() { code = run([]string{"vote", "1", "yes"}) })
	if code != 0 || !strings.HasPrefix(got, "voted on #1 — yes 1 / no 0 (1 voters, closes ") {
		t.Fatalf("cast: %d %q", code, got)
	}
	got = captureStdout(t, func() { code = run([]string{"vote", "#1", "no"}) })
	if code != 0 || !strings.HasPrefix(got, "voted on #1 (replaced yes) — yes 0 / no 1") {
		t.Fatalf("re-cast: %d %q", code, got)
	}
	got = captureStdout(t, func() { code = run([]string{"vote", "show", "1"}) })
	lines := strings.Split(strings.TrimSpace(got), "\n")
	if code != 0 || len(lines) != 5 || !strings.HasPrefix(lines[0], "vote #1 [🔒team] by alice — open (closes ") ||
		lines[1] != "Question: Ship it?" || !strings.HasPrefix(lines[2], "yes 0 / no 1 (1 voters) · rule: passes when yes >= 3") ||
		lines[3] != "  alice: no" || lines[4] != "So far: failed" {
		t.Fatalf("show: %d %q", code, got)
	}
	got = captureStdout(t, func() { code = run([]string{"vote", "list", "--open"}) })
	if lines := strings.Split(strings.TrimSpace(got), "\n"); code != 0 || len(lines) != 2 || !strings.HasPrefix(lines[0], "#2 [🔒team] open until ") {
		t.Fatalf("list: %d %q", code, got)
	}
	for _, bad := range [][]string{{"vote"}, {"vote", "new"}, {"vote", "new", "q", "only-one"}, {"vote", "new", "--hours", "0", "q"}, {"vote", "x", "yes"}, {"vote", "show"}} {
		if code := run(bad); code != 2 {
			t.Errorf("%q exit %d", bad, code)
		}
	}
	t.Setenv("AGENTSAGORA_TOKEN_FILE", filepath.Join(configDir(), "token-erin"))
	if code := run([]string{"vote", "show", "1"}); code != 1 {
		t.Errorf("erin show work vote exit %d", code)
	}
}
