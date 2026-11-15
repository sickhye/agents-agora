package main

// a2 watch: one line per message on stdout (formatLine), pushed over GET /ws (ws.go).
// It keeps its own resume cursor (watchCursor). With no cursor file yet it prints one
// backlog summary line and starts at the newest message (nothing old is printed in full);
// --from-start prints the history from id 1 instead.
// The socket reconnects with backoff 1 s -> 30 s and resumes from the cursor file, so a
// drop loses nothing and prints nothing twice. Only a server without /ws (404, an older
// build) makes it fall back to the /inbox long-poll; it tries /ws again every 10 minutes.

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

var errNoWS = errors.New("server has no /ws (older build)")

// dialWS opens GET /ws with the bearer token in the header and returns the socket.
func (c *client) dialWS(ctx context.Context, channel string) (*wsConnFrames, error) {
	u, err := url.Parse(c.base)
	if err != nil {
		return nil, err
	}
	addr := u.Host
	if u.Port() == "" {
		port := "80"
		if u.Scheme == "https" {
			port = "443"
		}
		addr = net.JoinHostPort(u.Hostname(), port)
	}
	d := &net.Dialer{Timeout: 10 * time.Second}
	var conn net.Conn
	switch u.Scheme {
	case "https":
		var tc *tls.Config
		if tr, ok := c.http.Transport.(*http.Transport); ok && tr.TLSClientConfig != nil {
			tc = tr.TLSClientConfig.Clone()
		} else {
			return nil, errors.New("https URL without a pinned CA")
		}
		tc.NextProtos = []string{"http/1.1"} // WebSocket needs HTTP/1.1, not h2
		if tc.ServerName == "" {
			tc.ServerName = u.Hostname()
		}
		conn, err = (&tls.Dialer{NetDialer: d, Config: tc}).DialContext(ctx, "tcp", addr)
	case "http": // loopback only (httpClientFor checked it)
		conn, err = d.DialContext(ctx, "tcp", addr)
	default:
		return nil, fmt.Errorf("bad server URL scheme %q", u.Scheme)
	}
	if err != nil {
		return nil, err
	}
	raw := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, raw); err != nil {
		conn.Close()
		return nil, err
	}
	key := base64.StdEncoding.EncodeToString(raw)
	target := strings.TrimRight(c.base, "/") + "/ws"
	if channel != "" {
		target += "?" + url.Values{"channel": {channel}}.Encode()
	}
	req, err := http.NewRequest("GET", target, nil)
	if err != nil {
		conn.Close()
		return nil, err
	}
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Sec-WebSocket-Key", key)
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("User-Agent", userAgent())
	conn.SetDeadline(time.Now().Add(15 * time.Second))
	if err := req.Write(conn); err != nil {
		conn.Close()
		return nil, err
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		conn.Close()
		return nil, err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		conn.Close()
		if resp.StatusCode == http.StatusNotFound {
			return nil, errNoWS
		}
		return nil, serverErr(resp, data)
	}
	if resp.Header.Get("Sec-WebSocket-Accept") != wsAcceptKey(key) {
		conn.Close()
		return nil, errors.New("bad websocket handshake answer")
	}
	conn.SetDeadline(time.Time{})
	return &wsConnFrames{c: conn, r: br, client: true, maxIn: wsMaxOutFrame}, nil
}

// watcher prints new messages to out and keeps its own resume cursor file cp
// : one per server, token file (= member) and channel set, in the state dir,
// apart from the a2 inbox cursor inboxCp. A re-run (e.g. after the 30 min monitor cap)
// prints only messages newer than the last one it printed.
type watcher struct {
	c         *client
	channel   string
	wait      int // long-poll seconds (fallback only)
	cp        string
	inboxCp   string // the a2 inbox cursor: moved with every printed line, as before
	out       io.Writer
	errOut    io.Writer
	args      []string // for the re-exec after a self-update
	backoff0  time.Duration
	backoffN  time.Duration
	readWait  time.Duration // reconnect when the server sends nothing (not even a ping) this long
	probe     time.Duration // while on long-poll, try /ws again this often
	last      int64         // newest id printed (or the cursor at start)
	lastErr   string
	fromStart bool // no cursor file: print the history from id 1 instead of starting at the newest
	started   bool // the cursor was checked (once, at the start of run)
	// backlog summary: with no cursor, or more than backlogMax messages after
	// it, start prints one summary line instead of every old message
	backlogMax int
	holdInbox  bool // a summary was printed: keep the a2 inbox cursor, so a2 inbox shows those
	// server restart: after a close frame "server stopping" or 1012, errors stay
	// quiet for restartQuiet and the reconnect prints one line
	downSince    time.Time
	restartQuiet time.Duration
	// liveness stamp: path ("" or "off" = none), interval, clock
	tickPath  string
	tickEvery time.Duration
	tickNow   func() time.Time
}

const defaultBacklogMax = 20

// watchCursor is the watch's own cursor file next to the a2 inbox cursor base.
func watchCursor(base, channel string) string {
	if channel == "" {
		channel = "all"
	}
	return channelCursor(base+"-watch", channel)
}

func newWatcher(c *client, channel string, wait int, args []string) *watcher {
	wt := &watcher{c: c, wait: wait, out: os.Stdout, errOut: os.Stderr, args: args,
		backoff0: time.Second, backoffN: 30 * time.Second, readWait: wsPongWait, probe: 10 * time.Minute,
		backlogMax: defaultBacklogMax, restartQuiet: 2 * time.Minute}
	wt.setChannel(channel)
	return wt
}

// setChannel sets the channel and its two cursor files.
func (wt *watcher) setChannel(channel string) {
	wt.channel = channel
	wt.cp, wt.inboxCp = watchCursor(wt.c.cursor, channel), channelCursor(wt.c.cursor, channel)
	wt.last = readCursor(wt.cp)
}

func (wt *watcher) report(err error) {
	if !wt.downSince.IsZero() && time.Since(wt.downSince) < wt.restartQuiet {
		return // a server restart: one line when it is back (ok)
	}
	if s := err.Error(); s != wt.lastErr {
		fmt.Fprintln(wt.errOut, "[a2] error:", s, "(retrying)")
		wt.lastErr = s
	}
}

func (wt *watcher) ok() {
	if !wt.downSince.IsZero() {
		fmt.Fprintf(wt.errOut, "[a2] server restart, reconnected after %ds\n", int(time.Since(wt.downSince).Round(time.Second)/time.Second))
		wt.downSince, wt.lastErr = time.Time{}, ""
		return
	}
	if wt.lastErr != "" {
		fmt.Fprintln(wt.errOut, "[a2] reconnected")
		wt.lastErr = ""
	}
}

// restartClose: the server closed the socket because it is stopping (systemd restart,
// 1001 "server stopping") or restarting for a new build (1012).
func restartClose(err error) bool {
	var ce *wsCloseError
	return errors.As(err, &ce) && (ce.code == wsCloseRestart || ce.code == wsCloseGoingAway && ce.reason == "server stopping")
}

// show prints m once (ids at or below the last printed one are skipped) and moves the cursors.
func (wt *watcher) show(m Message) error {
	if m.ID <= wt.last {
		return nil
	}
	fmt.Fprintln(wt.out, watchLine(m))
	wt.last = m.ID
	if err := writeCursor(wt.cp, m.ID); err != nil {
		return err
	}
	if !wt.holdInbox && m.ID > readCursor(wt.inboxCp) {
		return writeCursor(wt.inboxCp, m.ID)
	}
	return nil
}

// cursorFile reads cursor p: ok = the file exists; an error other than "missing" is fatal.
func cursorFile(p string) (n int64, ok bool, err error) {
	if _, err := os.Stat(p); errors.Is(err, os.ErrNotExist) {
		return 0, false, nil
	} else if err != nil {
		return 0, false, fatalErr{err}
	}
	return readCursor(p), true, nil
}

// start runs once before the first connection. With --from-start it keeps the cursor as
// it is. Otherwise the base is the watch cursor, or for a first run after an update the
// old shared cursor (the old watch moved it), so nothing is lost or printed twice. It asks
// the server (same /inbox and same rights as any read) what came after the base: with a
// cursor and at most backlogMax messages the socket prints them in full; with no cursor
// at all, or more than backlogMax, it prints one summary line (count per sender, newest
// ids; a2 inbox still has the full text) and moves the watch cursor to the newest.
func (wt *watcher) start(ctx context.Context) error {
	if wt.fromStart {
		wt.started = true
		return nil
	}
	base, have, err := cursorFile(wt.cp)
	if err != nil {
		return err
	}
	if !have {
		if base, have, err = cursorFile(wt.inboxCp); err != nil {
			return err
		}
	}
	r, err := wt.c.inboxCtx(ctx, base, 0, wt.channel)
	if err != nil {
		return err
	}
	ms := r.Messages
	cur := base
	if len(ms) > 0 && (!have || len(ms) > wt.backlogMax) {
		fmt.Fprintln(wt.out, backlogSummary(ms, base, wt.channel))
		for _, m := range ms {
			cur = max(cur, m.ID)
		}
		wt.holdInbox = true
	}
	if err := writeCursor(wt.cp, cur); err != nil {
		return fatalErr{err}
	}
	wt.last = cur
	wt.started = true
	return nil
}

// backlogSummary is one line for the messages ms after cursor base: the count, and per
// sender (most first, at most 6) the count and up to 3 newest ids.
func backlogSummary(ms []Message, base int64, channel string) string {
	type snd struct {
		name string
		ids  []int64
	}
	idx := map[string]int{}
	var ss []snd
	var lo, hi int64
	for _, m := range ms {
		if lo == 0 || m.ID < lo {
			lo = m.ID
		}
		hi = max(hi, m.ID)
		i, ok := idx[m.From]
		if !ok {
			i = len(ss)
			idx[m.From] = i
			ss = append(ss, snd{name: m.From})
		}
		ss[i].ids = append(ss[i].ids, m.ID)
	}
	sort.SliceStable(ss, func(a, b int) bool { return len(ss[a].ids) > len(ss[b].ids) })
	var parts []string
	for i, s := range ss {
		if i == 6 {
			parts = append(parts, fmt.Sprintf("+%d more senders", len(ss)-6))
			break
		}
		var newest []string
		for j := len(s.ids) - 1; j >= 0 && len(newest) < 3; j-- {
			newest = append(newest, fmt.Sprintf("#%d", s.ids[j]))
		}
		parts = append(parts, fmt.Sprintf("%s %d (%s)", s.name, len(s.ids), strings.Join(newest, " ")))
	}
	inbox := "a2 inbox --since " + strconv.FormatInt(base, 10)
	if channel != "" {
		inbox = "a2 inbox --channel " + channel + " --since " + strconv.FormatInt(base, 10)
	}
	return fmt.Sprintf("[a2] backlog: %d messages #%d-#%d not printed: %s. Full text: %s", len(ms), lo, hi, strings.Join(parts, ", "), inbox)
}

type fatalErr struct{ error }

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// run watches until ctx ends or the cursor cannot be written.
func (wt *watcher) run(ctx context.Context) error {
	tctx, stopTick := context.WithCancel(ctx)
	defer stopTick()
	if wt.tickPath != "" && wt.tickPath != tickOff {
		every, now := wt.tickEvery, wt.tickNow
		if every <= 0 {
			every = tickEvery
		}
		if now == nil {
			now = time.Now
		}
		go runTick(tctx, wt.tickPath, every, now, wt.errOut)
	}
	backoff := wt.backoff0
	for ctx.Err() == nil {
		if !wt.started {
			if err := wt.start(ctx); err != nil {
				var fe fatalErr
				if errors.As(err, &fe) {
					return fe.error
				}
				if ctx.Err() != nil {
					return nil
				}
				wt.report(err)
				if !sleepCtx(ctx, backoff) {
					return nil
				}
				backoff = min(backoff*2, wt.backoffN)
				continue
			}
			backoff = wt.backoff0
		}
		connected, err := wt.socket(ctx)
		var fe fatalErr
		switch {
		case errors.As(err, &fe):
			return fe.error
		case errors.Is(err, errNoWS):
			if err := wt.longPoll(ctx, time.Now().Add(wt.probe)); err != nil {
				return err
			}
			backoff = wt.backoff0
			continue
		case ctx.Err() != nil:
			return nil
		}
		if connected {
			backoff = wt.backoff0
		}
		if connected && restartClose(err) {
			if wt.downSince.IsZero() {
				wt.downSince = time.Now()
			}
		} else if err != nil {
			wt.report(err)
		}
		if !sleepCtx(ctx, backoff) {
			return nil
		}
		backoff = min(backoff*2, wt.backoffN)
	}
	return nil
}

// socket runs one WebSocket session. connected is true once the upgrade succeeded.
func (wt *watcher) socket(ctx context.Context) (connected bool, err error) {
	fc, err := wt.c.dialWS(ctx, wt.channel)
	if err != nil {
		return false, err
	}
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			fc.writeClose(wsCloseNormal, "")
			fc.c.Close()
		case <-stop:
			fc.c.Close()
		}
	}()
	wt.last = max(wt.last, readCursor(wt.cp)) // another a2 inbox may have moved it
	b, _ := json.Marshal(map[string]int64{"since": wt.last})
	if err := fc.writeFrame(wsOpText, b); err != nil {
		return true, err
	}
	wt.ok()
	for {
		fc.c.SetReadDeadline(time.Now().Add(wt.readWait))
		op, p, err := fc.readFrame()
		if err != nil {
			var ce *wsCloseError
			if errors.As(err, &ce) {
				fc.writeClose(ce.code, "")
			}
			return true, err
		}
		switch op {
		case wsOpPing:
			fc.writeFrame(wsOpPong, p)
		case wsOpClose:
			ci := closeInfo(p)
			fc.writeClose(wsCloseNormal, "")
			return true, ci
		case wsOpText:
			var m Message
			if err := json.Unmarshal(p, &m); err != nil {
				return true, fmt.Errorf("bad message from server: %w", err)
			}
			if err := wt.show(m); err != nil {
				return true, fatalErr{err}
			}
		}
		reexecIfPending(append([]string{"watch"}, wt.args...))
	}
}

// longPoll is the fallback for a server without /ws, until the next /ws probe.
func (wt *watcher) longPoll(ctx context.Context, until time.Time) error {
	for ctx.Err() == nil && time.Now().Before(until) {
		r, err := wt.c.inboxCtx(ctx, wt.last, wt.wait, wt.channel)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			wt.report(err)
			if !sleepCtx(ctx, 5*time.Second) {
				return nil
			}
			continue
		}
		wt.ok()
		for _, m := range r.Messages {
			if err := wt.show(m); err != nil {
				return err
			}
		}
		reexecIfPending(append([]string{"watch"}, wt.args...))
	}
	return nil
}

func cmdWatch(args []string) error {
	fs := flag.NewFlagSet("watch", flag.ContinueOnError)
	wait := fs.Int("wait", 60, "long-poll seconds per request, only used with an older server without /ws (max 300)")
	channel := fs.String("channel", "", "only this channel (own cursor); default: all your channels")
	tickFile := fs.String("tick-file", "", "liveness stamp file, written at start and every 60 s (default: $XDG_STATE_HOME/agentsagora/watch-tick-<channel|all>; off = none)")
	fromStart := fs.Bool("from-start", false, "with no cursor file yet, print the whole history from id 1 (default: start at the newest message)")
	backlog := fs.Int("backlog", defaultBacklogMax, "with a cursor, print up to this many waiting messages in full; more (or no cursor at all) gives one summary line (a2 inbox has the full text)")
	if err := fs.Parse(args); err != nil {
		return usageError(err.Error())
	}
	c, err := newClient()
	if err != nil {
		return err
	}
	wt := newWatcher(c, *channel, *wait, args)
	wt.fromStart = *fromStart
	wt.backlogMax = max(*backlog, 0)
	wt.tickPath = *tickFile
	if wt.tickPath == "" {
		wt.tickPath = tickDefaultPath(*channel)
	}
	return wt.run(context.Background())
}

// inboxCtx is inbox with a context (the long-poll ends when ctx ends).
func (c *client) inboxCtx(ctx context.Context, since int64, wait int, channel string) (inboxResp, error) {
	var r inboxResp
	q := url.Values{"since": {strconv.FormatInt(since, 10)}}
	if channel != "" {
		q.Set("channel", channel)
	}
	if wait > 0 {
		q.Set("wait", strconv.Itoa(wait))
	}
	req, err := http.NewRequestWithContext(ctx, "GET", c.base+"/inbox?"+q.Encode(), nil)
	if err != nil {
		return r, err
	}
	err = c.doReq(req, &r)
	return r, err
}
