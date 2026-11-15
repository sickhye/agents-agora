package main

import (
	"strings"
	"testing"
	"time"
)

// openVoteOld opens a yes/no vote and turns it into an old-rule record (MinYes, no N), as
// votes recorded before the majority rule look.
func openVoteOld(t *testing.T, f *fixture, who string, req voteNewReq) (int64, int64) {
	t.Helper()
	id, mid := openVote(t, f, who, req)
	setN(f, id, 0)
	return id, mid
}

func setN(f *fixture, id int64, n int) {
	f.sv.votes.mu.Lock()
	f.sv.votes.votes[id].N = n
	f.sv.votes.mu.Unlock()
}

func lastResult(t *testing.T, f *fixture, want int) string {
	t.Helper()
	rs := results(f)
	if len(rs) != want {
		t.Fatalf("results %d want %d: %v", len(rs), want, rs)
	}
	if want == 0 {
		return ""
	}
	return rs[want-1].Text
}

func TestVoteMajorityN4(t *testing.T) {
	f := setup(t)
	fakeClock(f.sv, time.Now())
	// N is the channel's member count, fixed when the vote opens
	id, _ := openVote(t, f, "alice", voteNewReq{Channel: "team", Question: "n?"})
	f.sv.votes.mu.Lock()
	n := f.sv.votes.votes[id].N
	f.sv.votes.mu.Unlock()
	if n < 2 {
		t.Fatalf("N=%d", n)
	}
	_, out := f.req(t, "GET", "/votes/1", f.tokens["dave"], nil)
	if v := voteOf(t, out); v["members"] != float64(n) || v["need"] != float64(n/2+1) {
		t.Errorf("view %v", v)
	}
	setN(f, id, 4) // pin N=4 for the cases below

	// 3 yes -> pass early
	mustCast(t, f, id, "yes", "alice", "dave")
	lastResult(t, f, 0)
	mustCast(t, f, id, "yes", "bob")
	if got := lastResult(t, f, 1); got != "[vote #1 result] passed yes 3 / no 0 (3 voters) · closed early: pass rule met" {
		t.Errorf("3 yes: %q", got)
	}
	// ties: 2 yes 2 no -> fail early (cannot pass: 4-2 < 3)
	id2, _ := openVote(t, f, "alice", voteNewReq{Channel: "team", Question: "tie"})
	setN(f, id2, 4)
	mustCast(t, f, id2, "yes", "alice", "dave")
	mustCast(t, f, id2, "no", "bob")
	lastResult(t, f, 1)
	mustCast(t, f, id2, "no", "carol")
	if got := lastResult(t, f, 2); got != "[vote #2 result] failed yes 2 / no 2 (4 voters) · closed early: cannot pass" {
		t.Errorf("tie: %q", got)
	}
	// 2 no, 2 not voted -> fail early
	id3, _ := openVote(t, f, "alice", voteNewReq{Channel: "team", Question: "two no"})
	setN(f, id3, 4)
	mustCast(t, f, id3, "no", "alice")
	lastResult(t, f, 2)
	mustCast(t, f, id3, "no", "dave")
	if got := lastResult(t, f, 3); got != "[vote #3 result] failed yes 0 / no 2 (2 voters) · closed early: cannot pass" {
		t.Errorf("two no: %q", got)
	}
	// 2 yes 1 no 1 not voted -> stays open; then the deadline passes only with yes >= need
	clk := fakeClock(f.sv, time.Now())
	id4, _ := openVote(t, f, "alice", voteNewReq{Channel: "team", Question: "open"})
	setN(f, id4, 4)
	mustCast(t, f, id4, "yes", "alice", "dave")
	mustCast(t, f, id4, "no", "bob")
	f.sv.voteTick(advance(clk, time.Hour))
	lastResult(t, f, 3)
	_, out = f.req(t, "GET", "/votes/4", f.tokens["dave"], nil)
	if v := voteOf(t, out); v["open"] != true || v["rule"] != "passes when yes >= 3 of 4 members (majority)" {
		t.Errorf("open vote %v", v)
	}
	f.sv.voteTick(advance(clk, 25*time.Hour)) // low turnout fails at the deadline
	if got := lastResult(t, f, 4); got != "[vote #4 result] failed yes 2 / no 1 (3 voters)" {
		t.Errorf("deadline: %q", got)
	}
	// no double result by tick or restart
	f.sv.voteTick(f.sv.now())
	restartFixture(t, f)
	fakeClock(f.sv, time.Now().Add(72*time.Hour))
	f.sv.voteTick(f.sv.now())
	lastResult(t, f, 4)
}

func TestVoteMajorityNoWinsStaysOpenOddN(t *testing.T) {
	f := setup(t)
	id, _ := openVote(t, f, "alice", voteNewReq{Channel: "team", Question: "odd"})
	setN(f, id, 5) // need 3; 2 no leaves 3 possible
	mustCast(t, f, id, "no", "alice", "dave")
	lastResult(t, f, 0)
	mustCast(t, f, id, "no", "bob") // 5-3 < 3
	if got := lastResult(t, f, 1); !strings.HasSuffix(got, "closed early: cannot pass") {
		t.Errorf("got %q", got)
	}
}

func TestVoteOldRuleRecord(t *testing.T) {
	f := setup(t)
	clk := fakeClock(f.sv, time.Now())
	id, _ := openVoteOld(t, f, "alice", voteNewReq{Channel: "team", Question: "old"})
	mustCast(t, f, id, "yes", "alice", "dave")
	mustCast(t, f, id, "no", "bob", "carol") // old rule never fails early
	lastResult(t, f, 0)
	_, out := f.req(t, "GET", "/votes/1", f.tokens["dave"], nil)
	if v := voteOf(t, out); v["rule"] != "passes when yes >= 3 and yes > no (old rule)" {
		t.Errorf("rule %v", v["rule"])
	}
	f.sv.voteTick(advance(clk, 25*time.Hour))
	if got := lastResult(t, f, 1); got != "[vote #1 result] failed yes 2 / no 2 (4 voters)" {
		t.Errorf("deadline: %q", got)
	}
}
