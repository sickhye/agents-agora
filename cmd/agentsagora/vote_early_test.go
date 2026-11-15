package main

import (
	"strings"
	"testing"
	"time"
)

// Early close: the 3rd yes closes a yes/no vote at once.
func TestVoteEarlyCloseOnThirdYes(t *testing.T) {
	f := setup(t)
	clk := fakeClock(f.sv, time.Now())
	id, mid := openVoteOld(t, f, "alice", voteNewReq{Channel: "team", Question: "early?"})
	mustCast(t, f, id, "yes", "alice", "dave")
	if len(results(f)) != 0 {
		t.Fatal("closed before the rule is met")
	}
	mustCast(t, f, id, "yes", "bob")
	rs := results(f)
	if len(rs) != 1 || rs[0].Text != "[vote #1 result] passed yes 3 / no 0 (3 voters) · closed early: pass rule met" ||
		rs[0].To != allTarget || rs[0].ReplyTo != mid {
		t.Fatalf("results %+v", rs)
	}
	_, out := f.req(t, "GET", "/votes/1", f.tokens["dave"], nil)
	if v := voteOf(t, out); v["open"] != false || v["result"] != "passed" || v["result_posted"] != true {
		t.Errorf("vote %v", v)
	}
	// a closed vote refuses further casts, and neither a cast nor the deadline tick posts again
	if code, _ := castHTTP(t, f, "carol", id, "yes"); code != 409 {
		t.Errorf("cast on closed vote: %d", code)
	}
	f.sv.voteTick(advance(clk, 25*time.Hour))
	f.sv.voteTick(f.sv.now())
	if len(results(f)) != 1 {
		t.Fatalf("result posted twice: %v", results(f))
	}
	// the early close survives a restart without a second message
	restartFixture(t, f)
	fakeClock(f.sv, time.Now().Add(48*time.Hour))
	f.sv.voteTick(f.sv.now())
	if len(results(f)) != 1 {
		t.Fatal("result posted again after restart")
	}
}

func TestVoteNoEarlyCloseWhenNotAbove(t *testing.T) {
	f := setup(t)
	clk := fakeClock(f.sv, time.Now())
	id, _ := openVoteOld(t, f, "alice", voteNewReq{Channel: "team", Question: "tie"})
	mustCast(t, f, id, "no", "alice", "dave", "bob")
	mustCast(t, f, id, "yes", "carol")
	f.sv.voteTick(advance(clk, time.Hour))
	if len(results(f)) != 0 {
		t.Fatalf("closed early: %v", results(f))
	}
	// yes 3 / no 3 is not above: still open; the deadline close keeps the plain format
	f2 := setup(t)
	clk2 := fakeClock(f2.sv, time.Now())
	f2.sv.voteMinYes = 3
	id2, _ := openVoteOld(t, f2, "alice", voteNewReq{Channel: "team", Question: "3-3"})
	mustCast(t, f2, id2, "no", "alice", "dave", "bob")
	mustCast(t, f2, id2, "yes", "carol")
	if len(results(f2)) != 0 {
		t.Fatal("early close with yes<min")
	}
	f2.sv.voteTick(advance(clk2, 25*time.Hour))
	rs := results(f2)
	if len(rs) != 1 || strings.Contains(rs[0].Text, "closed early") || !strings.Contains(rs[0].Text, "failed") {
		t.Fatalf("deadline result %v", rs)
	}
}

func TestVoteOptionsKeepDeadline(t *testing.T) {
	f := setup(t)
	clk := fakeClock(f.sv, time.Now())
	id, _ := openVoteOld(t, f, "alice", voteNewReq{Channel: "team", Question: "pick", Options: []string{"a", "b"}})
	mustCast(t, f, id, "opt1", "alice", "dave", "bob")
	if len(results(f)) != 0 {
		t.Fatal("option vote closed early")
	}
	f.sv.voteTick(advance(clk, 25*time.Hour))
	if rs := results(f); len(rs) != 1 || strings.Contains(rs[0].Text, "closed early") {
		t.Fatalf("results %v", rs)
	}
}

// 3 yes already in an open vote (opened before this change): the next tick closes it early.
func TestVoteTickClosesAlreadyPassing(t *testing.T) {
	f := setup(t)
	clk := fakeClock(f.sv, time.Now())
	id, _ := openVoteOld(t, f, "alice", voteNewReq{Channel: "team", Question: "x"})
	f.sv.voteMinYes = 3
	mustCast(t, f, id, "yes", "alice", "dave")
	f.sv.votes.mu.Lock()
	f.sv.votes.votes[id].MinYes = 2 // lowered after the fact, as if the rule was met before the cast hook existed
	f.sv.votes.mu.Unlock()
	f.sv.voteTick(advance(clk, time.Hour))
	if rs := results(f); len(rs) != 1 || !strings.HasSuffix(rs[0].Text, "closed early: pass rule met") {
		t.Fatalf("results %v", rs)
	}
}
