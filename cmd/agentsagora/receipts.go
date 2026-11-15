package main

// Delivery receipts.
//
// The server records, per member, which message ids it actually handed to that member's
// client: an /inbox response, a /ws push frame written to the socket, or an MCP inbox
// result. This is "fetched by the client" (delivered), not "a person read it"; nothing here
// is called "read".
//
// File: stateDir()/deliveries.jsonl, mode 0600, append only (like messages.jsonl). The
// first line is {"start": N}: messages with id <= N were written before recording began,
// so a missing record for them is "unrecorded", not "pending". Every later line is one
// delivery batch: {"member": "<base name>", "ids": [...], "at": "<RFC3339>"}. Members are
// stored under their base key (paths.go memberChannels), so a rename (names.go) keeps the
// receipts. No message text is stored.
//
// Who sees receipts: the sender of the message (GET /receipts/{id}, MCP tool receipts; the
// sender must still be able to read the channel) and the viewer UI (/view/api/messages).
// Anyone else gets the same answer as for a missing message.

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const deliveriesFile = "deliveries.jsonl"

// receipt states
const (
	rcDelivered  = "delivered"
	rcPending    = "pending"
	rcUnrecorded = "unrecorded" // message older than the delivery log, no record
)

type deliveryLine struct {
	Start  *int64    `json:"start,omitempty"`
	Member string    `json:"member,omitempty"`
	IDs    []int64   `json:"ids,omitempty"`
	At     time.Time `json:"at,omitzero"`
}

// deliveryLog is the in-memory copy of deliveries.jsonl.
type deliveryLog struct {
	mu     sync.Mutex
	path   string
	start  int64
	at     map[int64]map[string]time.Time // message id -> member base -> first delivery
	order  []int64                        // message ids in record order; the view's rseq is an index here
	notify chan struct{}                  // closed and replaced on every new record
}

// openDeliveryLog loads path; a missing file is created with start = lastID. Bad lines are
// skipped with a warning (receipts are not worth refusing to start for).
func openDeliveryLog(path string, lastID int64) (*deliveryLog, error) {
	d := &deliveryLog{path: path, at: map[int64]map[string]time.Time{}, notify: make(chan struct{})}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		d.start = lastID
		b, _ := json.Marshal(deliveryLine{Start: &lastID})
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, err
		}
		if err := writeFile0600(path, append(b, '\n')); err != nil {
			return nil, err
		}
		return d, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
	n := 0
	for sc.Scan() {
		n++
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var l deliveryLine
		if err := json.Unmarshal([]byte(line), &l); err != nil {
			fmt.Fprintf(os.Stderr, "a2: %s:%d: skipped: %v\n", path, n, err)
			continue
		}
		if l.Start != nil {
			d.start = *l.Start
			continue
		}
		d.add(l.Member, l.IDs, l.At)
	}
	return d, sc.Err()
}

// add records ids for member in memory and returns the ids that were new. Caller holds mu
// (or owns d during load).
func (d *deliveryLog) add(member string, ids []int64, at time.Time) []int64 {
	var fresh []int64
	for _, id := range ids {
		byMember := d.at[id]
		if byMember == nil {
			byMember = map[string]time.Time{}
			d.at[id] = byMember
		}
		if _, ok := byMember[member]; ok {
			continue
		}
		byMember[member] = at
		d.order = append(d.order, id)
		fresh = append(fresh, id)
	}
	return fresh
}

// record notes that ids were handed to member (base name) at now. Only new pairs are
// written. A write error is logged; the record stays in memory.
func (d *deliveryLog) record(member string, ids []int64, now time.Time) {
	if d == nil || member == "" || len(ids) == 0 {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	fresh := d.add(member, ids, now)
	if len(fresh) == 0 {
		return
	}
	b, _ := json.Marshal(deliveryLine{Member: member, IDs: fresh, At: now.In(jst)})
	f, err := os.OpenFile(d.path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err == nil {
		_, err = f.Write(append(b, '\n'))
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "a2: deliveries:", err)
	}
	close(d.notify)
	d.notify = make(chan struct{})
}

// of returns a copy of the deliveries of one message and the log start.
func (d *deliveryLog) of(id int64) (map[string]time.Time, int64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return maps.Clone(d.at[id]), d.start
}

// changedSince returns the message ids with a new record after sequence seq, and the
// current sequence. A seq beyond the log (file replaced) is treated as 0.
func (d *deliveryLog) changedSince(seq int) ([]int64, int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if seq < 0 {
		return nil, len(d.order)
	}
	if seq > len(d.order) {
		seq = 0
	}
	ids := slices.Clone(d.order[seq:])
	slices.Sort(ids)
	return slices.Compact(ids), len(d.order)
}

// poke wakes the viewer UI as a new record does (a vote change, vote.go): the view's
// socket sends {"receipts":true} and the page reloads, votes included.
func (d *deliveryLog) poke() {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	close(d.notify)
	d.notify = make(chan struct{})
}

// wake is closed on the next new record.
func (d *deliveryLog) wake() <-chan struct{} {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.notify
}

// receipt is one recipient of a message.
type receipt struct {
	Name      string `json:"name"`
	Status    string `json:"status"`              // delivered, pending or unrecorded
	Delivered string `json:"delivered,omitempty"` // RFC3339 (JST) of the first delivery
}

// recipients of a stored message under current names: to=member is that member; to=all is
// every member of this server who can read the channel now, except the sender.
func (sv *server) recipients(m Message, nt nameTable) []string {
	shown := nt.showMsg(m)
	if shown.To != allTarget {
		return []string{shown.To}
	}
	var out []string
	for _, b := range sv.members {
		n := nt.current(b)
		if n == shown.From || !slices.Contains(nt.channels(n), m.Channel) {
			continue
		}
		out = append(out, n)
	}
	return out
}

// receiptsOf is the receipt of a stored message. Callers check who may see it.
func (sv *server) receiptsOf(m Message, nt nameTable) []receipt {
	got, start := sv.deliv.of(m.ID)
	out := []receipt{}
	for _, n := range sv.recipients(m, nt) {
		r := receipt{Name: n, Status: rcPending}
		if b, ok := nt.baseOf(n); ok {
			if t, ok := got[b]; ok {
				r.Status, r.Delivered = rcDelivered, t.In(jst).Format(time.RFC3339)
			}
		}
		if r.Status == rcPending && m.ID <= start {
			r.Status = rcUnrecorded
		}
		out = append(out, r)
	}
	return out
}

// undelivered is the count behind the unread-count badge: n recipients still
// pending out of total. known is false when a recipient is unrecorded (message older than
// the delivery log), so no number is made up for it.
func undelivered(rs []receipt) (n, total int, known bool) {
	for _, r := range rs {
		switch r.Status {
		case rcPending:
			n++
		case rcUnrecorded:
			return 0, len(rs), false
		}
	}
	return n, len(rs), true
}

// delivered records that msgs were handed to member's client (member = current name).
func (sv *server) delivered(member string, msgs []Message) {
	if len(msgs) == 0 || sv.deliv == nil {
		return
	}
	ids := make([]int64, len(msgs))
	for i, m := range msgs {
		ids[i] = m.ID
	}
	sv.deliveredIDs(member, ids)
}

func (sv *server) deliveredIDs(member string, ids []int64) {
	if len(ids) == 0 || sv.deliv == nil {
		return
	}
	b, ok := currentNames().baseOf(member)
	if !ok {
		return
	}
	sv.deliv.record(b, ids, sv.now())
}

// memberReceipts is GET /receipts/{id} and the MCP receipts tool: only the sender of the
// message, and only while it can still read the channel. Everything else gets the same
// 404 as a missing message, so ids and channels do not leak.
func (sv *server) memberReceipts(member string, id int64) (map[string]any, error) {
	m, ok := sv.store.get(id)
	nt := currentNames()
	if id <= 0 || !ok || !canUse(member, m.Channel) || nt.canon(m.From) != member {
		return nil, errCode(http.StatusNotFound, "no receipts for #%d: unknown message, or not sent by you (only the sender sees receipts)", id)
	}
	shown := nt.showMsg(m)
	rs := sv.receiptsOf(m, nt)
	n, total, known := undelivered(rs)
	out := map[string]any{"id": m.ID, "channel": m.Channel, "to": shown.To, "receipts": rs, "recipients": total,
		"note": "delivered = handed to the recipient's client (inbox, watch or MCP inbox); it does not mean a person read it"}
	if known {
		out["undelivered"] = n // absent for a message older than the delivery log
	}
	return out, nil
}

func (sv *server) handleReceipts(w http.ResponseWriter, r *http.Request) {
	member := callerOf(r)
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		jsonError(w, http.StatusBadRequest, "bad id")
		return
	}
	out, err := sv.memberReceipts(member, id)
	sv.audit.write(auditEntry{Member: member, Remote: remoteIP(r), Action: "receipts", Status: statusOf(err), Target: "#" + strconv.FormatInt(id, 10)})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// viewReceipts adds the receipts of msgs (and of messages with a record after rseq) for
// the viewer UI. Messages whose recipients are all unrecorded are left out. The second map
// is the badge count per message (undelivered); a message with an unrecorded recipient is
// not in it, so the page shows no badge for it.
func (sv *server) viewReceipts(msgs []Message, rseq int, nt nameTable) (map[string][]receipt, map[string]int, int) {
	// the sequence first: a record made while this runs shows up again next time
	changed, seq := sv.deliv.changedSince(rseq) // rseq < 0 (first load): none
	out, badge := map[string][]receipt{}, map[string]int{}
	put := func(m Message) {
		rs := sv.receiptsOf(m, nt)
		if !slices.ContainsFunc(rs, func(r receipt) bool { return r.Status != rcUnrecorded }) {
			return
		}
		k := strconv.FormatInt(m.ID, 10)
		out[k] = rs
		if n, _, known := undelivered(rs); known {
			badge[k] = n
		}
	}
	for _, m := range msgs {
		put(m)
	}
	for _, id := range changed {
		if _, done := out[strconv.FormatInt(id, 10)]; done {
			continue
		}
		if m, ok := sv.store.get(id); ok {
			put(m)
		}
	}
	return out, badge, seq
}

// cmdReceipts: a2 receipts <id> — one line per recipient of a message you sent.
func cmdReceipts(args []string) error {
	if len(args) != 1 {
		return usageError("receipts needs <id> (a message you sent)")
	}
	id, err := strconv.ParseInt(strings.TrimPrefix(args[0], "#"), 10, 64)
	if err != nil || id <= 0 {
		return usageError("bad id " + strconv.Quote(args[0]))
	}
	c, err := newClient()
	if err != nil {
		return err
	}
	var r struct {
		Receipts    []receipt `json:"receipts"`
		Recipients  int       `json:"recipients"`
		Undelivered *int      `json:"undelivered"`
	}
	if err := c.do("GET", "/receipts/"+strconv.FormatInt(id, 10), nil, &r); err != nil {
		return err
	}
	fmt.Println(undeliveredLine(r.Undelivered, r.Recipients))
	for _, rc := range r.Receipts {
		fmt.Println(receiptLine(rc))
	}
	if len(r.Receipts) == 0 {
		fmt.Fprintln(os.Stderr, "a2: no recipients")
	}
	return nil
}

// undeliveredLine is the first line of a2 receipts: "undelivered N / M", or "undelivered ? / M" with a
// note for a message older than the delivery log (no made-up number).
func undeliveredLine(n *int, total int) string {
	if n == nil {
		return fmt.Sprintf("undelivered ? / %d (message predates recording)", total)
	}
	return fmt.Sprintf("undelivered %d / %d", *n, total)
}

// receiptLine is "name: delivered HH:MM:SS" or "name: not yet" (the owner reads it).
func receiptLine(rc receipt) string {
	switch rc.Status {
	case rcDelivered:
		hms := rc.Delivered
		if t, err := time.Parse(time.RFC3339, rc.Delivered); err == nil {
			hms = t.In(jst).Format("15:04:05")
		}
		return rc.Name + ": delivered " + hms
	case rcUnrecorded:
		return rc.Name + ": no record (message predates recording)"
	}
	return rc.Name + ": not yet"
}
