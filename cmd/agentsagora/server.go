package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	maxTextBytes    = 16 * 1024 // message text limit
	maxBodyBytes    = 64 * 1024 // /send body limit (JSON escaping can grow the text)
	maxFileBody     = maxFileBytes + 1024
	maxMCPBody      = maxFileBytes/3*4 + 64*1024 // base64 of the largest file plus JSON
	maxWait         = 300                        // seconds, long-poll cap
	rateBurst       = 20                         // messages (and files) a member can send at once
	ratePerMin      = 30                         // refill per minute
	reqBurst        = 120                        // any authenticated request
	reqPerMin       = 600                        //
	uploadBurst     = 200 << 20                  // file bytes a member can upload at once
	uploadPerMin    = (200 << 20) / 60           // 200 MB per hour
	authFailBurst   = 10                         // bad tokens per source address
	authFailPerMin  = 10                         //
	mcpInboxDefault = 100                        // messages per MCP inbox call
)

var jst = time.FixedZone("JST", 9*3600)

// Message is one line of messages.jsonl. Lines written before channels existed have no
// channel; they were work talk, so they load as team.
type Message struct {
	ID      int64  `json:"id"`
	TS      string `json:"ts"`
	Channel string `json:"channel"`
	From    string `json:"from"`
	To      string `json:"to"`
	Text    string `json:"text"`
	FileID  string `json:"file_id,omitempty"`
	// ReplyTo is the id of the thread root this message answers (0 = not a reply). Lines
	// written before threads existed have none. Always a root: a reply to a reply is stored
	// with the root's id, so threads are one level deep.
	ReplyTo int64 `json:"reply_to,omitempty"`
	// Notice is only set in a send response (e.g. "x is now called y"); never stored.
	Notice string `json:"notice,omitempty"`
	// Away is only set in a send response: one line naming the
	// recipients that are not alive; never stored.
	Away string `json:"away,omitempty"`
	// Kind: "" a plain message, "log" a record (send --log-only),
	// "status" a status line sent at once, "digest" batched status lines, "reminder" a
	// needs-reply reminder. Lines and clients without it read it as plain.
	Kind string `json:"kind,omitempty"`
	// ReplyBy: RFC3339 deadline of send --needs-reply.
	ReplyBy string `json:"reply_by,omitempty"`
	// Queued is only set in a send response: the status line waits for the digest, which
	// goes out at about this time (RFC3339); the message has no id yet. Never stored.
	Queued string `json:"queued,omitempty"`
}

// store keeps all messages in memory and appends each new one to the JSONL file.
type store struct {
	path   string
	mu     sync.Mutex
	msgs   []Message
	notify chan struct{} // closed and replaced on every append
}

func openStore(path string) (*store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	s := &store{path: path, notify: make(chan struct{})}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
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
		var m Message
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, n, err)
		}
		if m.Channel == "" {
			m.Channel = chTeam
		}
		s.msgs = append(s.msgs, m)
	}
	return s, sc.Err()
}

func (s *store) append(channel, from, to, text, fileID string, now time.Time) (Message, error) {
	return s.appendReply(channel, from, to, text, fileID, 0, now)
}

// get returns the message with the given id (ids grow by one per append, so binary search).
func (s *store) get(id int64) (Message, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i, ok := slices.BinarySearchFunc(s.msgs, id, func(m Message, id int64) int {
		switch {
		case m.ID < id:
			return -1
		case m.ID > id:
			return 1
		}
		return 0
	})
	if !ok {
		return Message{}, false
	}
	return s.msgs[i], true
}

func (s *store) appendReply(channel, from, to, text, fileID string, replyTo int64, now time.Time) (Message, error) {
	return s.appendMsg(Message{Channel: channel, From: from, To: to, Text: text, FileID: fileID, ReplyTo: replyTo}, now)
}

// appendMsg stores m with the next id and the time now (m.ID and m.TS are set here; the
// response-only fields Notice, Away and Queued are dropped).
func (s *store) appendMsg(m Message, now time.Time) (Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var id int64 = 1
	if len(s.msgs) > 0 {
		id = s.msgs[len(s.msgs)-1].ID + 1
	}
	m.ID, m.TS = id, now.In(jst).Format(time.RFC3339)
	m.Notice, m.Away, m.Queued = "", "", ""
	b, err := json.Marshal(m)
	if err != nil {
		return m, err
	}
	f, err := os.OpenFile(s.path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return m, err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		f.Close()
		return m, err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return m, err
	}
	if err := f.Close(); err != nil {
		return m, err
	}
	s.msgs = append(s.msgs, m)
	close(s.notify)
	s.notify = make(chan struct{})
	return m, nil
}

// inbox returns messages after since in the given channels that member may read (to member,
// or to all from someone else), and a channel that is closed on the next append.
// From and to are compared and returned under the current names (nt), so mail to an old
// name reaches the renamed member and old lines show the new name.
func (s *store) inbox(member string, since int64, channels []string, nt nameTable) ([]Message, <-chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Message{}
	for _, m := range s.msgs {
		if m.ID <= since || !slices.Contains(channels, m.Channel) {
			continue
		}
		m = nt.showMsg(m)
		if m.From == member && m.To == allTarget {
			continue
		}
		if m.To == member || m.To == allTarget {
			out = append(out, m)
		}
	}
	return out, s.notify
}

// limiter is a token bucket per key (member or source address).
type limiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	burst   float64
	perSec  float64
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newLimiter(burst int, perMin float64) *limiter {
	return &limiter{buckets: map[string]*bucket{}, burst: float64(burst), perSec: perMin / 60}
}

func (l *limiter) allow(key string, now time.Time) bool { return l.allowN(key, 1, now) }

func (l *limiter) allowN(key string, n float64, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}
	b.tokens = min(l.burst, b.tokens+now.Sub(b.last).Seconds()*l.perSec)
	b.last = now
	if b.tokens < n {
		return false
	}
	b.tokens -= n
	return true
}

// auditLog appends one JSON line per event: who, when, what, channel, size, result.
// It never holds message text, file contents or tokens.
type auditLog struct {
	path string
	mu   sync.Mutex
}

type auditEntry struct {
	TS      string `json:"ts"`
	Member  string `json:"member,omitempty"`
	Remote  string `json:"remote,omitempty"`
	Action  string `json:"action"`
	Channel string `json:"channel,omitempty"`
	Size    int64  `json:"size,omitempty"`
	Status  int    `json:"status,omitempty"`
	Target  string `json:"target,omitempty"` // recipient member or file id
}

func (a *auditLog) write(e auditEntry) {
	if a == nil {
		return
	}
	if e.TS == "" {
		e.TS = time.Now().In(jst).Format(time.RFC3339)
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false) // keep "old->new" readable
	enc.Encode(e)
	b := bytes.TrimRight(buf.Bytes(), "\n")
	a.mu.Lock()
	defer a.mu.Unlock()
	f, err := os.OpenFile(a.path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		fmt.Fprintln(os.Stderr, "a2: audit:", err)
		return
	}
	f.Write(append(b, '\n'))
	f.Close()
}

// auditLocal records a token change made on the command line.
func auditLocal(action, member string, target ...string) {
	if err := os.MkdirAll(stateDir(), 0o700); err != nil {
		return
	}
	e := auditEntry{Member: member, Remote: "local-cli", Action: action}
	if len(target) > 0 {
		e.Target = target[0]
	}
	(&auditLog{path: filepath.Join(stateDir(), "audit.jsonl")}).write(e)
}

type server struct {
	members   []string
	tokens    *tokenStore
	store     *store
	files     *fileStore
	deny      *denyList
	audit     *auditLog
	limit     *limiter       // messages and files
	reqLimit  *limiter       // any authenticated request
	upLimit   *limiter       // uploaded bytes
	failLimit *limiter       // bad tokens per source address
	allow     []netip.Prefix // empty = any source
	now       func() time.Time
	// viewer-only browser view (view.go)
	view       *viewState
	viewAllow  []netip.Prefix // sources that may open /view (empty = any allowed source)
	viewOrigin string         // the LAN browser origin accepted on /view*, e.g. https://192.0.2.10:18765
	viewLocal  []string       // loopback origins also accepted on /view*, from loopback peers only (viewlocal.go)
	// distribution and self-update (dist.go, selfupdate.go)
	dist  *distCache
	build buildInfo
	quit  chan struct{} // closed when the server is going down (wakes long-polls)
	// WebSocket push (ws.go)
	ws         *wsHub
	wsPing     time.Duration // ping interval (wsPingEvery; tests shorten it)
	wsPongWait time.Duration // close a socket silent this long (wsPongWait)
	restarting atomic.Bool   // set before quit closes for a self-update (close code 1012)
	// delivery receipts (receipts.go)
	deliv *deliveryLog
	// liveness alerts to a webhook (liveness.go)
	live *liveness
	// undelivered notice to the sender (sendcheck.go); 0 / nil = off
	undelivAfter time.Duration
	notices      *noticeLog
	undelivSkip  map[string]bool // members left out of undelivered notices (--undelivered-skip)
	// votes (vote.go)
	votes      *voteLog
	voteMinYes int // --vote-min-yes, fixed into each vote when it opens
	// status digest and needs-reply reminders (digest.go, needsreply.go, send-option)
	digest  *digestQueue
	replies *replyLog
}

func (sv *server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("POST /send", sv.auth(sv.handleSend))
	mux.HandleFunc("GET /inbox", sv.auth(sv.handleInbox))
	mux.HandleFunc("GET /members", sv.auth(sv.handleMembers))
	mux.HandleFunc("GET /receipts/{id}", sv.auth(sv.handleReceipts))
	mux.HandleFunc("POST /files", sv.auth(sv.handlePutFile))
	mux.HandleFunc("GET /files", sv.auth(sv.handleListFiles))
	mux.HandleFunc("GET /files/{id}", sv.auth(sv.handleGetFile))
	mux.HandleFunc("GET /ws", sv.authOrigin(sv.handleWS, true))
	mux.HandleFunc("POST /mcp", sv.auth(sv.handleMCP))
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Allow", "POST")
		jsonError(w, http.StatusMethodNotAllowed, "only POST (no SSE stream)")
	})
	sv.voteRoutes(mux)
	sv.distRoutes(mux)
	sv.viewRoutes(mux)
	return sv.sourceCheck(secureHeaders(viewLocalOnly(mux)))
}

func secureHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Cache-Control", "no-store")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", "default-src 'none'; sandbox")
		next.ServeHTTP(w, r)
	})
}

func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (sv *server) sourceCheck(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(sv.allow) > 0 {
			ip, perr := netip.ParseAddr(remoteIP(r))
			if perr != nil || !slices.ContainsFunc(sv.allow, func(p netip.Prefix) bool { return p.Contains(ip.Unmap()) }) {
				jsonError(w, http.StatusForbidden, "source address not allowed")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

type ctxKey struct{}

func (sv *server) auth(next http.HandlerFunc) http.HandlerFunc { return sv.authOrigin(next, false) }

// bearerToken is the token of the Authorization header ("" when missing). Tokens are
// only read from this header, never from the URL.
func bearerToken(r *http.Request) string {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return ""
	}
	return strings.TrimSpace(tok)
}

// authOrigin checks the bearer token. sameOrigin lets a browser in from the server's own
// origin (GET /ws); every other agent route refuses any Origin.
func (sv *server) authOrigin(next http.HandlerFunc, sameOrigin bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		now := sv.now()
		ip := remoteIP(r)
		// Browsers send Origin; our clients do not. Refusing it blocks DNS rebinding and
		// cross-site requests.
		if o := r.Header.Get("Origin"); o != "" && (!sameOrigin || sv.viewOrigin == "" || o != sv.viewOrigin) {
			jsonError(w, http.StatusForbidden, "browser requests not allowed")
			return
		}
		member := ""
		if tok := bearerToken(r); tok != "" {
			member = sv.tokens.member(tok)
		}
		if member == "" {
			sv.audit.write(auditEntry{Remote: ip, Action: "auth-fail " + r.Method + " " + r.URL.Path, Status: 401})
			if !sv.failLimit.allow(ip, now) {
				w.Header().Set("Retry-After", "60")
				jsonError(w, http.StatusTooManyRequests, "too many bad tokens")
				return
			}
			w.Header().Set("WWW-Authenticate", `Bearer realm="agentsagora"`)
			jsonError(w, http.StatusUnauthorized, "unknown or missing token")
			return
		}
		if !sv.reqLimit.allow(member, now) {
			w.Header().Set("Retry-After", "10")
			jsonError(w, http.StatusTooManyRequests, "rate limit")
			return
		}
		sv.live.touch(member, now) // liveness: any authenticated request is a contact
		next(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, member)))
	}
}

func callerOf(r *http.Request) string { s, _ := r.Context().Value(ctxKey{}).(string); return s }

func jsonError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func writeJSON(w http.ResponseWriter, code int, v any) { writeJSONErr(w, code, v) }

// writeJSONErr is writeJSON that reports a write error (a delivery receipt counts only a
// response that went out).
func writeJSONErr(w http.ResponseWriter, code int, v any) error {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		return err
	}
	if fl, ok := w.(http.Flusher); ok {
		fl.Flush()
	}
	return nil
}

// writeErr sends an httpErr with its code, anything else as 500 (details only in the server log).
func writeErr(w http.ResponseWriter, err error) {
	var he *httpErr
	if errors.As(err, &he) {
		if he.code == http.StatusTooManyRequests {
			w.Header().Set("Retry-After", "10")
		}
		jsonError(w, he.code, he.msg)
		return
	}
	fmt.Fprintln(os.Stderr, "a2:", err)
	jsonError(w, http.StatusInternalServerError, "internal error")
}

func statusOf(err error) int {
	var he *httpErr
	switch {
	case err == nil:
		return 200
	case errors.As(err, &he):
		return he.code
	}
	return 500
}

// --- operations shared by the HTTP API and the MCP tools ---

// resolveChannel picks the channel for a write: the given one, or for an old client that
// sends none, the member's first channel (team for full members, general otherwise).
func (sv *server) resolveChannel(member, channel string) (string, error) {
	if channel == "" {
		chs := channelsOf(member)
		if len(chs) == 0 {
			return "", errCode(http.StatusForbidden, "no channels")
		}
		return chs[0], nil
	}
	if !slices.Contains(allChannels, channel) {
		return "", errCode(http.StatusBadRequest, "unknown channel %q (channels: team, general)", channel)
	}
	if !canUse(member, channel) {
		return "", errCode(http.StatusForbidden, "channel %s not allowed for %s", channel, member)
	}
	return channel, nil
}

// readChannels is the channel list for a read: one allowed channel, or all of the member's.
func (sv *server) readChannels(member, channel string) ([]string, error) {
	if channel == "" {
		return channelsOf(member), nil
	}
	c, err := sv.resolveChannel(member, channel)
	if err != nil {
		return nil, err
	}
	return []string{c}, nil
}

// checkRecipient returns the recipient under its current name; an old name of a renamed
// member is delivered to the new name with a notice for the sender.
func (sv *server) checkRecipient(channel, to string) (string, string, error) {
	if to == allTarget {
		return to, "", nil
	}
	nt := currentNames()
	notice := ""
	if cur := nt.canon(to); cur != to {
		notice = fmt.Sprintf("%s is now called %s; please use the new name", to, cur)
		to = cur
	}
	if !sv.isMember(nt, to) {
		return to, "", errCode(http.StatusBadRequest, "unknown recipient %s", strconv.Quote(to))
	}
	if !slices.Contains(nt.channels(to), channel) {
		return to, "", errCode(http.StatusBadRequest, "recipient %s is not in channel %s", to, channel)
	}
	return to, notice, nil
}

func (sv *server) lint(channel string, texts ...string) error {
	if channel != chGeneral {
		return nil
	}
	if sv.deny == nil {
		return errCode(http.StatusServiceUnavailable, "general deny list not loaded")
	}
	if err := sv.deny.check(texts...); err != nil {
		return errCode(http.StatusUnprocessableEntity, "%s", err.Error())
	}
	return nil
}

func (sv *server) send(member, channel, to, text string, replyTo int64) (Message, error) {
	ch, err := sv.resolveChannel(member, channel)
	if err != nil {
		return Message{}, err
	}
	return sv.sendOn(member, ch, to, text, replyTo)
}

// sendWith is send with the send-option options (kind, needs-reply).
func (sv *server) sendWith(member, channel, to, text string, replyTo int64, o sendOpts) (Message, error) {
	ch, err := sv.resolveChannel(member, channel)
	if err != nil {
		return Message{}, err
	}
	return sv.sendOnOpts(member, ch, to, text, replyTo, o, text)
}

// threadRoot checks reply_to for a message by member on channel ch and returns the root id
// to store (0 = no thread). The parent must be in the same channel and visible to the
// sender (to all, to the sender, or from the sender; the viewer sees everything). A parent
// the sender cannot see gets the same answer as a missing one, so ids do not leak.
func (sv *server) threadRoot(member, ch string, replyTo int64) (int64, error) {
	if replyTo == 0 {
		return 0, nil
	}
	if replyTo < 0 {
		return 0, errCode(http.StatusBadRequest, "bad reply_to")
	}
	p, ok := sv.store.get(replyTo)
	p = currentNames().showMsg(p)
	if !ok || (member != viewerName && p.To != allTarget && p.To != member && p.From != member) {
		return 0, errCode(http.StatusBadRequest, "unknown reply_to #%d", replyTo)
	}
	if p.Channel != ch {
		return 0, errCode(http.StatusBadRequest, "reply_to #%d is in channel %s, not %s", replyTo, p.Channel, ch)
	}
	if p.ReplyTo != 0 {
		return p.ReplyTo, nil
	}
	return p.ID, nil
}

// sendOn writes a text message on a channel already checked for the sender.
func (sv *server) sendOn(member, ch, to, text string, replyTo int64) (Message, error) {
	return sv.sendOnLint(member, ch, to, text, replyTo, text)
}

// sendOnLint is sendOn with the general lint run on lintTexts instead of the text (a vote
// message, vote.go: only the question and options are user text; the fixed frame has
// "#N", which the deny list refuses).
func (sv *server) sendOnLint(member, ch, to, text string, replyTo int64, lintTexts ...string) (Message, error) {
	return sv.sendOnOpts(member, ch, to, text, replyTo, sendOpts{}, lintTexts...)
}

// sendOnOpts is sendOnLint with the send-option options: a kind (log or status; a status
// line may wait for the digest, digest.go) and a needs-reply deadline (needsreply.go).
func (sv *server) sendOnOpts(member, ch, to, text string, replyTo int64, o sendOpts, lintTexts ...string) (Message, error) {
	if err := o.check(); err != nil {
		return Message{}, err
	}
	if len(text) > maxTextBytes {
		return Message{}, errCode(http.StatusRequestEntityTooLarge, "text over %d bytes", maxTextBytes)
	}
	if strings.TrimSpace(text) == "" {
		return Message{}, errCode(http.StatusBadRequest, "empty text")
	}
	if to == "" {
		to = allTarget
	}
	to, notice, err := sv.checkRecipient(ch, to)
	if err != nil {
		return Message{}, err
	}
	if o.within > 0 && to == allTarget {
		return Message{}, errCode(http.StatusBadRequest, "needs_reply needs one recipient, not all")
	}
	root, err := sv.threadRoot(member, ch, replyTo)
	if err != nil {
		return Message{}, err
	}
	if err := sv.lint(ch, lintTexts...); err != nil {
		return Message{}, err
	}
	now := sv.now()
	if !sv.limit.allow(member, now) {
		return Message{}, errCode(http.StatusTooManyRequests, "rate limit")
	}
	nm := Message{Channel: ch, From: member, To: to, Text: text, ReplyTo: root, Kind: o.kind}
	if o.kind == kindStatus {
		if due, queued := sv.digestStatus(nm, now); queued {
			nm.Queued, nm.Notice = due.In(jst).Format(time.RFC3339), notice
			return nm, nil
		}
	}
	if o.within > 0 {
		nm.ReplyBy = now.Add(o.within).In(jst).Format(time.RFC3339)
	}
	m, err := sv.store.appendMsg(nm, now)
	if err != nil {
		return Message{}, fmt.Errorf("append: %w", err)
	}
	m.Notice = notice
	m.Away = sv.awayNotice(m, currentNames(), sv.now()) // recipients not alive
	return m, nil
}

func (sv *server) putFile(member, channel, to, name, mimeType string, data []byte) (FileMeta, Message, error) {
	ch, err := sv.resolveChannel(member, channel)
	if err != nil {
		return FileMeta{}, Message{}, err
	}
	if to == "" {
		to = allTarget
	}
	to, notice, err := sv.checkRecipient(ch, to)
	if err != nil {
		return FileMeta{}, Message{}, err
	}
	if err := checkName(name); err != nil {
		return FileMeta{}, Message{}, err
	}
	if int64(len(data)) > sv.files.maxFile {
		return FileMeta{}, Message{}, errCode(http.StatusRequestEntityTooLarge, "file over %d bytes", sv.files.maxFile)
	}
	texts := []string{name, mimeType}
	if base, _, _ := mime.ParseMediaType(mimeType); isTextMIME(base) && isText(data) {
		texts = append(texts, string(data))
	}
	if err := sv.lint(ch, texts...); err != nil {
		return FileMeta{}, Message{}, err
	}
	now := sv.now()
	if !sv.limit.allow(member, now) {
		return FileMeta{}, Message{}, errCode(http.StatusTooManyRequests, "rate limit")
	}
	if !sv.upLimit.allowN(member, float64(len(data)), now) {
		return FileMeta{}, Message{}, errCode(http.StatusTooManyRequests, "upload volume limit (200 MB per hour)")
	}
	fm, err := sv.files.put(member, ch, name, mimeType, data, now)
	if err != nil {
		return FileMeta{}, Message{}, err
	}
	text := fmt.Sprintf("[file] %s (%s, %d bytes) id=%s", fm.Name, fm.MIME, fm.Size, fm.ID)
	m, err := sv.store.append(ch, member, to, text, fm.ID, now)
	if err != nil {
		return fm, Message{}, fmt.Errorf("append: %w", err)
	}
	m.Notice = notice
	return fm, m, nil
}

func (sv *server) getFile(member, id string) (FileMeta, []byte, error) {
	fm, ok := sv.files.meta(id)
	if !ok {
		return FileMeta{}, nil, errCode(http.StatusNotFound, "no such file (bad id, or deleted to stay under the 2 GB total)")
	}
	if !canUse(member, fm.Channel) {
		return FileMeta{}, nil, errCode(http.StatusForbidden, "channel %s not allowed for %s", fm.Channel, member)
	}
	data, err := sv.files.read(id)
	if err != nil {
		return FileMeta{}, nil, errCode(http.StatusNotFound, "file deleted")
	}
	return fm, data, nil
}

func (sv *server) listFiles(member, channel string) ([]FileMeta, error) {
	chs, err := sv.readChannels(member, channel)
	if err != nil {
		return nil, err
	}
	nt := currentNames()
	files := sv.files.list(chs)
	for i := range files {
		files[i].From = nt.canon(files[i].From)
	}
	return files, nil
}

type memberInfo struct {
	Name     string   `json:"name"`
	Channels []string `json:"channels"`
	HasToken bool     `json:"has_token"`
	// liveness; absent for a member never seen
	Alive    *bool  `json:"alive,omitempty"`
	LastSeen string `json:"last_seen,omitempty"` // RFC3339 (JST) of the last contact
}

func (sv *server) memberList() []memberInfo {
	has := sv.tokens.hasToken()
	nt := currentNames()
	out := []memberInfo{}
	now := sv.now()
	for _, b := range sv.members {
		m := nt.current(b)
		mi := memberInfo{Name: m, Channels: nt.channels(m), HasToken: has[m]}
		if lv := sv.liveOf(nt, b, now); lv.seen {
			mi.Alive, mi.LastSeen = &lv.alive, lv.last.In(jst).Format(time.RFC3339)
		}
		out = append(out, mi)
	}
	return out
}

// --- HTTP handlers ---

type sendReq struct {
	Channel string `json:"channel,omitempty"`
	To      string `json:"to"`
	Text    string `json:"text"`
	ReplyTo int64  `json:"reply_to,omitempty"` // optional thread root (or any message in it)
	// send-option (optional; older servers ignore them): kind "log" or "status", and the
	// needs-reply time in seconds (one reminder to both sides if no reply comes in time).
	Kind          string `json:"kind,omitempty"`
	NeedsReplySec int64  `json:"needs_reply_sec,omitempty"`
}

func (sv *server) handleSend(w http.ResponseWriter, r *http.Request) {
	from := callerOf(r)
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	var req sendReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			jsonError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("body over %d bytes", maxBodyBytes))
			return
		}
		jsonError(w, http.StatusBadRequest, "bad JSON body")
		return
	}
	if req.To == "" {
		jsonError(w, http.StatusBadRequest, "unknown recipient \"\"")
		return
	}
	within := time.Duration(-1) // out of range: sendOpts.check refuses it
	if req.NeedsReplySec >= 0 && req.NeedsReplySec <= int64(maxNeedsReply/time.Second) {
		within = time.Duration(req.NeedsReplySec) * time.Second
	}
	m, err := sv.sendWith(from, req.Channel, req.To, req.Text, req.ReplyTo, sendOpts{kind: req.Kind, within: within})
	sv.audit.write(auditEntry{Member: from, Remote: remoteIP(r), Action: "send", Channel: m.Channel + orReq(m.Channel, req.Channel), Size: int64(len(req.Text)), Status: statusOf(err), Target: req.To})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

// orReq fills the audit channel with the requested one when the operation failed early.
func orReq(got, req string) string {
	if got != "" {
		return ""
	}
	return req
}

func (sv *server) handleInbox(w http.ResponseWriter, r *http.Request) {
	member := callerOf(r)
	q := r.URL.Query()
	var since int64
	if v := q.Get("since"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			jsonError(w, http.StatusBadRequest, "bad since")
			return
		}
		since = n
	}
	wait := 0
	if v := q.Get("wait"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			jsonError(w, http.StatusBadRequest, "bad wait")
			return
		}
		wait = min(n, maxWait)
	}
	chs, err := sv.readChannels(member, q.Get("channel"))
	var filt inboxFilter
	if err == nil {
		// send-check filters (sendcheck.go): they only narrow this member's inbox
		filt, err = sv.parseFilter(q.Get("since_time"), q.Get("from"), q.Get("thread"), currentNames())
	}
	if err != nil {
		sv.audit.write(auditEntry{Member: member, Remote: remoteIP(r), Action: "inbox", Channel: q.Get("channel"), Status: statusOf(err)})
		writeErr(w, err)
		return
	}
	body := func(msgs []Message) map[string]any {
		out := map[string]any{"member": member, "messages": msgs}
		if !filt.empty() {
			out["filtered"] = true // a client asking for filters checks this (older servers ignore them)
		}
		return out
	}
	msgs, ch := sv.inboxFiltered(member, since, chs, currentNames(), filt)
	if len(msgs) == 0 && wait > 0 {
		t := time.NewTimer(time.Duration(wait) * time.Second)
		defer t.Stop()
		for len(msgs) == 0 {
			select {
			case <-ch:
				msgs, ch = sv.inboxFiltered(member, since, chs, currentNames(), filt)
			case <-t.C:
				writeJSON(w, http.StatusOK, body(msgs))
				return
			case <-r.Context().Done():
				return
			case <-sv.quit:
				writeJSON(w, http.StatusOK, body(msgs))
				return
			}
		}
	}
	// record the delivery only when the response body went out without a write error
	if writeJSONErr(w, http.StatusOK, body(msgs)) == nil {
		sv.delivered(member, msgs)
	}
	sv.live.touch(member, sv.now()) // a long poll ends here: still a contact
}

func (sv *server) handleMembers(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"you": callerOf(r), "your_channels": channelsOf(callerOf(r)), "members": sv.memberList()})
}

// handlePutFile: POST /files?channel=C&name=N[&to=M], body = raw bytes, Content-Type = MIME.
func (sv *server) handlePutFile(w http.ResponseWriter, r *http.Request) {
	member := callerOf(r)
	q := r.URL.Query()
	r.Body = http.MaxBytesReader(w, r.Body, maxFileBody)
	data, err := io.ReadAll(r.Body)
	if err != nil {
		jsonError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("file over %d bytes", maxFileBytes))
		return
	}
	fm, m, err := sv.putFile(member, q.Get("channel"), q.Get("to"), q.Get("name"), r.Header.Get("Content-Type"), data)
	sv.audit.write(auditEntry{Member: member, Remote: remoteIP(r), Action: "put_file", Channel: q.Get("channel"), Size: int64(len(data)), Status: statusOf(err), Target: fm.ID})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"file": fm, "message": m})
}

func (sv *server) handleListFiles(w http.ResponseWriter, r *http.Request) {
	files, err := sv.listFiles(callerOf(r), r.URL.Query().Get("channel"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"files": files})
}

func (sv *server) handleGetFile(w http.ResponseWriter, r *http.Request) {
	member := callerOf(r)
	id := r.PathValue("id")
	fm, data, err := sv.getFile(member, id)
	target := id
	if !fileIDRe.MatchString(id) {
		target = "(bad id)"
	}
	sv.audit.write(auditEntry{Member: member, Remote: remoteIP(r), Action: "get_file", Channel: fm.Channel, Size: int64(len(data)), Status: statusOf(err), Target: target})
	if err != nil {
		writeErr(w, err)
		return
	}
	h := w.Header()
	h.Set("Content-Type", fm.MIME)
	h.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": fm.Name}))
	h.Set("Content-Length", strconv.Itoa(len(data)))
	h.Set("X-AgentsAgora-Channel", fm.Channel)
	h.Set("X-AgentsAgora-SHA256", fm.SHA256)
	w.WriteHeader(http.StatusOK)
	w.Write(data)
}

// --- setup ---

func parseMembers(s string) []string {
	if strings.TrimSpace(s) == "" {
		return DefaultMembers
	}
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" && p != allTarget {
			out = append(out, p)
		}
	}
	return out
}

func parseAllow(s string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		pr, err := netip.ParsePrefix(p)
		if err != nil {
			return nil, fmt.Errorf("--allow %q: %w", p, err)
		}
		out = append(out, pr)
	}
	return out, nil
}

// newServer opens the stores. denyPath "" leaves the general lint unloaded (general writes
// then fail with 503); cmdServe always passes a path and refuses to start without the file.
func newServer(members []string, allow []netip.Prefix, denyPath ...string) (*server, error) {
	for _, m := range members {
		if m == viewerName || m == systemSender {
			return nil, fmt.Errorf("%s is a reserved name, not a member", m)
		}
		if _, ok := memberChannels[m]; !ok {
			return nil, fmt.Errorf("member %s is not in the member table", m)
		}
	}
	ts, err := newTokenStore(tokensPath(), members)
	if err != nil {
		return nil, err
	}
	st, err := openStore(filepath.Join(stateDir(), "messages.jsonl"))
	if err != nil {
		return nil, err
	}
	fs, err := openFileStore(filepath.Join(stateDir(), "files"))
	if err != nil {
		return nil, err
	}
	sv := &server{members: members, tokens: ts, store: st, files: fs,
		audit:     &auditLog{path: filepath.Join(stateDir(), "audit.jsonl")},
		limit:     newLimiter(rateBurst, ratePerMin),
		reqLimit:  newLimiter(reqBurst, reqPerMin),
		upLimit:   newLimiter(uploadBurst, uploadPerMin),
		failLimit: newLimiter(authFailBurst, authFailPerMin),
		allow:     allow, now: time.Now, quit: make(chan struct{}),
		ws: newWSHub(), wsPing: wsPingEvery, wsPongWait: wsPongWait}
	if sv.deliv, err = openDeliveryLog(filepath.Join(stateDir(), deliveriesFile), st.lastID()); err != nil {
		return nil, err
	}
	if sv.votes, err = openVoteLog(filepath.Join(stateDir(), votesFile)); err != nil {
		return nil, err
	}
	sv.votes.poke, sv.voteMinYes = sv.deliv.poke, defaultVoteMinYes // a vote change refreshes the viewer UI like a receipt
	if sv.digest, err = openDigest(filepath.Join(stateDir(), digestFile), defaultDigestMins*time.Minute); err != nil {
		return nil, err
	}
	if sv.replies, err = openReplyLog(filepath.Join(stateDir(), replyRemindFile)); err != nil {
		return nil, err
	}
	sv.live = newLiveness(filepath.Join(stateDir(), livenessFile), filepath.Join(configDir(), webhookFile), sv.now())
	sv.build, _, _ = selfInfo()
	sv.build.Started = time.Now().In(jst).Format(time.RFC3339)
	if sv.view, err = newViewState(viewSessionsPath(), time.Now()); err != nil {
		return nil, err
	}
	if len(denyPath) > 0 && denyPath[0] != "" {
		d, err := loadDenyList(denyPath[0])
		if err != nil {
			return nil, err
		}
		sv.deny = d
	}
	return sv, nil
}

// checkBind refuses wildcard addresses and addresses on VPN or tunnel interfaces.
func checkBind(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return fmt.Errorf("--addr host must be an IP address, got %q", host)
	}
	if ip.IsUnspecified() {
		return fmt.Errorf("--addr %s listens on every interface; give the address of the interface to listen on, e.g. 192.0.2.10", host)
	}
	ifs, err := net.Interfaces()
	if err != nil {
		return nil
	}
	for _, ifc := range ifs {
		name := strings.ToLower(ifc.Name)
		if !slices.ContainsFunc([]string{"tun", "tap", "wg", "tailscale", "utun", "ppp", "zt"}, func(p string) bool { return strings.HasPrefix(name, p) }) {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			if pn, ok := a.(*net.IPNet); ok {
				if aip, ok := netip.AddrFromSlice(pn.IP); ok && aip.Unmap() == ip.Unmap() {
					return fmt.Errorf("--addr %s is on %s (VPN/tunnel); listen on the LAN address only", host, ifc.Name)
				}
			}
		}
	}
	return nil
}

// loadServerCert loads the server certificate and refuses a key file readable by others.
func loadServerCert(certPath, keyPath string) (tls.Certificate, error) {
	st, err := os.Stat(keyPath)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("TLS key: %w (run: a2 tls init)", err)
	}
	if st.Mode().Perm()&0o077 != 0 {
		return tls.Certificate{}, fmt.Errorf("TLS key %s must be mode 0600", keyPath)
	}
	return tls.LoadX509KeyPair(certPath, keyPath)
}

func serverTLSConfig(cert tls.Certificate) *tls.Config {
	return &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13}
}

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	addr := fs.String("addr", "127.0.0.1:18765", "listen address IP:PORT (no wildcard, no VPN interface)")
	allowS := fs.String("allow", "127.0.0.0/8", "source CIDRs allowed, comma separated (empty = any)")
	membersS := fs.String("members", "", "members, comma separated (default: the whole member table)")
	denyS := fs.String("general-deny", filepath.Join(configDir(), "general-deny.txt"), "general channel deny list (required; see examples/general-deny.txt)")
	certS := fs.String("tls-cert", "", "server certificate (default <config dir>/tls/server.pem)")
	keyS := fs.String("tls-key", "", "server key (default <config dir>/tls/server-key.pem)")
	viewAllowS := fs.String("view-allow", viewDefaultAllow, "source CIDRs that may open the viewer UI (/view), comma separated")
	viewOriginS := fs.String("view-origin", "", "browser origin accepted on /view (default https://<addr>)")
	viewLocalS := fs.String("view-local", "auto", "loopback address that also serves /view, for https://localhost:<port> on this machine (auto = 127.0.0.1:<port of --addr>, off = none)")
	distDirS := fs.String("dist-dir", "", "dir of the cross-built clients served on /dist (default: the dir of this binary)")
	noSelfUpdate := fs.Bool("no-self-update", false, "do not restart when the binary on disk changes")
	undelivMins := fs.Int("undelivered-after", defaultUndeliveredMins, "minutes after which the sender of a message not yet delivered to every recipient gets one a2-system notice (0 = off)")
	undelivSkipS := fs.String("undelivered-skip", "", "member names (comma separated) left out of undelivered notices: a message only to them gets none; with other recipients they are not counted or named")
	voteMinYes := fs.Int("vote-min-yes", defaultVoteMinYes, "yes votes a vote needs to pass (and yes > no; options: votes of the top option)")
	digestMins := fs.Int("status-digest", defaultDigestMins, "minutes a status line (send --status) waits to go out with the other status lines of its sender as one digest message (0 = off: at once)")
	if err := fs.Parse(args); err != nil {
		return usageError(err.Error())
	}
	if *undelivMins < 0 {
		return usageError("--undelivered-after must be 0 (off) or more minutes")
	}
	if *voteMinYes < 1 {
		return usageError("--vote-min-yes must be 1 or more")
	}
	if *digestMins < 0 {
		return usageError("--status-digest must be 0 (off) or more minutes")
	}
	allow, err := parseAllow(*allowS)
	if err != nil {
		return usageError(err.Error())
	}
	viewAllow, err := parseAllow(*viewAllowS)
	if err != nil {
		return usageError(strings.Replace(err.Error(), "--allow", "--view-allow", 1))
	}
	if len(viewAllow) == 0 {
		return usageError("--view-allow needs at least one CIDR")
	}
	viewOrigin := strings.TrimRight(*viewOriginS, "/")
	if viewOrigin == "" {
		viewOrigin = "https://" + *addr
	}
	if !strings.HasPrefix(viewOrigin, "https://") {
		return usageError("--view-origin must start with https://")
	}
	if err := checkBind(*addr); err != nil {
		return err
	}
	localAddr, err := viewLocalAddr(*viewLocalS, *addr)
	if err != nil {
		return usageError(err.Error())
	}
	if *certS == "" {
		*certS = filepath.Join(tlsDir(), "server.pem")
	}
	if *keyS == "" {
		*keyS = filepath.Join(tlsDir(), "server-key.pem")
	}
	cert, err := loadServerCert(expandHome(*certS), expandHome(*keyS))
	if err != nil {
		return err
	}
	sv, err := newServer(parseMembers(*membersS), allow, expandHome(*denyS))
	if err != nil {
		return err
	}
	sv.viewAllow, sv.viewOrigin = viewAllow, viewOrigin
	sv.voteMinYes = *voteMinYes
	sv.digest.every = time.Duration(*digestMins) * time.Minute
	for _, n := range strings.Split(*undelivSkipS, ",") {
		if n = strings.TrimSpace(n); n != "" {
			if sv.undelivSkip == nil {
				sv.undelivSkip = map[string]bool{}
			}
			sv.undelivSkip[currentNames().canon(n)] = true
		}
	}
	if err := sv.enableUndelivered(time.Duration(*undelivMins) * time.Minute); err != nil {
		return err
	}
	if *undelivMins == 0 {
		fmt.Fprintln(os.Stderr, "a2: undelivered notices off (--undelivered-after 0)")
	}
	distDir := expandHome(*distDirS)
	if distDir == "" {
		if _, _, p := selfInfo(); p != "" {
			distDir = filepath.Dir(p)
		}
	}
	sv.dist = newDistCache(distDir)
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		return err
	}
	// Loopback view listener (viewlocal.go). A failure here leaves the LAN server running.
	var lnLocal net.Listener
	switch {
	case localAddr == "":
	case localAddr == ln.Addr().String():
		sv.viewLocal = viewLocalOrigins(localAddr)
	default:
		if l, lerr := net.Listen("tcp", localAddr); lerr != nil {
			fmt.Fprintf(os.Stderr, "a2: loopback view listener %s not started: %v\n", localAddr, lerr)
		} else {
			lnLocal = l
			sv.viewLocal = viewLocalOrigins(l.Addr().String())
		}
	}
	hs := &http.Server{
		Handler:           sv.handler(),
		TLSConfig:         serverTLSConfig(cert),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       2 * time.Minute, // a 20 MB upload on the LAN takes seconds
		WriteTimeout:      (maxWait + 30) * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    16 << 10,
		ErrorLog:          nil,
	}
	if lnLocal != nil {
		hs.ConnContext = viewLocalConnContext(lnLocal.Addr())
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// self-update: watch the binary on disk (selfupdate.go)
	updated := make(chan struct{})
	if !*noSelfUpdate {
		if _, sha, p := selfInfo(); p != "" && sha != "" {
			u := newSelfUpdater(p, sha, time.Now)
			go func() {
				if sv.watchSelf(ctx, u, selfUpdatePoll) {
					close(updated)
				}
			}()
		}
	}
	go sv.liveRun(sv.quit) // liveness alerts to a webhook (liveness.go)
	shutDone := make(chan struct{})
	go func() {
		defer close(shutDone)
		select {
		case <-ctx.Done():
		case <-updated:
			sv.restarting.Store(true) // sockets get close code 1012 (service restart)
		}
		close(sv.quit) // long-polls answer and sockets get a close frame now
		sctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		defer cancel()
		if err := hs.Shutdown(sctx); err != nil {
			hs.Close()
		}
		sv.ws.wait(shutdownGrace) // hijacked sockets are not tracked by Shutdown
	}()
	fmt.Fprintf(os.Stderr, "a2: serving HTTPS on %s (members %s, state %s, %s)\n", ln.Addr(), strings.Join(sv.memberNames(currentNames()), ","), stateDir(), versionLine(sv.build))
	if lnLocal != nil {
		fmt.Fprintf(os.Stderr, "a2: viewer UI also on %s/view (loopback peers only)\n", sv.viewLocal[0])
		go func() {
			if err := hs.ServeTLS(lnLocal, "", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
				fmt.Fprintf(os.Stderr, "a2: loopback view listener stopped: %v\n", err)
			}
		}()
	}
	if err := hs.ServeTLS(ln, "", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	<-shutDone
	select {
	case <-updated:
		return exitCodeError{selfUpdateExit, "exiting for the new binary (systemd restarts it)"}
	default:
	}
	return nil
}
