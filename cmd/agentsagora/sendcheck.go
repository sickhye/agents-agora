package main

// Send-time checks and inbox filters.
//
// A) Away notice at send time. When a message goes to a member that is not alive (the
// liveness.go rule: no open /ws and no contact within liveWindow), the send response
// carries "away", one line the CLI prints after "sent #N ...". For to=all it lists the
// recipients of that channel that are not alive (receipts.go recipients: members who can
// read the channel, without the sender). It shows only what GET /members already shows
// every member (alive, last contact). The message is stored and delivered as before.
//
// B) Undelivered notice. Every liveness tick, a message whose recipients have not all
// received it --undelivered-after minutes after it was sent gets ONE notice to its sender,
// on the same channel, from the system sender "a2-system":
// "#<id> undelivered K/M (N min elapsed): name, name (long absent)". The mark is kept in
// stateDir()/undelivered-notices.jsonl (0600, append only, like deliveries.jsonl): first
// line {"start": N} (messages with id <= N were sent before the feature: never checked),
// then {"id": X, "at": T} per notice (written before the notice is stored, so a crash can
// lose a notice but never repeat one) and {"through": X} when the check cursor moves.
// System notices and messages sent by the viewer UI are never checked.
//
// C) Inbox filters: since_time (RFC3339, or HH:MM today in JST), from (member, old name
// or a2-system) and thread (the message and every message whose reply_to chain leads to
// it). They only narrow what the member's inbox already returns.

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	systemSender           = "a2-system" // sender of server notices; never a member or token name
	noticesFile            = "undelivered-notices.jsonl"
	defaultUndeliveredMins = 10
	longAbsent             = 24 * time.Hour // "(long absent)" after a recipient not seen this long
	awayTail               = " (the message is kept and will be delivered when they connect)"
)

// --- A) away notice ---

// lastSeenText is "HH:MM" (JST) for today, "MM-DD HH:MM" for an earlier day.
func lastSeenText(t, now time.Time) string {
	t, now = t.In(jst), now.In(jst)
	if t.Format("2006-01-02") == now.Format("2006-01-02") {
		return t.Format("15:04")
	}
	return t.Format("01-02 15:04")
}

// awayNotice is the line for a just stored message m ("" when every recipient is alive).
func (sv *server) awayNotice(m Message, nt nameTable, now time.Time) string {
	type away struct{ name, last string }
	var out []away
	for _, n := range sv.recipients(m, nt) {
		b, ok := nt.baseOf(n)
		if !ok {
			continue
		}
		lv := sv.liveOf(nt, b, now)
		if lv.alive {
			continue
		}
		last := "no last-seen record"
		if lv.seen {
			last = "last seen " + lastSeenText(lv.last, now)
		}
		out = append(out, away{n, last})
	}
	if len(out) == 0 {
		return ""
	}
	if nt.showMsg(m).To != allTarget {
		return "[a2] notice: " + out[0].name + " no response — " + out[0].last + awayTail
	}
	parts := make([]string, len(out))
	for i, a := range out {
		parts[i] = a.name + " (" + a.last + ")"
	}
	return fmt.Sprintf("[a2] notice: no response from %d — %s%s", len(out), strings.Join(parts, ", "), awayTail)
}

// --- B) undelivered notice ---

type noticeLine struct {
	Start   *int64    `json:"start,omitempty"`
	ID      int64     `json:"id,omitempty"`
	Through int64     `json:"through,omitempty"`
	At      time.Time `json:"at,omitzero"`
}

// noticeLog is the in-memory copy of undelivered-notices.jsonl.
type noticeLog struct {
	mu       sync.Mutex
	path     string
	start    int64
	through  int64
	notified map[int64]bool
}

// openNoticeLog loads path; a missing file is created with start = lastID (no backfill).
func openNoticeLog(path string, lastID int64) (*noticeLog, error) {
	l := &noticeLog{path: path, notified: map[int64]bool{}}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		l.start = lastID
		b, _ := json.Marshal(noticeLine{Start: &lastID})
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, err
		}
		if err := writeFile0600(path, append(b, '\n')); err != nil {
			return nil, err
		}
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
		var nl noticeLine
		if err := json.Unmarshal([]byte(line), &nl); err != nil {
			fmt.Fprintf(os.Stderr, "a2: %s:%d: skipped: %v\n", path, n, err)
			continue
		}
		switch {
		case nl.Start != nil:
			l.start = *nl.Start
		case nl.ID > 0:
			l.notified[nl.ID] = true
		case nl.Through > l.through:
			l.through = nl.Through
		}
	}
	return l, sc.Err()
}

func (l *noticeLog) appendLine(nl noticeLine) error {
	b, _ := json.Marshal(nl)
	f, err := os.OpenFile(l.path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(append(b, '\n'))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// enableUndelivered turns the undelivered notice on (after > 0) and opens its mark file.
func (sv *server) enableUndelivered(after time.Duration) error {
	if after <= 0 {
		sv.undelivAfter, sv.notices = 0, nil
		return nil
	}
	l, err := openNoticeLog(filepath.Join(stateDir(), noticesFile), sv.store.lastID())
	if err != nil {
		return err
	}
	sv.undelivAfter, sv.notices = after, l
	return nil
}

// undeliveredTick checks the messages older than undelivAfter once each, in id order, and
// stores one system notice to the sender for each one still pending for some recipient.
func (sv *server) undeliveredTick(now time.Time) {
	l := sv.notices
	if l == nil || sv.undelivAfter <= 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	nt := currentNames()
	from := max(l.start, l.through)
	through := l.through
	for _, m := range sv.store.after(from) {
		ts, err := time.Parse(time.RFC3339, m.TS)
		if err == nil && now.Sub(ts) < sv.undelivAfter {
			break // ids grow with time: the rest are younger
		}
		through = m.ID
		if err != nil || l.notified[m.ID] || m.From == systemSender {
			continue
		}
		sender := nt.canon(m.From)
		if sender == viewerName || !sv.isMember(nt, sender) || !slices.Contains(nt.channels(sender), m.Channel) {
			continue // viewer UI messages, and senders that left the channel
		}
		rs := sv.receiptsOf(m, nt)
		if len(sv.undelivSkip) > 0 { // --undelivered-skip: these members are left out of the notice
			kept := rs[:0:0]
			for _, r := range rs {
				if !sv.undelivSkip[nt.canon(r.Name)] {
					kept = append(kept, r)
				}
			}
			if len(kept) == 0 {
				continue // every recipient is skipped: no notice
			}
			rs = kept
		}
		n, total, known := undelivered(rs)
		if !known || n == 0 {
			continue
		}
		var names []string
		for _, r := range rs {
			if r.Status != rcPending {
				continue
			}
			name := r.Name
			if b, ok := nt.baseOf(r.Name); ok {
				if lv := sv.liveOf(nt, b, now); !lv.alive && (!lv.seen || now.Sub(lv.last) >= longAbsent) {
					name += " (long absent)"
				}
			}
			names = append(names, name)
		}
		// mark first: a crash between the two loses this notice but never repeats it
		if err := l.appendLine(noticeLine{ID: m.ID, At: now.In(jst)}); err != nil {
			fmt.Fprintln(os.Stderr, "a2: undelivered notices:", err)
			through = m.ID - 1 // try again next tick
			break
		}
		l.notified[m.ID] = true
		text := fmt.Sprintf("#%d undelivered %d/%d (%d min elapsed): %s", m.ID, n, total, int(sv.undelivAfter/time.Minute), strings.Join(names, ", "))
		if _, err := sv.store.append(m.Channel, systemSender, sender, text, "", now); err != nil {
			fmt.Fprintln(os.Stderr, "a2: undelivered notice:", err)
		}
	}
	if through > l.through {
		if err := l.appendLine(noticeLine{Through: through}); err != nil {
			fmt.Fprintln(os.Stderr, "a2: undelivered notices:", err)
			return
		}
		l.through = through
	}
}

// --- C) inbox filters ---

type inboxFilter struct {
	after  time.Time // messages at or after this time
	from   string    // current sender name
	thread int64     // thread of this message id
}

func (f inboxFilter) empty() bool { return f.after.IsZero() && f.from == "" && f.thread == 0 }

// parseSinceTime reads RFC3339, or HH:MM as today in JST (by now).
func parseSinceTime(s string, now time.Time) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	if hm, err := time.Parse("15:04", s); err == nil && len(s) == 5 {
		d := now.In(jst)
		return time.Date(d.Year(), d.Month(), d.Day(), hm.Hour(), hm.Minute(), 0, 0, jst), nil
	}
	return time.Time{}, fmt.Errorf("bad time %q: give RFC3339 (2026-11-12T09:30:00+09:00) or HH:MM (today, JST)", s)
}

// parseFilter reads the filter arguments of /inbox and the MCP inbox tool.
func (sv *server) parseFilter(sinceTime, from, thread string, nt nameTable) (inboxFilter, error) {
	var f inboxFilter
	if sinceTime != "" {
		t, err := parseSinceTime(sinceTime, sv.now())
		if err != nil {
			return f, errCode(http.StatusBadRequest, "since_time: %s", err.Error())
		}
		f.after = t
	}
	if from != "" {
		cur := nt.canon(from)
		if cur != systemSender && !sv.isMember(nt, cur) {
			return f, errCode(http.StatusBadRequest, "from: unknown member %s", strconv.Quote(from))
		}
		f.from = cur
	}
	if thread != "" {
		n, err := strconv.ParseInt(strings.TrimPrefix(thread, "#"), 10, 64)
		if err != nil || n <= 0 {
			return f, errCode(http.StatusBadRequest, "bad thread")
		}
		f.thread = n
	}
	return f, nil
}

// threadSet is root plus every message whose reply_to chain leads to it (a parent always
// has a smaller id, so one pass in id order is enough).
func (s *store) threadSet(root int64) map[int64]bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	set := map[int64]bool{root: true}
	for _, m := range s.msgs {
		if m.ID > root && m.ReplyTo != 0 && set[m.ReplyTo] {
			set[m.ID] = true
		}
	}
	return set
}

// inboxFiltered is store.inbox narrowed by f (never wider: a filter only drops messages).
func (sv *server) inboxFiltered(member string, since int64, chs []string, nt nameTable, f inboxFilter) ([]Message, <-chan struct{}) {
	msgs, ch := sv.store.inbox(member, since, chs, nt)
	if f.empty() {
		return msgs, ch
	}
	var set map[int64]bool
	if f.thread != 0 {
		set = sv.store.threadSet(f.thread)
	}
	out := msgs[:0]
	for _, m := range msgs {
		if f.from != "" && m.From != f.from {
			continue
		}
		if set != nil && !set[m.ID] {
			continue
		}
		if !f.after.IsZero() {
			ts, err := time.Parse(time.RFC3339, m.TS)
			if err != nil || ts.Before(f.after) {
				continue
			}
		}
		out = append(out, m)
	}
	return out, ch
}
