package main

// Needs-reply reminders.
//
// send --needs-reply [--within D] (default 30m, 1m to 7 days, one recipient) stores the
// message with reply_by = sent time + D. Every minute (liveRun) a message whose reply_by
// has passed is checked once: if the recipient has sent nothing in its thread after it
// (reply_to = the thread root; a reply to a reply is stored with the root id), the server
// stores ONE reminder to each side, from a2-system, in the same thread and channel:
//   to the recipient: "#N reply requested: <sender> (M min elapsed). a2 send --reply-to N --to <sender> ..."
//   to the sender:    "#N <recipient> no reply (M min elapsed); one reminder sent"
// The mark is kept in stateDir()/reply-reminders.jsonl (0600, append only, {"id": N,
// "at": T} per reminder), written before the reminders are stored: a crash can lose a
// reminder but never repeat one.

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const replyRemindFile = "reply-reminders.jsonl"

type replyLine struct {
	ID int64     `json:"id"`
	At time.Time `json:"at"`
}

// replyLog is the in-memory copy of reply-reminders.jsonl plus the messages already
// answered (memory only; found again after a restart).
type replyLog struct {
	mu   sync.Mutex
	path string
	done map[int64]bool
}

func openReplyLog(path string) (*replyLog, error) {
	l := &replyLog{path: path, done: map[int64]bool{}}
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
		var rl replyLine
		if err := json.Unmarshal([]byte(line), &rl); err != nil {
			fmt.Fprintf(os.Stderr, "a2: %s:%d: skipped: %v\n", path, n, err)
			continue
		}
		l.done[rl.ID] = true
	}
	return l, sc.Err()
}

func (l *replyLog) mark(id int64, at time.Time) error {
	b, _ := json.Marshal(replyLine{ID: id, At: at.In(jst)})
	if err := os.MkdirAll(filepath.Dir(l.path), 0o700); err != nil {
		return err
	}
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

// needsReply returns the messages that carry a reply_by deadline.
func (s *store) needsReply() []Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Message
	for _, m := range s.msgs {
		if m.ReplyBy != "" {
			out = append(out, m)
		}
	}
	return out
}

// repliedBy: who (a current name) sent a message in m's thread after m.
func (s *store) repliedBy(m Message, who string, nt nameTable) bool {
	root := m.ReplyTo
	if root == 0 {
		root = m.ID
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, x := range s.msgs {
		if x.ID > m.ID && x.ReplyTo == root && nt.showMsg(x).From == who {
			return true
		}
	}
	return false
}

// replyTick sends the reminders that are due (liveRun, every minute).
func (sv *server) replyTick(now time.Time) {
	l := sv.replies
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	nt := currentNames()
	for _, m := range sv.store.needsReply() {
		if l.done[m.ID] {
			continue
		}
		by, err := time.Parse(time.RFC3339, m.ReplyBy)
		if err != nil || now.Before(by) {
			continue
		}
		sm := nt.showMsg(m)
		if sv.store.repliedBy(m, sm.To, nt) {
			l.done[m.ID] = true
			continue
		}
		// mark first: a crash between the two loses the reminder but never repeats it
		if err := l.mark(m.ID, now); err != nil {
			fmt.Fprintln(os.Stderr, "a2: reply reminders:", err)
			continue
		}
		l.done[m.ID] = true
		mins := 0
		if ts, err := time.Parse(time.RFC3339, m.TS); err == nil {
			mins = int(now.Sub(ts) / time.Minute)
		}
		root := m.ReplyTo
		if root == 0 {
			root = m.ID
		}
		for _, r := range []Message{
			{To: sm.To, Text: fmt.Sprintf("#%d reply requested: %s (%d min elapsed). a2 send --reply-to %d --to %s \"...\"", m.ID, sm.From, mins, m.ID, sm.From)},
			{To: sm.From, Text: fmt.Sprintf("#%d %s no reply (%d min elapsed); one reminder sent", m.ID, sm.To, mins)},
		} {
			r.Channel, r.From, r.ReplyTo, r.Kind = m.Channel, systemSender, root, kindReminder
			if _, err := sv.store.appendMsg(r, now); err != nil {
				fmt.Fprintln(os.Stderr, "a2: reply reminder:", err)
			}
		}
	}
}
