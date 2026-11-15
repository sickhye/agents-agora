package main

// Liveness alerts to a webhook.
//
// Passive: no ping messages. A member's last contact is the last authenticated request
// (inbox poll, send, MCP call, /ws open; server.go authOrigin), the end of an inbox answer,
// and every minute while it has an open /ws. A member is alive while it has an open /ws or
// its last contact is less than liveWindow ago.
//
// Watched members are those alive at least once in the last liveWatch (24h); a member never
// seen (no token yet, never connected) is skipped. When a watched member has not been alive
// for liveWindow, one alert line is sent; when it is alive again, one recovery line. One
// alert per incident: the open incidents and last contacts are kept in
// stateDir()/liveness.json (0600), so a restart neither repeats nor forgets them. The first
// liveGrace after the server starts never alerts (clients reconnect then).
//
// What leaves the machine: only the alert line (member name, HH:MM, undelivered count;
// never message text, file names or tokens) as {"text": ...} by HTTPS POST to the
// incoming-webhook URL in configDir()/alert-webhook (the format chat tools such as Slack,
// Mattermost or Discord (with /slack suffix) accept). The file is made by the owner; the
// server never creates or prints it, refuses it unless it is a regular file of mode 0600
// owned by the server user, and never logs the URL (errors are logged as "timeout",
// "status N" or "request failed"). No file = alerts off (one log line).

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	liveWindow   = 10 * time.Minute // alive while the last contact is younger than this
	liveWatch    = 24 * time.Hour   // watch members alive at least once in this span
	liveGrace    = 3 * time.Minute  // no alert this long after the server starts
	liveTick     = time.Minute
	livenessFile = "liveness.json"
	webhookFile  = "alert-webhook"
	hookTimeout  = 10 * time.Second
	hookMaxTries = 2 // first try, then once more at the next tick
)

// liveMember is one member's line in liveness.json (key = member base name).
type liveMember struct {
	Last      time.Time `json:"last,omitzero"`       // last contact
	Down      bool      `json:"down,omitempty"`      // incident open (alert sent or tried)
	DownSince time.Time `json:"down_since,omitzero"` // when the incident was opened
}

// liveFile is liveness.json. Updated and Down are written every tick for
// `a2 liveness-check`; older builds ignore them.
type liveFile struct {
	Members map[string]*liveMember `json:"members"`
	Updated time.Time              `json:"updated,omitzero"` // last tick
	Down    []liveDown             `json:"down"`             // open incidents (null in older files)
}

// liveDown is one open incident: current name, last contact, undelivered messages now.
type liveDown struct {
	Name    string    `json:"name"`
	Last    time.Time `json:"last"`
	Pending int       `json:"pending"`
}

type liveOut struct {
	text  string
	tries int
}

type liveness struct {
	mu      sync.Mutex
	path    string // liveness.json
	hook    string // alert-webhook file
	started time.Time
	m       map[string]*liveMember
	dirty   bool
	queue   []liveOut
	offWhy  string    // last "alerts off" reason logged, so it is logged once
	log     io.Writer // server log (stderr); tests capture it
	client  *http.Client
}

func newLiveness(statePath, hookPath string, now time.Time) *liveness {
	l := &liveness{path: statePath, hook: hookPath, started: now, m: map[string]*liveMember{}, log: os.Stderr,
		client: &http.Client{Timeout: hookTimeout,
			// never follow a redirect: the line goes to the webhook host only
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	b, err := os.ReadFile(statePath)
	if err == nil {
		var f struct {
			Members map[string]*liveMember `json:"members"`
		}
		if err := json.Unmarshal(b, &f); err != nil {
			l.logf("%s: skipped: %v", statePath, err)
		} else {
			for k, v := range f.Members {
				if v != nil {
					l.m[k] = v
				}
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		l.logf("%s: %v", statePath, err)
	}
	return l
}

func (l *liveness) logf(format string, args ...any) {
	fmt.Fprintf(l.log, "a2: liveness: "+format+"\n", args...)
}

// touch records a contact of member (current name) at now.
func (l *liveness) touch(member string, now time.Time) {
	if l == nil {
		return
	}
	b, ok := currentNames().baseOf(member)
	if !ok {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.touchLocked(b, now)
}

func (l *liveness) touchLocked(base string, now time.Time) {
	lm := l.m[base]
	if lm == nil {
		lm = &liveMember{}
		l.m[base] = lm
	}
	if now.After(lm.Last) {
		lm.Last = now
		l.dirty = true
	}
}

// open reports whether member (current name) has an open /ws.
func (h *wsHub) open(member string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.count[member] > 0
}

// liveView is the state of one member for /members and the viewer UI.
type liveView struct {
	seen  bool
	alive bool
	last  time.Time
}

func (sv *server) liveOf(nt nameTable, base string, now time.Time) liveView {
	if sv.live == nil {
		return liveView{}
	}
	wsOpen := sv.ws.open(nt.current(base))
	sv.live.mu.Lock()
	defer sv.live.mu.Unlock()
	lm := sv.live.m[base]
	if lm == nil || lm.Last.IsZero() {
		return liveView{seen: wsOpen, alive: wsOpen, last: now}
	}
	last := lm.Last
	if wsOpen {
		last = now
	}
	return liveView{seen: true, alive: wsOpen || now.Sub(lm.Last) < liveWindow, last: last}
}

// liveTick checks every member once: opens and closes incidents, sends the queued lines and
// saves the state. The state is saved every tick with "updated" and the open
// incidents in "down", read by `a2 liveness-check` (livecheck.go). cmdServe calls it every
// minute; tests call it with their own clock.
func (sv *server) liveTick(now time.Time) {
	l := sv.live
	if l == nil {
		return
	}
	nt := currentNames()
	type check struct {
		base, name string
		wsOpen     bool
	}
	var cs []check
	var out []string // members outside the team channel: not watched
	for _, b := range sv.members {
		n := nt.current(b)
		if !slices.Contains(nt.channels(n), chTeam) {
			out = append(out, b)
			continue
		}
		cs = append(cs, check{b, n, sv.ws.open(n)})
	}
	type downInfo struct {
		name string
		last time.Time
	}
	var downs []downInfo
	l.mu.Lock()
	for _, b := range out {
		// left team (or never in it): drop any open incident so no stale line stays
		if lm := l.m[b]; lm != nil && lm.Down {
			lm.Down, lm.DownSince = false, time.Time{}
			l.dirty = true
		}
	}
	for _, c := range cs {
		if c.wsOpen {
			l.touchLocked(c.base, now)
		}
		lm := l.m[c.base]
		if lm == nil || lm.Last.IsZero() {
			continue // never seen: not watched
		}
		alive := c.wsOpen || now.Sub(lm.Last) < liveWindow
		switch {
		case lm.Down && alive:
			lm.Down, lm.DownSince = false, time.Time{}
			l.dirty = true
			l.queue = append(l.queue, liveOut{text: fmt.Sprintf("a2 liveness: %s back %s", c.name, lm.Last.In(jst).Format("15:04"))})
		case !lm.Down && !alive && now.Sub(lm.Last) < liveWatch+liveWindow && now.Sub(l.started) >= liveGrace:
			lm.Down, lm.DownSince = true, now
			l.dirty = true
			downs = append(downs, downInfo{c.name, lm.Last})
		}
	}
	// open incidents for the state file (liveness-check), in member order
	var open []downInfo
	for _, c := range cs {
		if lm := l.m[c.base]; lm != nil && lm.Down {
			open = append(open, downInfo{c.name, lm.Last})
		}
	}
	l.mu.Unlock()
	// the undelivered count takes the store and receipt locks; not under l.mu
	var lines []liveOut
	for _, d := range downs {
		lines = append(lines, liveOut{text: fmt.Sprintf("a2 liveness: %s no response — last seen %s, undelivered %d", d.name, d.last.In(jst).Format("15:04"), sv.pendingFor(d.name, nt))})
	}
	down := []liveDown{}
	for _, d := range open {
		down = append(down, liveDown{Name: d.name, Last: d.last, Pending: sv.pendingFor(d.name, nt)})
	}
	l.mu.Lock()
	l.queue = append(l.queue, lines...)
	queue := l.queue
	l.queue = nil
	// saved every tick, so a stale "updated" means the server is not running
	save, _ := json.MarshalIndent(liveFile{Members: l.m, Updated: now, Down: down}, "", "  ")
	l.dirty = false
	l.mu.Unlock()
	if save != nil {
		if err := os.MkdirAll(filepath.Dir(l.path), 0o700); err == nil {
			err = writeFile0600(l.path, append(save, '\n'))
			if err != nil {
				l.logf("save: %v", err)
			}
		}
	}
	if len(queue) == 0 {
		return
	}
	hook, ok := l.webhook()
	if !ok {
		return // alerts off: lines are dropped (the reason was logged once)
	}
	var retry []liveOut
	for _, q := range queue {
		if err := l.post(hook, q.text); err != nil {
			q.tries++
			if q.tries < hookMaxTries {
				l.logf("webhook post failed (%s), retry next minute", err)
				retry = append(retry, q)
			} else {
				l.logf("webhook post failed (%s), line dropped", err)
			}
		}
	}
	if len(retry) > 0 {
		l.mu.Lock()
		l.queue = append(retry, l.queue...)
		l.mu.Unlock()
	}
}

// pendingFor counts the messages member (current name) can receive that were never handed
// to its client (receipts.go). Messages older than the delivery log are not counted.
func (sv *server) pendingFor(member string, nt nameTable) int {
	if sv.deliv == nil {
		return 0
	}
	base, ok := nt.baseOf(member)
	if !ok {
		return 0
	}
	_, start := sv.deliv.of(0)
	msgs, _ := sv.store.inbox(member, start, nt.channels(member), nt)
	sv.deliv.mu.Lock()
	defer sv.deliv.mu.Unlock()
	n := 0
	for _, m := range msgs {
		if _, got := sv.deliv.at[m.ID][base]; !got {
			n++
		}
	}
	return n
}

// webhook reads the webhook URL. It returns ok=false (and logs the reason once) when the
// file is missing, not a regular file, not mode 0600, not owned by the server user, or
// has no https URL. The URL itself is never logged.
func (l *liveness) webhook() (string, bool) {
	u, why := readWebhook(l.hook)
	l.mu.Lock()
	defer l.mu.Unlock()
	if why != "" {
		if why != l.offWhy {
			l.offWhy = why
			l.logf("%s", why)
		}
		return "", false
	}
	if l.offWhy != "" {
		l.offWhy = ""
		l.logf("webhook file found, alerts on")
	}
	return u, true
}

func readWebhook(path string) (string, string) {
	fi, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", "no " + webhookFile + " file in the config dir, alerts off"
	}
	if err != nil {
		return "", webhookFile + " file unreadable, alerts off"
	}
	if !fi.Mode().IsRegular() {
		return "", "webhook file not a regular file, alerts off"
	}
	// Windows has no Unix modes or owners; rely on the user profile ACL there.
	if runtime.GOOS != "windows" && (fi.Mode().Perm() != 0o600 || !ownedByMe(fi)) {
		return "", "webhook file not 0600, alerts off"
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", webhookFile + " file unreadable, alerts off"
	}
	u := strings.TrimSpace(string(b))
	pu, err := url.Parse(u)
	if err != nil || pu.Scheme != "https" || pu.Host == "" || strings.ContainsAny(u, " \t\r\n") {
		return "", "webhook file has no https URL, alerts off"
	}
	return u, ""
}

// post sends one line. Errors carry no URL.
func (l *liveness) post(hook, text string) error {
	body, _ := json.Marshal(map[string]string{"text": text})
	req, err := http.NewRequest("POST", hook, bytes.NewReader(body))
	if err != nil {
		return errors.New("bad request")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", userAgent)
	resp, err := l.client.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) && ue.Timeout() {
			return errors.New("timeout")
		}
		return errors.New("request failed")
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}

// liveRun ticks until stop is closed. The first line says whether alerts are on.
func (sv *server) liveRun(stop <-chan struct{}) {
	if sv.live == nil {
		return
	}
	if _, ok := sv.live.webhook(); ok {
		sv.live.logf("alerts on (webhook file found)")
	}
	t := time.NewTicker(liveTick)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			now := sv.now()
			sv.liveTick(now)
			sv.undeliveredTick(now) // undelivered notice to the sender sendcheck.go)
			sv.voteTick(now)        // vote results (vote.go)
			sv.digestTick(now)      // status digest digest.go)
			sv.replyTick(now)       // needs-reply reminders needsreply.go)
		}
	}
}
