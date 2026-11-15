package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// wsRaw dials path on the test server with hdr (Upgrade headers are added) and returns the
// HTTP status and, on 101, the socket.
func wsRaw(t *testing.T, base, path string, hdr map[string]string) (int, *wsConnFrames) {
	t.Helper()
	u, _ := url.Parse(base)
	conn, err := net.Dial("tcp", u.Host)
	if err != nil {
		t.Fatal(err)
	}
	key := "dGhlIHNhbXBsZSBub25jZQ=="
	req, _ := http.NewRequest("GET", base+path, nil)
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Sec-WebSocket-Key", key)
	req.Header.Set("Sec-WebSocket-Version", "13")
	for k, v := range hdr {
		if v == "" {
			req.Header.Del(k)
		} else {
			req.Header.Set(k, v)
		}
	}
	req.Write(conn)
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 101 {
		resp.Body.Close()
		conn.Close()
		return resp.StatusCode, nil
	}
	if resp.Header.Get("Sec-WebSocket-Accept") != "s3pPLMBiTxaQ9kYGzzhZRbK+xOo=" { // RFC 6455 1.3 example
		t.Fatalf("accept %q", resp.Header.Get("Sec-WebSocket-Accept"))
	}
	fc := &wsConnFrames{c: conn, r: br, client: true, maxIn: wsMaxOutFrame}
	t.Cleanup(func() { conn.Close() })
	return 101, fc
}

func bearer(tok string) map[string]string { return map[string]string{"Authorization": "Bearer " + tok} }

func sendSince(t *testing.T, fc *wsConnFrames, since int64) {
	t.Helper()
	b, _ := json.Marshal(map[string]int64{"since": since})
	if err := fc.writeFrame(wsOpText, b); err != nil {
		t.Fatal(err)
	}
}

// nextMsg reads frames (answering pings) until a text message or d passes.
func nextMsg(t *testing.T, fc *wsConnFrames, d time.Duration) (Message, error) {
	t.Helper()
	fc.c.SetReadDeadline(time.Now().Add(d))
	for {
		op, p, err := fc.readFrame()
		if err != nil {
			return Message{}, err
		}
		switch op {
		case wsOpPing:
			fc.writeFrame(wsOpPong, p)
		case wsOpClose:
			return Message{}, closeInfo(p)
		case wsOpText:
			var m Message
			if err := json.Unmarshal(p, &m); err != nil {
				t.Fatal(err)
			}
			return m, nil
		}
	}
}

func TestWSHandshakeAuth(t *testing.T) {
	f := setup(t)
	f.sv.viewOrigin = "https://a2.test"
	tok := f.tokens["alice"]
	cases := []struct {
		name string
		hdr  map[string]string
		path string
		want int
	}{
		{"no token", nil, "/ws", 401},
		{"bad token", bearer("x"), "/ws", 401},
		{"token in query only", nil, "/ws?token=" + url.QueryEscape(tok), 401},
		{"foreign origin", map[string]string{"Authorization": "Bearer " + tok, "Origin": "https://evil.test"}, "/ws", 403},
		{"no upgrade", map[string]string{"Authorization": "Bearer " + tok, "Upgrade": "", "Connection": ""}, "/ws", 426},
		{"bad version", map[string]string{"Authorization": "Bearer " + tok, "Sec-WebSocket-Version": "8"}, "/ws", 426},
		{"bad key", map[string]string{"Authorization": "Bearer " + tok, "Sec-WebSocket-Key": "short"}, "/ws", 400},
		{"channel not allowed", bearer(f.tokens["erin"]), "/ws?channel=team", 403},
		{"ok", bearer(tok), "/ws", 101},
		{"same origin ok", map[string]string{"Authorization": "Bearer " + tok, "Origin": "https://a2.test"}, "/ws", 101},
	}
	for _, c := range cases {
		if got, _ := wsRaw(t, f.srv.URL, c.path, c.hdr); got != c.want {
			t.Errorf("%s: status %d, want %d", c.name, got, c.want)
		}
	}
	// the token never shows up in the audit log
	if b, _ := readFileString(filepath.Join(stateDir(), "audit.jsonl")); strings.Contains(b, tok) {
		t.Error("token in audit log")
	}
}

func readFileString(p string) (string, error) {
	b, err := os.ReadFile(p)
	return string(b), err
}

func TestWSPushLatencyAndShape(t *testing.T) {
	f := setup(t)
	_, fc := wsRaw(t, f.srv.URL, "/ws", bearer(f.tokens["bob"]))
	sendSince(t, fc, 0)
	time.Sleep(100 * time.Millisecond)
	start := time.Now()
	code, sent := send(t, f, "alice", "team", "bob", "hello push")
	if code != 200 {
		t.Fatal(code, sent)
	}
	m, err := nextMsg(t, fc, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("push took %v", d)
	}
	if m.ID != idOf(sent) || m.Text != "hello push" || m.From != "alice" || m.To != "bob" || m.Channel != "team" {
		t.Errorf("got %+v", m)
	}
	// same shape as an /inbox item
	_, out := f.req(t, "GET", "/inbox", f.tokens["bob"], nil)
	items := msgs(t, out)
	b1, _ := json.Marshal(items[0])
	var viaInbox Message
	json.Unmarshal(b1, &viaInbox)
	if viaInbox != m {
		t.Errorf("ws %+v != inbox %+v", m, viaInbox)
	}
}

func TestWSChannelRights(t *testing.T) {
	f := setup(t)
	_, fc := wsRaw(t, f.srv.URL, "/ws", bearer(f.tokens["erin"])) // general only
	sendSince(t, fc, 0)
	send(t, f, "alice", "team", "all", "work item")
	send(t, f, "alice", "general", "bob", "dm to someone else")
	_, g := send(t, f, "alice", "general", "all", "lunch?")
	m, err := nextMsg(t, fc, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if m.ID != idOf(g) || m.Channel != "general" {
		t.Errorf("general-only member got %+v", m)
	}
	if m, err := nextMsg(t, fc, 300*time.Millisecond); err == nil {
		t.Errorf("unexpected extra message %+v", m)
	}
	// a full member filtering on general does not get work either
	_, fc2 := wsRaw(t, f.srv.URL, "/ws?channel=general", bearer(f.tokens["bob"]))
	sendSince(t, fc2, 0)
	m, err = nextMsg(t, fc2, 2*time.Second)
	if err != nil || m.Channel != "general" || m.Text != "dm to someone else" {
		t.Errorf("filtered got %+v %v", m, err)
	}
}

func TestWSResumeFromSince(t *testing.T) {
	f := setup(t)
	var ids []int64
	for i := 0; i < 3; i++ {
		_, o := send(t, f, "alice", "team", "all", "m"+string(rune('a'+i)))
		ids = append(ids, idOf(o))
	}
	_, fc := wsRaw(t, f.srv.URL, "/ws", bearer(f.tokens["dave"]))
	sendSince(t, fc, ids[0])
	for _, want := range ids[1:] {
		m, err := nextMsg(t, fc, 2*time.Second)
		if err != nil || m.ID != want {
			t.Fatalf("got %+v %v, want #%d", m, err, want)
		}
	}
	// no since frame: starts at the newest id
	_, fc2 := wsRaw(t, f.srv.URL, "/ws", bearer(f.tokens["dave"]))
	time.Sleep(wsSinceWait + 200*time.Millisecond)
	_, o := send(t, f, "alice", "team", "all", "after")
	m, err := nextMsg(t, fc2, 2*time.Second)
	if err != nil || m.ID != idOf(o) {
		t.Errorf("no-since socket got %+v %v", m, err)
	}
}

func TestWSPingPongTimeout(t *testing.T) {
	f := setup(t)
	f.sv.wsPing, f.sv.wsPongWait = 50*time.Millisecond, 300*time.Millisecond
	// silent client: gets pings, then a close
	_, fc := wsRaw(t, f.srv.URL, "/ws", bearer(f.tokens["alice"]))
	sendSince(t, fc, 0)
	fc.c.SetReadDeadline(time.Now().Add(3 * time.Second))
	pings, start := 0, time.Now()
	var closed *wsCloseError
	for closed == nil {
		op, p, err := fc.readFrame()
		if err != nil {
			t.Fatalf("no close frame: %v (pings %d)", err, pings)
		}
		switch op {
		case wsOpPing:
			pings++
		case wsOpClose:
			closed = closeInfo(p)
		}
	}
	if pings == 0 || closed.code != wsCloseGoingAway || time.Since(start) > 2*time.Second {
		t.Errorf("pings %d close %+v after %v", pings, closed, time.Since(start))
	}
	// answering client stays open past the timeout
	_, fc2 := wsRaw(t, f.srv.URL, "/ws", bearer(f.tokens["alice"]))
	sendSince(t, fc2, 0)
	if _, err := nextMsg(t, fc2, 800*time.Millisecond); !isTimeout(err) {
		t.Errorf("answering client closed: %v", err)
	}
	// revoked token: closed at the next ping
	if err := tokenRevoke("alice"); err != nil {
		t.Fatal(err)
	}
	_, err := nextMsg(t, fc2, 2*time.Second)
	var ce *wsCloseError
	if !errors.As(err, &ce) || ce.code != wsClosePolicy {
		t.Errorf("revoked token socket: %v", err)
	}
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

func TestWSLimits(t *testing.T) {
	f := setup(t)
	tok := f.tokens["frank"]
	for i := 0; i < wsMaxPerMember; i++ {
		if code, _ := wsRaw(t, f.srv.URL, "/ws", bearer(tok)); code != 101 {
			t.Fatalf("socket %d: %d", i, code)
		}
	}
	if code, _ := wsRaw(t, f.srv.URL, "/ws", bearer(tok)); code != 429 {
		t.Errorf("socket %d: %d, want 429", wsMaxPerMember+1, code)
	}
	if code, _ := wsRaw(t, f.srv.URL, "/ws", bearer(f.tokens["erin"])); code != 101 {
		t.Errorf("other member blocked: %d", code)
	}
	// oversize client frame -> close 1009
	_, fc := wsRaw(t, f.srv.URL, "/ws", bearer(f.tokens["dave"]))
	fc.writeFrame(wsOpText, bytes.Repeat([]byte(" "), wsMaxInFrame+1))
	_, err := nextMsg(t, fc, 2*time.Second)
	var ce *wsCloseError
	if !errors.As(err, &ce) || ce.code != wsCloseTooBig {
		t.Errorf("oversize frame: %v", err)
	}
	// unmasked client frame -> protocol error
	_, fc = wsRaw(t, f.srv.URL, "/ws", bearer(f.tokens["dave"]))
	fc.client = false
	fc.writeFrame(wsOpText, []byte(`{"since":0}`))
	fc.client = true
	if _, err := nextMsg(t, fc, 2*time.Second); !errors.As(err, &ce) || ce.code != wsCloseProtocol {
		t.Errorf("unmasked frame: %v", err)
	}
}

func TestWSShutdownClosesSockets(t *testing.T) {
	for _, restart := range []bool{false, true} {
		f := setup(t)
		_, fc := wsRaw(t, f.srv.URL, "/ws", bearer(f.tokens["alice"]))
		sendSince(t, fc, 0)
		time.Sleep(100 * time.Millisecond)
		if restart {
			f.sv.restarting.Store(true)
		}
		close(f.sv.quit)
		_, err := nextMsg(t, fc, 3*time.Second)
		want := wsCloseGoingAway
		if restart {
			want = wsCloseRestart
		}
		var ce *wsCloseError
		if !errors.As(err, &ce) || ce.code != want {
			t.Errorf("restart=%v: %v, want close %d", restart, err, want)
		}
		fc.writeClose(wsCloseNormal, "")
		done := make(chan struct{})
		go func() { f.sv.ws.wait(5 * time.Second); close(done) }()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatal("sockets still open after shutdown")
		}
	}
}

// syncBuf is a goroutine-safe writer for the watcher output.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) lines() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.FieldsFunc(s.b.String(), func(r rune) bool { return r == '\n' })
}

func waitLines(t *testing.T, b *syncBuf, n int, d time.Duration) []string {
	t.Helper()
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		if l := b.lines(); len(l) >= n {
			return l
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("got %d lines, want %d: %q", len(b.lines()), n, b.lines())
	return nil
}

func testWatcher(t *testing.T, base, tok string, hc *http.Client) (*watcher, *syncBuf, *syncBuf) {
	c := &client{base: base, token: tok, cursor: filepath.Join(t.TempDir(), "cursor"), http: hc}
	wt := newWatcher(c, "", 1, nil)
	out, errOut := &syncBuf{}, &syncBuf{}
	wt.out, wt.errOut = out, errOut
	wt.backoff0, wt.backoffN = 100*time.Millisecond, 400*time.Millisecond
	return wt, out, errOut
}

func runWatcher(t *testing.T, wt *watcher) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- wt.run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("watch: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("watch did not stop")
		}
	})
}

func TestWatchReconnectResumes(t *testing.T) {
	f := setup(t)
	wt, out, _ := testWatcher(t, f.srv.URL, f.tokens["bob"], f.srv.Client())
	runWatcher(t, wt)
	time.Sleep(200 * time.Millisecond)
	send(t, f, "alice", "team", "all", "one")
	send(t, f, "alice", "team", "bob", "two\nlines")
	waitLines(t, out, 2, 3*time.Second)
	f.sv.ws.dropAll() // network drop
	send(t, f, "alice", "team", "all", "three")
	send(t, f, "alice", "general", "all", "four")
	lines := waitLines(t, out, 4, 5*time.Second)
	time.Sleep(300 * time.Millisecond)
	lines = out.lines()
	if len(lines) != 4 {
		t.Fatalf("lines %q", lines)
	}
	for i, want := range []string{"#1 [🔒team]", "#2 [🔒team]", "#3 [🔒team]", "#4 [#general]"} {
		if !strings.HasPrefix(lines[i], "[a2] "+want) {
			t.Errorf("line %d %q, want %s", i, lines[i], want)
		}
	}
	if !strings.HasSuffix(lines[1], `: two\nlines`) {
		t.Errorf("line format changed: %q", lines[1])
	}
	if readCursor(wt.cp) != 4 {
		t.Errorf("cursor %d", readCursor(wt.cp))
	}
}

func TestWatchFallbackLongPollOn404(t *testing.T) {
	f := setup(t)
	h := f.sv.handler()
	var wsHits sync.WaitGroup
	wsHits.Add(1)
	var once sync.Once
	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ws" { // an older server
			once.Do(wsHits.Done)
			http.NotFound(w, r)
			return
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(old.Close)
	wt, out, errOut := testWatcher(t, old.URL, f.tokens["dave"], old.Client())
	runWatcher(t, wt)
	wsHits.Wait()
	send(t, f, "alice", "team", "all", "via long-poll")
	l := waitLines(t, out, 1, 3*time.Second)
	if !strings.HasSuffix(l[0], "via long-poll") {
		t.Errorf("line %q", l[0])
	}
	if s := errOut.b.String(); s != "" {
		t.Errorf("stderr %q", s)
	}
}

func TestWatchReconnectsOnSilentServer(t *testing.T) {
	var mu sync.Mutex
	dials := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		dials++
		mu.Unlock()
		fc, err := wsUpgrade(w, r)
		if err != nil {
			return
		}
		io.Copy(io.Discard, fc.r) // never pings
	}))
	t.Cleanup(srv.Close)
	wt, _, _ := testWatcher(t, srv.URL, "x", srv.Client())
	wt.fromStart = true // this fake server only upgrades; skip the first-run /inbox call
	wt.readWait = 200 * time.Millisecond
	runWatcher(t, wt)
	time.Sleep(900 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if dials < 2 {
		t.Errorf("dials %d, want a reconnect after the read timeout", dials)
	}
}

// TestWSOverTLSWithHTTP2 checks the upgrade on a TLS server that also offers h2 (as
// ServeTLS does): the client asks for http/1.1 by ALPN.
func TestWSOverTLSWithHTTP2(t *testing.T) {
	f := setup(t)
	srv := httptest.NewUnstartedServer(f.sv.handler())
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)
	tc := srv.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	tr := &http.Transport{TLSClientConfig: tc, ForceAttemptHTTP2: true}
	c := &client{base: srv.URL, token: f.tokens["alice"], http: &http.Client{Transport: tr}}
	fc, err := c.dialWS(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer fc.c.Close()
	if st := fc.c.(*tls.Conn).ConnectionState(); st.NegotiatedProtocol == "h2" {
		t.Fatal("negotiated h2")
	}
	sendSince(t, fc, 0)
	send(t, f, "dave", "team", "alice", "tls push")
	if m, err := nextMsg(t, fc, 2*time.Second); err != nil || m.Text != "tls push" {
		t.Errorf("got %+v %v", m, err)
	}
}

func TestViewWS(t *testing.T) {
	v := setupView(t)
	_, cookie := v.login(t, v.viewerTok, "")
	csrf := v.csrf(t, cookie)
	ck := viewCookie + "=" + cookie
	cases := []struct {
		name string
		hdr  map[string]string
		want int
	}{
		{"no cookie", map[string]string{"Origin": v.origin}, 401},
		{"no origin", map[string]string{"Cookie": ck}, 403},
		{"foreign origin", map[string]string{"Cookie": ck, "Origin": "https://evil.test"}, 403},
		{"cross-site", map[string]string{"Cookie": ck, "Origin": v.origin, "Sec-Fetch-Site": "cross-site"}, 403},
		{"agent token", map[string]string{"Origin": v.origin, "Authorization": "Bearer " + v.tokens["alice"]}, 401},
	}
	for _, c := range cases {
		if got, _ := wsRaw(t, v.srv.URL, "/view/ws", c.hdr); got != c.want {
			t.Errorf("%s: %d, want %d", c.name, got, c.want)
		}
	}
	hdr := map[string]string{"Cookie": ck, "Origin": v.origin, "Sec-Fetch-Site": "same-origin"}
	// bad csrf -> closed
	_, fc := wsRaw(t, v.srv.URL, "/view/ws", hdr)
	fc.writeFrame(wsOpText, []byte(`{"since":0,"csrf":"nope"}`))
	var ce *wsCloseError
	if _, err := nextMsg(t, fc, 2*time.Second); !errors.As(err, &ce) || ce.code != wsClosePolicy {
		t.Errorf("bad csrf: %v", err)
	}
	// good csrf: sees DMs between members too
	_, fc = wsRaw(t, v.srv.URL, "/view/ws", hdr)
	b, _ := json.Marshal(map[string]any{"since": 0, "csrf": csrf})
	fc.writeFrame(wsOpText, b)
	time.Sleep(100 * time.Millisecond)
	send(t, v.fixture, "alice", "team", "dave", "dm seen by the owner")
	m, err := nextMsg(t, fc, 2*time.Second)
	if err != nil || m.Text != "dm seen by the owner" {
		t.Errorf("view got %+v %v", m, err)
	}
}
