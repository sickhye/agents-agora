package main

// Message kinds and the status digest.
//
// send --log-only stores a message with kind "log": a record (hand-off add/done and the
// like). It is stored and delivered like any message; a2 watch shows it as one short
// "[a2] [log] ..." line so a receiver can skip it, a2 inbox shows the full text.
//
// send --status stores a message with kind "status": a status line. With --status-digest
// MIN > 0 (default 10) the server keeps the line and, once the oldest waiting line of a
// group (same sender, channel, recipient and thread) has waited MIN minutes, stores the
// whole group as ONE message of kind "digest" from that sender (liveRun checks every
// minute). A status line whose text has "OUTCOME: problem", "BLOCK" or "blocked" is
// stored at once; the waiting lines of its group go out first, so the order holds.
// The waiting lines are kept in stateDir()/status-digest.json (0600), so a restart loses
// none (a crash between storing a digest and rewriting the file can repeat that digest
// once; it never loses a line). --status-digest 0 stores every status line at once.
//
// Older clients never send a kind; older servers ignore it (the line is then plain).

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	kindLog           = "log"
	kindStatus        = "status"
	kindDigest        = "digest"
	kindReminder      = "reminder"
	digestFile        = "status-digest.json"
	defaultDigestMins = 10
	maxNeedsReply     = 7 * 24 * time.Hour
	minNeedsReply     = time.Minute
)

// sendOpts are the send-option send options.
type sendOpts struct {
	kind   string        // "", kindLog or kindStatus
	within time.Duration // needs-reply time (0 = none)
}

func (o sendOpts) check() error {
	switch o.kind {
	case "", kindLog, kindStatus:
	default:
		return errCode(http.StatusBadRequest, "unknown kind %q (log or status)", o.kind)
	}
	if o.within < 0 || o.within > maxNeedsReply || (o.within > 0 && o.within < minNeedsReply) {
		return errCode(http.StatusBadRequest, "needs_reply_sec must be %d to %d", int(minNeedsReply/time.Second), int(maxNeedsReply/time.Second))
	}
	if o.within > 0 && o.kind != "" {
		return errCode(http.StatusBadRequest, "needs_reply goes with a plain message, not kind %s", o.kind)
	}
	return nil
}

// statusUrgent: a status line with a problem or a block goes out at once.
func statusUrgent(text string) bool {
	return strings.Contains(text, "OUTCOME: problem") || strings.Contains(text, "BLOCK") ||
		strings.Contains(strings.ToLower(text), "blocked")
}

type digestItem struct {
	Channel string    `json:"channel"`
	From    string    `json:"from"`
	To      string    `json:"to"`
	ReplyTo int64     `json:"reply_to,omitempty"`
	Text    string    `json:"text"`
	At      time.Time `json:"at"`
}

func (it digestItem) key() string {
	return it.From + "\x00" + it.Channel + "\x00" + it.To + "\x00" + strconv.FormatInt(it.ReplyTo, 10)
}

// digestQueue holds the waiting status lines (in arrival order) and their file.
type digestQueue struct {
	mu    sync.Mutex
	path  string
	every time.Duration // 0 = off
	items []digestItem
}

// openDigest loads path (missing = empty). A file that does not parse is moved aside to
// path.bad, so a damaged file never stops the server.
func openDigest(path string, every time.Duration) (*digestQueue, error) {
	q := &digestQueue{path: path, every: every}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return q, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &q.items); err != nil {
		fmt.Fprintf(os.Stderr, "a2: %s: %v (moved to %s.bad)\n", path, err, path)
		os.Rename(path, path+".bad")
		q.items = nil
	}
	return q, nil
}

func (q *digestQueue) save() error {
	b, err := json.Marshal(q.items)
	if err != nil {
		return err
	}
	return writeFile0600(q.path, b)
}

// digestStatus takes a checked status message m (no id yet). queued = it waits for the
// digest, which goes out at about due; otherwise the caller stores it now.
func (sv *server) digestStatus(m Message, now time.Time) (due time.Time, queued bool) {
	q := sv.digest
	if q == nil {
		return time.Time{}, false
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	it := digestItem{Channel: m.Channel, From: m.From, To: m.To, ReplyTo: m.ReplyTo, Text: m.Text, At: now}
	if q.every <= 0 || statusUrgent(m.Text) {
		sv.flushDigestLocked(now, it.key()) // waiting lines of this group first
		return time.Time{}, false
	}
	q.items = append(q.items, it)
	if err := q.save(); err != nil {
		fmt.Fprintln(os.Stderr, "a2: status digest:", err, "(sent at once)")
		q.items = q.items[:len(q.items)-1]
		return time.Time{}, false
	}
	first := now
	for _, x := range q.items {
		if x.key() == it.key() {
			first = x.At
			break
		}
	}
	return first.Add(q.every), true
}

// digestTick stores every group whose oldest line has waited the digest time (liveRun).
func (sv *server) digestTick(now time.Time) {
	q := sv.digest
	if q == nil {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	sv.flushDigestLocked(now, "")
}

// flushDigestLocked stores the due groups (only, when only != "": that group, due or
// not) as digest messages and drops them from the queue. q.mu is held.
func (sv *server) flushDigestLocked(now time.Time, only string) {
	q := sv.digest
	groups := map[string][]digestItem{}
	var order []string
	for _, it := range q.items {
		k := it.key()
		if _, ok := groups[k]; !ok {
			order = append(order, k)
		}
		groups[k] = append(groups[k], it)
	}
	sent := map[string]bool{}
	for _, k := range order {
		g := groups[k]
		if only != "" && k != only || only == "" && now.Before(g[0].At.Add(q.every)) {
			continue
		}
		ok := true
		for _, text := range digestTexts(g) {
			m := Message{Channel: g[0].Channel, From: g[0].From, To: g[0].To, ReplyTo: g[0].ReplyTo, Text: text, Kind: kindDigest}
			if _, err := sv.store.appendMsg(m, now); err != nil {
				fmt.Fprintln(os.Stderr, "a2: status digest:", err)
				ok = false
				break
			}
		}
		if ok {
			sent[k] = true
		}
	}
	if len(sent) == 0 {
		return
	}
	keep := q.items[:0:0]
	for _, it := range q.items {
		if !sent[it.key()] {
			keep = append(keep, it)
		}
	}
	q.items = keep
	if err := q.save(); err != nil {
		fmt.Fprintln(os.Stderr, "a2: status digest:", err)
	}
}

// digestTexts builds the digest text(s) of one group: a head line, then "HH:MM text" per
// status line; a group too long for one message is split.
func digestTexts(g []digestItem) []string {
	var out, lines []string
	var first, last time.Time
	size := 0
	flush := func() {
		if len(lines) == 0 {
			return
		}
		head := fmt.Sprintf("status digest: %d lines, %s-%s", len(lines), first.In(jst).Format("15:04"), last.In(jst).Format("15:04"))
		out = append(out, head+"\n"+strings.Join(lines, "\n"))
		lines, size = nil, 0
	}
	for _, it := range g {
		l := it.At.In(jst).Format("15:04") + " " + it.Text
		if size > 0 && size+len(l)+1 > maxTextBytes-64 {
			flush()
		}
		if len(lines) == 0 {
			first = it.At
		}
		last = it.At
		lines = append(lines, l)
		size += len(l) + 1
	}
	flush()
	return out
}
