package main

// Votes: members decide things (for example which a2 improvements to build) by
// voting.
//
// Rule: a vote is open 24 h by default (--hours 1..168). A yes/no vote passes when
// yes >= min-yes and yes > no (min-yes: server flag --vote-min-yes, default 3; the value
// is fixed in the vote when it opens). A vote with options: the winner is the option with
// the most votes; it passes only when it has >= min-yes votes and no other option has as
// many (a tie does not pass).
//
// A vote is announced as a normal message from the proposer, to all on its channel
// ("[vote #N] ..."), so watch and inbox see it with the usual line format. One vote per
// member; voting again replaces the old vote; the proposer may vote; a closed vote refuses.
// When the time is up, the server posts ONE result message from a2-system to all on the
// channel, as a reply in the vote message's thread:
// "[vote #N result] passed|failed yes N / no M (K voters)".
//
// Who: only members that can read the vote's channel can see or cast it; anyone else gets
// the same 404 as for a missing vote (general-only members never see team votes). The
// viewer UI sees every vote (counts on the vote message, /view/api/messages "votes"), but
// does not vote. a2-system is not a member and cannot vote.
//
// File: stateDir()/votes.jsonl, mode 0600, append only (like deliveries.jsonl). One line
// per event, tagged by "kind": open (the vote), msg (its message id), drop (the message
// could not be sent: the id stays used, the vote is gone), cast (member base name, choice,
// time) and closed (written BEFORE the result message is stored, so a crash can lose a
// result but never post it twice; it survives a restart).

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	votesFile         = "votes.jsonl"
	defaultVoteHours  = 24
	maxVoteHours      = 168
	defaultVoteMinYes = 3
	maxVoteOptions    = 10
	maxQuestionBytes  = 2000
	maxOptionBytes    = 200
	voteYes, voteNo   = "yes", "no"
	votePassed        = "passed"
	voteFailed        = "failed"
)

type voteRec struct {
	ID       int64     `json:"id"`
	Channel  string    `json:"channel"`
	From     string    `json:"from"` // proposer, base name
	Question string    `json:"question"`
	Options  []string  `json:"options,omitempty"` // empty = yes/no
	MinYes   int       `json:"min_yes"`
	Opened   time.Time `json:"opened"`
	Closes   time.Time `json:"closes"`

	msgID   int64
	ballots map[string]string // member base -> choice (yes, no, opt1 ...)
	castAt  map[string]time.Time
	closed  bool // result posted (mark written)
	dropped bool
}

type voteLine struct {
	Kind   string    `json:"kind"` // open, msg, drop, cast, closed
	Open   *voteRec  `json:"open,omitempty"`
	Vote   int64     `json:"vote,omitempty"`
	MsgID  int64     `json:"msg_id,omitempty"`
	Member string    `json:"member,omitempty"`
	Choice string    `json:"choice,omitempty"`
	Result string    `json:"result,omitempty"`
	At     time.Time `json:"at,omitzero"`
}

// voteLog is the in-memory copy of votes.jsonl.
type voteLog struct {
	mu    sync.Mutex
	path  string
	votes map[int64]*voteRec
	order []int64
	last  int64       // highest id used (dropped ones included)
	poke  func()      // tells the viewer UI to refresh (deliveryLog.poke)
	errf  func(error) // write errors (stderr)
}

func openVoteLog(path string) (*voteLog, error) {
	l := &voteLog{path: path, votes: map[int64]*voteRec{}}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return l, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	n := 0
	for sc.Scan() {
		n++
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var vl voteLine
		if err := json.Unmarshal([]byte(line), &vl); err != nil {
			fmt.Fprintf(os.Stderr, "a2: %s:%d: skipped: %v\n", path, n, err)
			continue
		}
		l.apply(vl)
	}
	return l, sc.Err()
}

// apply adds one line to memory (load and live writes). Caller holds mu or owns l.
func (l *voteLog) apply(vl voteLine) {
	if vl.Kind == "open" {
		if vl.Open == nil || vl.Open.ID <= 0 {
			return
		}
		v := *vl.Open
		v.ballots, v.castAt = map[string]string{}, map[string]time.Time{}
		if _, dup := l.votes[v.ID]; !dup {
			l.order = append(l.order, v.ID)
		}
		l.votes[v.ID] = &v
		l.last = max(l.last, v.ID)
		return
	}
	v := l.votes[vl.Vote]
	if v == nil {
		return
	}
	switch vl.Kind {
	case "msg":
		v.msgID = vl.MsgID
	case "drop":
		v.dropped = true
	case "cast":
		v.ballots[vl.Member], v.castAt[vl.Member] = vl.Choice, vl.At
	case "closed":
		v.closed = true
	}
}

func (l *voteLog) write(vl voteLine) error {
	b, err := json.Marshal(vl)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(l.path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(append(b, '\n'))
	if serr := f.Sync(); err == nil {
		err = serr
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// writeApply persists a line and then applies it; nothing changes in memory on a write error.
func (l *voteLog) writeApply(vl voteLine) error {
	if err := l.write(vl); err != nil {
		return err
	}
	l.apply(vl)
	if l.poke != nil {
		l.poke()
	}
	return nil
}

// --- tally ---

// voteView is what members, MCP and the viewer UI get for one vote.
type voteView struct {
	ID       int64          `json:"id"`
	Channel  string         `json:"channel"`
	From     string         `json:"from"`
	Question string         `json:"question"`
	Options  []string       `json:"options,omitempty"` // empty = yes/no; choice optN is Options[N-1]
	Choices  []string       `json:"choices"`           // yes, no / opt1 ... in display order
	MinYes   int            `json:"min_yes"`
	Opened   string         `json:"opened"`
	Closes   string         `json:"closes"`
	Open     bool           `json:"open"`
	MsgID    int64          `json:"msg_id,omitempty"`
	Counts   map[string]int `json:"counts"`
	Ballots  []ballot       `json:"ballots"`
	Voters   int            `json:"voters"`
	Passed   bool           `json:"passed"`           // by the counts so far; final once closed
	Winner   string         `json:"winner,omitempty"` // options: the single top option
	Result   string         `json:"result"`           // passed / failed (so far while open)
	Posted   bool           `json:"result_posted"`
	Rule     string         `json:"rule"`
}

type ballot struct {
	Name   string `json:"name"`
	Choice string `json:"choice"`
	At     string `json:"at"`
}

func (v *voteRec) choices() []string {
	if len(v.Options) == 0 {
		return []string{voteYes, voteNo}
	}
	out := make([]string, len(v.Options))
	for i := range v.Options {
		out[i] = "opt" + strconv.Itoa(i+1)
	}
	return out
}

func (v *voteRec) isOpen(now time.Time) bool { return !v.closed && now.Before(v.Closes) }

// tally counts the ballots and applies the rule.
func (v *voteRec) tally() (counts map[string]int, passed bool, winner string) {
	counts = map[string]int{}
	for _, c := range v.choices() {
		counts[c] = 0
	}
	for _, c := range v.ballots {
		if _, ok := counts[c]; ok {
			counts[c]++
		}
	}
	if len(v.Options) == 0 {
		return counts, counts[voteYes] >= v.MinYes && counts[voteYes] > counts[voteNo], ""
	}
	top, n := "", -1
	tie := false
	for _, c := range v.choices() {
		switch {
		case counts[c] > n:
			top, n, tie = c, counts[c], false
		case counts[c] == n:
			tie = true
		}
	}
	if tie || n <= 0 {
		return counts, false, ""
	}
	return counts, n >= v.MinYes, top
}

func (v *voteRec) rule() string {
	if len(v.Options) == 0 {
		return fmt.Sprintf("passes when yes >= %d and yes > no", v.MinYes)
	}
	return fmt.Sprintf("the option with the most votes passes when it has >= %d votes (a tie does not pass)", v.MinYes)
}

func (v *voteRec) view(nt nameTable, now time.Time) voteView {
	counts, passed, winner := v.tally()
	vv := voteView{ID: v.ID, Channel: v.Channel, From: nt.current(v.From), Question: v.Question, Options: v.Options,
		Choices: v.choices(), MinYes: v.MinYes, Opened: v.Opened.In(jst).Format(time.RFC3339), Closes: v.Closes.In(jst).Format(time.RFC3339),
		Open: v.isOpen(now), MsgID: v.msgID, Counts: counts, Ballots: []ballot{}, Voters: len(v.ballots),
		Passed: passed, Winner: winner, Result: voteFailed, Posted: v.closed, Rule: v.rule()}
	if passed {
		vv.Result = votePassed
	}
	for b, c := range v.ballots {
		vv.Ballots = append(vv.Ballots, ballot{Name: nt.current(b), Choice: c, At: v.castAt[b].In(jst).Format(time.RFC3339)})
	}
	slices.SortFunc(vv.Ballots, func(a, b ballot) int { return strings.Compare(a.Name, b.Name) })
	return vv
}

// countsText is "yes 4 / no 1" or "opt1 1 / opt2 4 / opt3 0".
func countsText(choices []string, counts map[string]int) string {
	parts := make([]string, len(choices))
	for i, c := range choices {
		parts[i] = fmt.Sprintf("%s %d", c, counts[c])
	}
	return strings.Join(parts, " / ")
}

// resultText is the one result message.
func (v *voteRec) resultText() string {
	counts, passed, winner := v.tally()
	res := voteFailed
	if passed {
		res = votePassed
	}
	if len(v.Options) == 0 {
		return fmt.Sprintf("[vote #%d result] %s yes %d / no %d (%d voters)", v.ID, res, counts[voteYes], counts[voteNo], len(v.ballots))
	}
	head := fmt.Sprintf("[vote #%d result] %s", v.ID, res)
	if winner != "" {
		i, _ := strconv.Atoi(strings.TrimPrefix(winner, "opt"))
		head += fmt.Sprintf(" %s (%s) %d votes", winner, v.Options[i-1], counts[winner])
	} else if len(v.ballots) > 0 {
		head += " tie"
	}
	return fmt.Sprintf("%s — %s (%d voters)", head, countsText(v.choices(), counts), len(v.ballots))
}

// announceText is the vote message.
func (v *voteRec) announceText() string {
	var b strings.Builder
	fmt.Fprintf(&b, "[vote #%d] %s\n", v.ID, v.Question)
	if len(v.Options) == 0 {
		b.WriteString("Choices: yes / no\n")
		fmt.Fprintf(&b, "Vote: a2 vote %d yes or a2 vote %d no (MCP: vote_cast) · status: a2 vote show %d\n", v.ID, v.ID, v.ID)
		fmt.Fprintf(&b, "Closes: %s JST · pass rule: at least %d yes votes and yes > no", v.Closes.In(jst).Format("01-02 15:04"), v.MinYes)
		return b.String()
	}
	parts := make([]string, len(v.Options))
	for i, o := range v.Options {
		parts[i] = fmt.Sprintf("opt%d %s", i+1, o)
	}
	fmt.Fprintf(&b, "Choices: %s\n", strings.Join(parts, " / "))
	fmt.Fprintf(&b, "Vote: a2 vote %d opt1 (opt1..opt%d, MCP: vote_cast) · status: a2 vote show %d\n", v.ID, len(v.Options), v.ID)
	fmt.Fprintf(&b, "Closes: %s JST · pass rule: the top choice has at least %d votes (a tie fails)", v.Closes.In(jst).Format("01-02 15:04"), v.MinYes)
	return b.String()
}

// parseChoice maps yes/no, optN, N or an option's exact text (any case) to the stored choice.
func (v *voteRec) parseChoice(s string) (string, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if len(v.Options) == 0 {
		if s == voteYes || s == voteNo {
			return s, nil
		}
		return "", errCode(http.StatusBadRequest, "vote #%d is yes/no: give yes or no", v.ID)
	}
	n, err := strconv.Atoi(strings.TrimPrefix(s, "opt"))
	if err == nil && n >= 1 && n <= len(v.Options) {
		return "opt" + strconv.Itoa(n), nil
	}
	for i, o := range v.Options {
		if strings.ToLower(o) == s {
			return "opt" + strconv.Itoa(i+1), nil
		}
	}
	return "", errCode(http.StatusBadRequest, "vote #%d: give opt1..opt%d", v.ID, len(v.Options))
}

// --- operations (HTTP and MCP) ---

// visibleVote is the vote id for member, or the same 404 as a missing vote when member
// cannot read its channel. Caller holds votes.mu.
func (sv *server) visibleVote(member string, id int64) (*voteRec, error) {
	v := sv.votes.votes[id]
	if id <= 0 || v == nil || v.dropped || !canUse(member, v.Channel) {
		return nil, errCode(http.StatusNotFound, "unknown vote #%d", id)
	}
	return v, nil
}

func cleanOptions(opts []string) ([]string, error) {
	if len(opts) == 0 {
		return nil, nil
	}
	if len(opts) < 2 || len(opts) > maxVoteOptions {
		return nil, errCode(http.StatusBadRequest, "give no options (yes/no) or 2..%d options", maxVoteOptions)
	}
	out := make([]string, len(opts))
	seen := map[string]bool{}
	for i, o := range opts {
		o = strings.Join(strings.Fields(o), " ")
		low := strings.ToLower(o)
		if o == "" || len(o) > maxOptionBytes {
			return nil, errCode(http.StatusBadRequest, "option %d: empty or over %d bytes", i+1, maxOptionBytes)
		}
		if seen[low] {
			return nil, errCode(http.StatusBadRequest, "option %q given twice", o)
		}
		seen[low] = true
		out[i] = o
	}
	return out, nil
}

// voteNew opens a vote and posts its message (from member, to all on the channel).
func (sv *server) voteNew(member, channel, question string, options []string, hours int) (voteView, Message, error) {
	if member == systemSender || member == viewerName {
		return voteView{}, Message{}, errCode(http.StatusForbidden, "%s cannot open a vote", member)
	}
	ch, err := sv.resolveChannel(member, channel)
	if err != nil {
		return voteView{}, Message{}, err
	}
	question = strings.TrimSpace(question)
	if question == "" || len(question) > maxQuestionBytes {
		return voteView{}, Message{}, errCode(http.StatusBadRequest, "question empty or over %d bytes", maxQuestionBytes)
	}
	opts, err := cleanOptions(options)
	if err != nil {
		return voteView{}, Message{}, err
	}
	userText := append([]string{question}, opts...)
	if err := sv.lint(ch, userText...); err != nil { // general: before an id is used
		return voteView{}, Message{}, err
	}
	if hours == 0 {
		hours = defaultVoteHours
	}
	if hours < 1 || hours > maxVoteHours {
		return voteView{}, Message{}, errCode(http.StatusBadRequest, "hours must be 1..%d", maxVoteHours)
	}
	nt := currentNames()
	base, ok := nt.baseOf(member)
	if !ok {
		return voteView{}, Message{}, errCode(http.StatusForbidden, "not a member")
	}
	now := sv.now()
	l := sv.votes
	l.mu.Lock()
	defer l.mu.Unlock()
	v := &voteRec{ID: l.last + 1, Channel: ch, From: base, Question: question, Options: opts, MinYes: sv.voteMinYes,
		Opened: now.In(jst), Closes: now.Add(time.Duration(hours) * time.Hour).In(jst)}
	if err := l.writeApply(voteLine{Kind: "open", Open: v}); err != nil {
		return voteView{}, Message{}, fmt.Errorf("votes: %w", err)
	}
	v = l.votes[v.ID]
	m, err := sv.sendOnLint(member, ch, allTarget, v.announceText(), 0, userText...)
	if err != nil {
		if derr := l.writeApply(voteLine{Kind: "drop", Vote: v.ID}); derr != nil {
			fmt.Fprintln(os.Stderr, "a2: votes:", derr)
			v.dropped = true // memory at least
		}
		return voteView{}, Message{}, err
	}
	if err := l.writeApply(voteLine{Kind: "msg", Vote: v.ID, MsgID: m.ID}); err != nil {
		fmt.Fprintln(os.Stderr, "a2: votes:", err)
		v.msgID = m.ID
	}
	return v.view(nt, now), m, nil
}

// voteCast stores member's vote (replacing an earlier one) and returns the vote and the
// earlier choice ("" for a first vote).
func (sv *server) voteCast(member string, id int64, choice string) (voteView, string, error) {
	if member == systemSender || member == viewerName {
		return voteView{}, "", errCode(http.StatusForbidden, "%s cannot vote", member)
	}
	nt := currentNames()
	base, ok := nt.baseOf(member)
	if !ok || !sv.isMember(nt, member) {
		return voteView{}, "", errCode(http.StatusForbidden, "only members can vote")
	}
	now := sv.now()
	l := sv.votes
	l.mu.Lock()
	defer l.mu.Unlock()
	v, err := sv.visibleVote(member, id)
	if err != nil {
		return voteView{}, "", err
	}
	if !v.isOpen(now) {
		return voteView{}, "", errCode(http.StatusConflict, "vote #%d is closed (closed at %s JST)", id, v.Closes.In(jst).Format("01-02 15:04"))
	}
	c, err := v.parseChoice(choice)
	if err != nil {
		return voteView{}, "", err
	}
	prev := v.ballots[base]
	if err := l.writeApply(voteLine{Kind: "cast", Vote: id, Member: base, Choice: c, At: now.In(jst)}); err != nil {
		return voteView{}, "", fmt.Errorf("votes: %w", err)
	}
	return v.view(nt, now), prev, nil
}

func (sv *server) voteShow(member string, id int64) (voteView, error) {
	l := sv.votes
	l.mu.Lock()
	defer l.mu.Unlock()
	v, err := sv.visibleVote(member, id)
	if err != nil {
		return voteView{}, err
	}
	return v.view(currentNames(), sv.now()), nil
}

// voteList is the votes on member's channels (or one of them), newest first.
func (sv *server) voteList(member, channel string, openOnly bool) ([]voteView, error) {
	chs, err := sv.readChannels(member, channel)
	if err != nil {
		return nil, err
	}
	nt, now := currentNames(), sv.now()
	l := sv.votes
	l.mu.Lock()
	defer l.mu.Unlock()
	out := []voteView{}
	for i := len(l.order) - 1; i >= 0; i-- {
		v := l.votes[l.order[i]]
		if v.dropped || !slices.Contains(chs, v.Channel) || (openOnly && !v.isOpen(now)) {
			continue
		}
		out = append(out, v.view(nt, now))
	}
	return out, nil
}

// viewVotes is every vote with a message, keyed by message id, for the viewer UI.
func (sv *server) viewVotes() map[string]voteView {
	nt, now := currentNames(), sv.now()
	l := sv.votes
	l.mu.Lock()
	defer l.mu.Unlock()
	out := map[string]voteView{}
	for _, id := range l.order {
		if v := l.votes[id]; !v.dropped && v.msgID != 0 {
			out[strconv.FormatInt(v.msgID, 10)] = v.view(nt, now)
		}
	}
	return out
}

// voteTick posts the result of every vote whose time is up, once (liveRun, every minute).
func (sv *server) voteTick(now time.Time) {
	l := sv.votes
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, id := range l.order {
		v := l.votes[id]
		if v.dropped || v.closed || now.Before(v.Closes) {
			continue
		}
		text := v.resultText()
		res := voteFailed
		if strings.HasPrefix(text, fmt.Sprintf("[vote #%d result] %s", v.ID, votePassed)) {
			res = votePassed
		}
		// mark first: a crash between the two loses the result message but never repeats it
		if err := l.writeApply(voteLine{Kind: "closed", Vote: id, Result: res, At: now.In(jst)}); err != nil {
			fmt.Fprintln(os.Stderr, "a2: votes:", err)
			return // try again next tick
		}
		if _, err := sv.store.appendReply(v.Channel, systemSender, allTarget, text, "", v.msgID, now); err != nil {
			fmt.Fprintln(os.Stderr, "a2: vote result:", err)
		}
	}
}

// --- HTTP ---

type voteNewReq struct {
	Channel  string   `json:"channel,omitempty"`
	Question string   `json:"question"`
	Options  []string `json:"options,omitempty"`
	Hours    int      `json:"hours,omitempty"`
}

type voteCastReq struct {
	Choice string `json:"choice"`
}

func (sv *server) voteRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /votes", sv.auth(sv.handleVoteNew))
	mux.HandleFunc("GET /votes", sv.auth(sv.handleVoteList))
	mux.HandleFunc("GET /votes/{id}", sv.auth(sv.handleVoteShow))
	mux.HandleFunc("POST /votes/{id}", sv.auth(sv.handleVoteCast))
}

func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		jsonError(w, http.StatusBadRequest, "bad JSON body")
		return false
	}
	return true
}

func voteIDOf(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(strings.TrimPrefix(r.PathValue("id"), "#"), 10, 64)
	if err != nil || id <= 0 {
		jsonError(w, http.StatusBadRequest, "bad vote id")
		return 0, false
	}
	return id, true
}

func (sv *server) handleVoteNew(w http.ResponseWriter, r *http.Request) {
	member := callerOf(r)
	var req voteNewReq
	if !decodeBody(w, r, &req) {
		return
	}
	vv, m, err := sv.voteNew(member, req.Channel, req.Question, req.Options, req.Hours)
	sv.audit.write(auditEntry{Member: member, Remote: remoteIP(r), Action: "vote_new", Channel: m.Channel + orReq(m.Channel, req.Channel), Size: int64(len(req.Question)), Status: statusOf(err), Target: voteTarget(vv.ID)})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"vote": vv, "message": m})
}

func voteTarget(id int64) string {
	if id == 0 {
		return ""
	}
	return "vote #" + strconv.FormatInt(id, 10)
}

func (sv *server) handleVoteCast(w http.ResponseWriter, r *http.Request) {
	member := callerOf(r)
	id, ok := voteIDOf(w, r)
	if !ok {
		return
	}
	var req voteCastReq
	if !decodeBody(w, r, &req) {
		return
	}
	vv, prev, err := sv.voteCast(member, id, req.Choice)
	sv.audit.write(auditEntry{Member: member, Remote: remoteIP(r), Action: "vote_cast", Channel: vv.Channel, Status: statusOf(err), Target: voteTarget(id)})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"vote": vv, "previous": prev})
}

func (sv *server) handleVoteShow(w http.ResponseWriter, r *http.Request) {
	id, ok := voteIDOf(w, r)
	if !ok {
		return
	}
	vv, err := sv.voteShow(callerOf(r), id)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"vote": vv})
}

func (sv *server) handleVoteList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	vs, err := sv.voteList(callerOf(r), q.Get("channel"), q.Get("open") == "1" || q.Get("open") == "true")
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"votes": vs})
}

// --- CLI ---

const voteUsage = `a2 vote new [--channel team|general] [--hours H] "<question>" [opt1 opt2 ...]   (no options = yes/no)
a2 vote <id> <yes|no|optN>
a2 vote show <id>
a2 vote list [--channel C] [--open]`

func cmdVote(args []string) error {
	if len(args) == 0 {
		return usageError("vote needs a subcommand:\n" + voteUsage)
	}
	switch args[0] {
	case "new":
		return cmdVoteNew(args[1:])
	case "show":
		return cmdVoteShow(args[1:])
	case "list":
		return cmdVoteList(args[1:])
	}
	if len(args) != 2 {
		return usageError("usage:\n" + voteUsage)
	}
	id, err := parseVoteID(args[0])
	if err != nil {
		return err
	}
	c, err := newClient()
	if err != nil {
		return err
	}
	var r struct {
		Vote     voteView `json:"vote"`
		Previous string   `json:"previous"`
	}
	if err := c.do("POST", "/votes/"+strconv.FormatInt(id, 10), voteCastReq{Choice: args[1]}, &r); err != nil {
		return err
	}
	line := fmt.Sprintf("voted on #%d", id)
	if r.Previous != "" {
		line += " (replaced " + r.Previous + ")"
	}
	fmt.Printf("%s — %s (%d voters, closes %s JST)\n", line, countsText(r.Vote.Choices, r.Vote.Counts), r.Vote.Voters, shortTime(r.Vote.Closes))
	return nil
}

func parseVoteID(s string) (int64, error) {
	id, err := strconv.ParseInt(strings.TrimPrefix(s, "#"), 10, 64)
	if err != nil || id <= 0 {
		return 0, usageError("bad vote id " + strconv.Quote(s) + "\n" + voteUsage)
	}
	return id, nil
}

func shortTime(rfc string) string {
	if t, err := time.Parse(time.RFC3339, rfc); err == nil {
		return t.In(jst).Format("01-02 15:04")
	}
	return rfc
}

func cmdVoteNew(args []string) error {
	fs := flag.NewFlagSet("vote new", flag.ContinueOnError)
	channel := fs.String("channel", "", "team or general; default: your first channel")
	hours := fs.Int("hours", defaultVoteHours, "hours the vote stays open (1..168)")
	if err := fs.Parse(args); err != nil {
		return usageError(err.Error())
	}
	if fs.NArg() < 1 || fs.NArg() == 2 {
		return usageError("vote new needs a question and no options (yes/no) or 2 or more options:\n" + voteUsage)
	}
	if *hours < 1 || *hours > maxVoteHours {
		return usageError("--hours must be 1..168")
	}
	c, err := newClient()
	if err != nil {
		return err
	}
	var r struct {
		Vote    voteView `json:"vote"`
		Message Message  `json:"message"`
	}
	if err := c.do("POST", "/votes", voteNewReq{Channel: *channel, Question: fs.Arg(0), Options: fs.Args()[1:], Hours: *hours}, &r); err != nil {
		return err
	}
	kind := "yes/no"
	if len(r.Vote.Options) > 0 {
		kind = strings.Join(r.Vote.Choices, "/")
	}
	fmt.Printf("vote #%d opened in %s (%s, closes %s JST, message #%d; %s)\n", r.Vote.ID, r.Vote.Channel, kind, shortTime(r.Vote.Closes), r.Message.ID, r.Vote.Rule)
	return nil
}

func cmdVoteShow(args []string) error {
	if len(args) != 1 {
		return usageError("vote show needs <id>")
	}
	id, err := parseVoteID(args[0])
	if err != nil {
		return err
	}
	c, err := newClient()
	if err != nil {
		return err
	}
	var r struct {
		Vote voteView `json:"vote"`
	}
	if err := c.do("GET", "/votes/"+strconv.FormatInt(id, 10), nil, &r); err != nil {
		return err
	}
	for _, l := range voteShowLines(r.Vote) {
		fmt.Println(l)
	}
	return nil
}

// voteShowLines is the a2 vote show output.
func voteShowLines(v voteView) []string {
	state := "open (closes " + shortTime(v.Closes) + " JST)"
	if !v.Open {
		state = "closed (" + shortTime(v.Closes) + " JST)"
	}
	out := []string{fmt.Sprintf("vote #%d [%s] by %s — %s", v.ID, v.Channel, v.From, state), "Question: " + v.Question}
	for i, o := range v.Options {
		out = append(out, fmt.Sprintf("  opt%d: %s", i+1, o))
	}
	out = append(out, fmt.Sprintf("%s (%d voters) · rule: %s", countsText(v.Choices, v.Counts), v.Voters, v.Rule))
	for _, b := range v.Ballots {
		out = append(out, fmt.Sprintf("  %s: %s", b.Name, b.Choice))
	}
	res := "Result: " + v.Result
	if v.Winner != "" {
		res += " (" + v.Winner + ")"
	}
	if v.Open {
		res = "So far: " + v.Result
	}
	return append(out, res)
}

func cmdVoteList(args []string) error {
	fs := flag.NewFlagSet("vote list", flag.ContinueOnError)
	channel := fs.String("channel", "", "only this channel")
	open := fs.Bool("open", false, "only open votes")
	if err := fs.Parse(args); err != nil {
		return usageError(err.Error())
	}
	if fs.NArg() != 0 {
		return usageError("vote list takes only --channel and --open")
	}
	c, err := newClient()
	if err != nil {
		return err
	}
	q := "/votes?channel=" + url.QueryEscape(*channel)
	if *open {
		q += "&open=1"
	}
	var r struct {
		Votes []voteView `json:"votes"`
	}
	if err := c.do("GET", q, nil, &r); err != nil {
		return err
	}
	for _, v := range r.Votes {
		state := "open until " + shortTime(v.Closes)
		if !v.Open {
			state = "closed " + v.Result
		}
		fmt.Printf("#%d [%s] %s — %s (%d voters) — %s\n", v.ID, v.Channel, state, countsText(v.Choices, v.Counts), v.Voters, oneLine(v.Question))
	}
	if len(r.Votes) == 0 {
		fmt.Fprintln(os.Stderr, "a2: no votes")
	}
	return nil
}

func oneLine(s string) string { return strings.NewReplacer("\r", " ", "\n", " ").Replace(s) }
