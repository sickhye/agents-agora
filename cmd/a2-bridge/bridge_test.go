package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

const (
	testDiscordToken = "MTEST.discord-bot-token-should-never-appear.xyz"
	testA2Token      = "a2tok-should-never-appear-0123456789"
	testChannel      = "123456789012345678"
	botID            = "999000000000000001"
)

// ---- fake a2 server ----

type fakeA2 struct {
	mu       sync.Mutex
	msgs     []a2Message
	sent     []map[string]any
	you      string
	channels []string
	failWith string // error text returned with HTTP 500 on every request
	srv      *httptest.Server
}

func newFakeA2(t *testing.T) *fakeA2 {
	f := &fakeA2{you: "discord-bridge", channels: []string{"general"}}
	mux := http.NewServeMux()
	auth := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer "+testA2Token {
				http.Error(w, `{"error":"bad token"}`, 401)
				return
			}
			f.mu.Lock()
			fw := f.failWith
			f.mu.Unlock()
			if fw != "" {
				w.WriteHeader(500)
				json.NewEncoder(w).Encode(map[string]string{"error": fw})
				return
			}
			h(w, r)
		}
	}
	mux.HandleFunc("GET /members", auth(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{"you": f.you, "your_channels": f.channels})
	}))
	mux.HandleFunc("GET /inbox", auth(func(w http.ResponseWriter, r *http.Request) {
		since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
		wait := r.URL.Query().Get("wait") != "0"
		for i := 0; ; i++ {
			f.mu.Lock()
			var out []a2Message
			for _, m := range f.msgs { // the fake does not filter: the bridge must
				if m.ID > since {
					out = append(out, m)
				}
			}
			f.mu.Unlock()
			if len(out) > 0 || !wait || i > 10 || r.Context().Err() != nil {
				json.NewEncoder(w).Encode(map[string]any{"messages": out})
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}))
	mux.HandleFunc("POST /send", auth(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if strings.Contains(fmt.Sprint(body["text"]), "LINT") {
			w.WriteHeader(422)
			json.NewEncoder(w).Encode(map[string]string{"error": "work content goes to work (general channel rule: x, general-deny.txt line 3)"})
			return
		}
		f.mu.Lock()
		f.sent = append(f.sent, body)
		f.mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{"id": 1})
	}))
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeA2) add(m a2Message) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if m.ID == 0 {
		m.ID = int64(len(f.msgs) + 1)
		if n := len(f.msgs); n > 0 {
			m.ID = f.msgs[n-1].ID + 1
		}
	}
	f.msgs = append(f.msgs, m)
}

func (f *fakeA2) sentTexts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, b := range f.sent {
		if b["channel"] != "general" || b["to"] != "all" {
			out = append(out, "BAD-TARGET:"+fmt.Sprint(b))
			continue
		}
		out = append(out, fmt.Sprint(b["text"]))
	}
	return out
}

// ---- fake Discord ----

type fakeDiscord struct {
	mu          sync.Mutex
	msgs        []discordMessage
	posts       []map[string]any
	postTimes   []time.Time
	nextID      uint64
	post429     int // answer the next N POSTs with 429
	retryAfter  float64
	failAll     string // HTTP 403 with this message on every request
	identified  []string
	getRequests int
	srv         *httptest.Server
}

func newFakeDiscord(t *testing.T) *fakeDiscord {
	f := &fakeDiscord{nextID: 1000, retryAfter: 0.2}
	mux := http.NewServeMux()
	auth := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bot "+testDiscordToken {
				w.WriteHeader(401)
				io.WriteString(w, `{"message":"401: Unauthorized","code":0}`)
				return
			}
			if !strings.HasPrefix(r.Header.Get("User-Agent"), "DiscordBot (") {
				w.WriteHeader(400)
				return
			}
			f.mu.Lock()
			fa := f.failAll
			f.mu.Unlock()
			if fa != "" {
				w.WriteHeader(403)
				json.NewEncoder(w).Encode(map[string]any{"message": fa, "code": 50001})
				return
			}
			h(w, r)
		}
	}
	mux.HandleFunc("GET /api/users/@me", auth(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"id": botID, "username": "a2bridge", "bot": true})
	}))
	mux.HandleFunc("GET /api/channels/{id}", auth(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"id": r.PathValue("id"), "name": "frank-room"})
	}))
	mux.HandleFunc("GET /api/channels/{id}/messages", auth(func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") != testChannel {
			w.WriteHeader(404)
			return
		}
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		after := r.URL.Query().Get("after")
		f.mu.Lock()
		f.getRequests++
		var out []discordMessage
		if after == "" { // latest `limit`
			for i := len(f.msgs) - 1; i >= 0 && len(out) < limit; i-- {
				out = append(out, f.msgs[i])
			}
		} else { // oldest `limit` after id, returned newest first like Discord
			var asc []discordMessage
			for _, m := range f.msgs {
				if snowLess(after, m.ID) {
					asc = append(asc, m)
				}
			}
			if len(asc) > limit {
				asc = asc[:limit]
			}
			for i := len(asc) - 1; i >= 0; i-- {
				out = append(out, asc[i])
			}
		}
		f.mu.Unlock()
		json.NewEncoder(w).Encode(out)
	}))
	mux.HandleFunc("POST /api/channels/{id}/messages", auth(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		defer f.mu.Unlock()
		f.postTimes = append(f.postTimes, time.Now())
		if f.post429 > 0 {
			f.post429--
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(429)
			json.NewEncoder(w).Encode(map[string]any{"message": "You are being rate limited.", "retry_after": f.retryAfter, "global": false})
			return
		}
		f.posts = append(f.posts, body)
		// the bot's own post shows up in the channel too
		f.nextID++
		f.msgs = append(f.msgs, discordMessage{ID: strconv.FormatUint(f.nextID, 10), Content: fmt.Sprint(body["content"]),
			Author: discordUser{ID: botID, Username: "a2bridge", Bot: true}})
		w.Header().Set("X-RateLimit-Remaining", "4")
		json.NewEncoder(w).Encode(map[string]any{"id": "1"})
	}))
	mux.HandleFunc("GET /gw", func(w http.ResponseWriter, r *http.Request) { f.gateway(t, w, r) })
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeDiscord) add(m discordMessage) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	m.ID = strconv.FormatUint(f.nextID, 10)
	f.msgs = append(f.msgs, m)
}

func (f *fakeDiscord) postTexts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, p := range f.posts {
		am, _ := p["allowed_mentions"].(map[string]any)
		if parse, ok := am["parse"].([]any); !ok || len(parse) != 0 {
			out = append(out, "MENTIONS-ALLOWED")
		}
		out = append(out, fmt.Sprint(p["content"]))
	}
	return out
}

// gateway: a fake WebSocket gateway (server frames unmasked, client frames masked).
func (f *fakeDiscord) gateway(t *testing.T, w http.ResponseWriter, r *http.Request) {
	key := r.Header.Get("Sec-WebSocket-Key")
	h := sha1.Sum([]byte(key + wsGUID))
	conn, brw, err := w.(http.Hijacker).Hijack()
	if err != nil {
		return
	}
	defer conn.Close()
	fmt.Fprintf(brw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", base64.StdEncoding.EncodeToString(h[:]))
	send := func(s string) {
		brw.Write([]byte{0x81, byte(len(s))})
		brw.WriteString(s)
		brw.Flush()
	}
	send(`{"op":10,"d":{"heartbeat_interval":41250}}`)
	ws := &wsConn{c: conn, br: brw.Reader}
	data, err := ws.readMessage()
	if err != nil {
		return
	}
	var p struct {
		Op int `json:"op"`
		D  struct {
			Token string `json:"token"`
		} `json:"d"`
	}
	json.Unmarshal(data, &p)
	f.mu.Lock()
	f.identified = append(f.identified, p.D.Token)
	f.mu.Unlock()
	send(`{"op":0,"t":"READY","s":1,"d":{}}`)
	ws.readMessage() // close frame
}

// ---- harness ----

type harness struct {
	t   *testing.T
	a2  *fakeA2
	dc  *fakeDiscord
	dir string
	log *syncBuf
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func writeFile(t *testing.T, p, s string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(p, []byte(s), mode); err != nil {
		t.Fatal(err)
	}
	os.Chmod(p, mode)
}

func newHarness(t *testing.T) *harness {
	h := &harness{t: t, a2: newFakeA2(t), dc: newFakeDiscord(t), dir: t.TempDir(), log: &syncBuf{}}
	writeFile(t, filepath.Join(h.dir, "dtok"), testDiscordToken+"\n", 0o600)
	writeFile(t, filepath.Join(h.dir, "atok"), testA2Token+"\n", 0o600)
	writeFile(t, filepath.Join(h.dir, "client.conf"), "url="+h.a2.srv.URL+"\ntoken_file=/nonexistent\n", 0o600)
	h.writeConf("")
	return h
}

func (h *harness) writeConf(extra string) string {
	p := filepath.Join(h.dir, "bridge.conf")
	writeFile(h.t, p, "# test\ndiscord_token_file="+filepath.Join(h.dir, "dtok")+
		"\ndiscord_channel_id="+testChannel+"\na2_token_file="+filepath.Join(h.dir, "atok")+
		"\nstate_dir="+filepath.Join(h.dir, "state")+"\ndiscord_api="+h.dc.srv.URL+"/api"+
		"\ndiscord_gateway=ws"+strings.TrimPrefix(h.dc.srv.URL, "http")+"/gw\n"+extra, 0o600)
	return p
}

func (h *harness) bridge() *bridge {
	h.t.Helper()
	c, err := loadConfig(filepath.Join(h.dir, "bridge.conf"))
	if err != nil {
		h.t.Fatal(err)
	}
	c.PollEvery = 30 * time.Millisecond
	b, err := newBridge(c, h.log)
	if err != nil {
		h.t.Fatal(err)
	}
	b.sleep = func(ctx context.Context, d time.Duration) error { return sleepCtx(ctx, min(d, 50*time.Millisecond)) }
	return b
}

// start runs a bridge until the returned stop func is called.
func (h *harness) start() func() {
	h.t.Helper()
	b := h.bridge()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- b.run(ctx) }()
	h.waitFor("running", func() bool { return strings.Contains(h.log.String(), "running:") })
	return func() {
		cancel()
		if err := <-done; err != nil {
			h.t.Fatalf("run: %v", err)
		}
	}
}

func (h *harness) waitFor(what string, ok func() bool) {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			h.t.Fatalf("timeout waiting for %s; log:\n%s", what, h.log.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (h *harness) noSecrets() {
	h.t.Helper()
	for _, s := range []string{testDiscordToken, testA2Token, "should-never-appear"} {
		if strings.Contains(h.log.String(), s) {
			h.t.Fatalf("log contains a token:\n%s", h.log.String())
		}
	}
}

// ---- tests ----

func TestGeneralOnlyOwnSkipAndFormats(t *testing.T) {
	h := newHarness(t)
	h.a2.add(a2Message{Channel: "general", From: "bob", To: "all", Text: "old, before start"})
	h.dc.add(discordMessage{Content: "old discord", Author: discordUser{ID: "5", Username: "bob"}})
	stop := h.start()
	defer stop()

	h.a2.add(a2Message{Channel: "general", From: "alice", To: "all", Text: "hello @everyone"})
	h.a2.add(a2Message{Channel: "team", From: "alice", To: "all", Text: "work only"})
	h.a2.add(a2Message{Channel: "general", From: "discord-bridge", To: "all", Text: "Alice: from discord"})
	h.a2.add(a2Message{Channel: "general", From: "alice", To: "frank", Text: "direct message"})
	h.a2.add(a2Message{Channel: "general", From: "erin", To: "all", Text: "**someone**: echoed back"})
	h.a2.add(a2Message{Channel: "general", From: "erin", To: "all", Text: "a reply", ReplyTo: 2})
	h.a2.add(a2Message{Channel: "general", From: "erin", To: "all", Text: "last"})

	h.dc.add(discordMessage{Content: "hey", Author: discordUser{ID: "10", Username: "alice", GlobalName: "Alice"}})
	h.dc.add(discordMessage{Content: "**alice**: hello", Author: discordUser{ID: botID, Username: "a2bridge", Bot: true}})
	h.dc.add(discordMessage{Content: "pic", Author: discordUser{ID: "11", Username: "frank", Bot: true},
		Attachments: []struct {
			URL      string `json:"url"`
			Filename string `json:"filename"`
		}{{URL: "https://cdn.example/x.png", Filename: "x.png"}}})
	h.dc.add(discordMessage{Content: "**x**: loop", Author: discordUser{ID: "12", Username: "other"}})
	h.dc.add(discordMessage{Type: 7, Content: "", Author: discordUser{ID: "13", Username: "joiner"}})
	h.dc.add(discordMessage{Content: "end", Author: discordUser{ID: "10", Username: "alice"}, Member: &struct {
		Nick string `json:"nick"`
	}{Nick: "Ali"}})

	h.waitFor("discord posts", func() bool { return len(h.dc.postTexts()) >= 3 })
	h.waitFor("a2 sends", func() bool { return len(h.a2.sentTexts()) >= 3 })
	time.Sleep(150 * time.Millisecond) // nothing more may arrive

	wantD := []string{"**alice**: hello @everyone", "↪ #2 **erin**: a reply", "**erin**: last"}
	if got := h.dc.postTexts(); !slices.Equal(got, wantD) {
		t.Errorf("discord posts:\n got %q\nwant %q", got, wantD)
	}
	wantA := []string{"Alice: hey", "frank: pic\nhttps://cdn.example/x.png", "Ali: end"}
	if got := h.a2.sentTexts(); !slices.Equal(got, wantA) {
		t.Errorf("a2 sends:\n got %q\nwant %q", got, wantA)
	}
	h.noSecrets()
}

func TestRateLimit429(t *testing.T) {
	h := newHarness(t)
	h.dc.post429 = 2
	h.dc.retryAfter = 0.15
	b := h.bridge()
	b.dc.sleep = sleepCtx // real waits for this test
	start := time.Now()
	if err := b.dc.post(context.Background(), testChannel, "x"); err != nil {
		t.Fatal(err)
	}
	if el := time.Since(start); el < 280*time.Millisecond {
		t.Errorf("two 429s of 0.15 s waited only %s", el)
	}
	if got := h.dc.postTexts(); !slices.Equal(got, []string{"x"}) {
		t.Errorf("posts %q", got)
	}
	if !strings.Contains(h.log.String(), "rate limited") {
		t.Errorf("no rate limit log:\n%s", h.log.String())
	}
	// always 429: give up after 5 tries with a 429 error (not refused => retried later)
	h.dc.post429 = 100
	h.dc.retryAfter = 0.001
	err := b.dc.post(context.Background(), testChannel, "y")
	if err == nil || refused(err) {
		t.Errorf("want a retryable 429 error, got %v", err)
	}
}

func TestRateLimitHeaders(t *testing.T) {
	var times []time.Time
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		times = append(times, time.Now())
		mu.Unlock()
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset-After", "0.2")
		io.WriteString(w, `{"id":"1","username":"u"}`)
	}))
	defer srv.Close()
	d := newDiscordClient(srv.URL, testDiscordToken, srv.Client(), func(string, ...any) {})
	for i := 0; i < 2; i++ {
		if _, err := d.me(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if gap := times[1].Sub(times[0]); gap < 180*time.Millisecond {
		t.Errorf("Remaining=0/Reset-After=0.2 not honoured: gap %s", gap)
	}
}

func TestRestartKeepsLastIDs(t *testing.T) {
	h := newHarness(t)
	stop := h.start()
	h.a2.add(a2Message{Channel: "general", From: "alice", To: "all", Text: "one"})
	h.dc.add(discordMessage{Content: "uno", Author: discordUser{ID: "10", Username: "alice"}})
	h.waitFor("first round", func() bool { return len(h.dc.postTexts()) == 1 && len(h.a2.sentTexts()) == 1 })
	stop()

	st, err := os.Stat(statePath(filepath.Join(h.dir, "state")))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("state mode %o, want 600", st.Mode().Perm())
	}
	// while stopped
	h.a2.add(a2Message{Channel: "general", From: "alice", To: "all", Text: "two"})
	h.dc.add(discordMessage{Content: "dos", Author: discordUser{ID: "10", Username: "alice"}})

	stop = h.start()
	defer stop()
	h.waitFor("second round", func() bool { return len(h.dc.postTexts()) >= 2 && len(h.a2.sentTexts()) >= 2 })
	time.Sleep(150 * time.Millisecond)
	if got, want := h.dc.postTexts(), []string{"**alice**: one", "**alice**: two"}; !slices.Equal(got, want) {
		t.Errorf("discord posts %q, want %q", got, want)
	}
	if got, want := h.a2.sentTexts(), []string{"alice: uno", "alice: dos"}; !slices.Equal(got, want) {
		t.Errorf("a2 sends %q, want %q", got, want)
	}
	if n := len(h.dc.identified); n != 1 {
		t.Errorf("gateway identify ran %d times, want 1 (saved in state)", n)
	}
	if h.dc.identified[0] != testDiscordToken {
		t.Errorf("identify sent a wrong token")
	}
}

func TestLintRefusalIsSkipped(t *testing.T) {
	h := newHarness(t)
	stop := h.start()
	defer stop()
	h.dc.add(discordMessage{Content: "LINT hit", Author: discordUser{ID: "10", Username: "alice"}})
	h.dc.add(discordMessage{Content: "fine", Author: discordUser{ID: "10", Username: "alice"}})
	h.waitFor("send", func() bool { return len(h.a2.sentTexts()) >= 1 })
	if got := h.a2.sentTexts(); !slices.Equal(got, []string{"alice: fine"}) {
		t.Errorf("sends %q", got)
	}
	if !strings.Contains(h.log.String(), "a2 refused discord") || strings.Contains(h.log.String(), "LINT hit") {
		t.Errorf("want a refusal line without the text:\n%s", h.log.String())
	}
}

func TestTokenNeverInLogs(t *testing.T) {
	h := newHarness(t)
	// servers echo the tokens in their errors; the bridge must redact them
	h.dc.failAll = "bad token " + testDiscordToken
	var out syncBuf
	err := cmdCheck(context.Background(), []string{"--conf", filepath.Join(h.dir, "bridge.conf")}, &out)
	if err == nil {
		t.Fatal("check passed with a failing Discord")
	}
	h.a2.mu.Lock()
	h.a2.failWith = "server saw " + testA2Token
	h.a2.mu.Unlock()
	err2 := cmdCheck(context.Background(), []string{"--conf", filepath.Join(h.dir, "bridge.conf")}, &out)
	h.log.Write([]byte(out.String()))
	b := h.bridge()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	err3 := b.run(ctx)
	for _, e := range []error{err, err2, err3} {
		if e != nil {
			h.log.Write([]byte(e.Error() + "\n"))
		}
	}
	if !strings.Contains(h.log.String(), "[redacted]") {
		t.Errorf("expected a redacted error:\n%s", h.log.String())
	}
	h.noSecrets()

	// a working check prints no token either
	h.dc.failAll = ""
	h.a2.mu.Lock()
	h.a2.failWith = ""
	h.a2.mu.Unlock()
	h.dc.add(discordMessage{Content: "", Author: discordUser{ID: "10", Username: "alice"}})
	var ok syncBuf
	if err := cmdCheck(context.Background(), []string{"--conf", filepath.Join(h.dir, "bridge.conf")}, &ok); err != nil {
		t.Fatalf("check: %v\n%s", err, ok.String())
	}
	s := ok.String()
	if strings.Contains(s, testDiscordToken) || strings.Contains(s, testA2Token) {
		t.Fatalf("check output has a token:\n%s", s)
	}
	for _, want := range []string{"ok   a2 member discord-bridge", "ok   discord bot a2bridge", "ok   discord channel #frank-room", "Message Content Intent", "all checks passed"} {
		if !strings.Contains(s, want) {
			t.Errorf("check output lacks %q:\n%s", want, s)
		}
	}
}

func TestConfigRefusals(t *testing.T) {
	h := newHarness(t)
	p := h.writeConf("a2_channel=team\n")
	if _, err := loadConfig(p); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Errorf("a2_channel=team: %v", err)
	}
	p = h.writeConf("a2_channel=random\n")
	if _, err := loadConfig(p); err == nil {
		t.Error("a2_channel=random accepted")
	}
	p = h.writeConf("a2_channel=general\n")
	if _, err := loadConfig(p); err != nil {
		t.Errorf("a2_channel=general: %v", err)
	}
	p = h.writeConf("a2_url=https://192.0.2.10:18765/\na2_ca_file=/x/ca.pem\n")
	if c, err := loadConfig(p); err != nil || c.A2URL != "https://192.0.2.10:18765" || c.A2CAFile != "/x/ca.pem" {
		t.Errorf("a2_url override: %+v %v", c, err)
	}
	p = h.writeConf("discord_api=http://example.com/api\n")
	if _, err := loadConfig(p); err == nil {
		t.Error("plain http to a non-loopback Discord API accepted")
	}
	p = h.writeConf("")
	os.Chmod(p, 0o644)
	if _, err := loadConfig(p); err == nil || !strings.Contains(err.Error(), "0600") {
		t.Errorf("0644 conf: %v", err)
	}
	os.Chmod(p, 0o600)
	os.Chmod(filepath.Join(h.dir, "dtok"), 0o644)
	c, _ := loadConfig(p)
	if _, err := newBridge(c, io.Discard); err == nil || strings.Contains(err.Error(), testDiscordToken) {
		t.Errorf("0644 token file: %v", err)
	}
	os.Chmod(filepath.Join(h.dir, "dtok"), 0o600)
	// a bridge member that may use work is refused
	h.a2.channels = []string{"team", "general"}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := h.bridge().run(ctx); err == nil || !strings.Contains(err.Error(), "general-only") {
		t.Errorf("work member: %v", err)
	}
	h.a2.channels = []string{"general"}
	h.a2.you = "alice"
	if err := h.bridge().run(ctx); err == nil || !strings.Contains(err.Error(), "want \"discord-bridge\"") {
		t.Errorf("wrong member: %v", err)
	}
}

func TestClip(t *testing.T) {
	// i18n fixture: Japanese text on purpose (clipping counts characters)
	long := strings.Repeat("あ", 3000)
	got := clip(long, maxDiscordRunes, 4*maxDiscordRunes)
	if n := utf8.RuneCountInString(got); n != maxDiscordRunes || !strings.HasSuffix(got, "…") {
		t.Errorf("runes %d", n)
	}
	got = clip(strings.Repeat("é", 20000), maxA2Runes, maxA2Bytes)
	if len(got) > maxA2Bytes || !utf8.ValidString(got) || !strings.HasSuffix(got, "…") {
		t.Errorf("bytes %d valid %v", len(got), utf8.ValidString(got))
	}
	if clip("short", 10, 100) != "short" {
		t.Error("short text changed")
	}
	m := a2Message{From: "alice", Text: strings.Repeat("x", 5000)}
	if n := utf8.RuneCountInString(toDiscordText(m)); n != maxDiscordRunes {
		t.Errorf("discord text %d runes", n)
	}
}

func TestPrefixDetection(t *testing.T) {
	for s, want := range map[string]bool{
		"**alice**: hi":        true,
		"↪ #12 **erin**: re":   true,
		"↪ #3":                 true,
		"Alice: hey":           false,
		"**bold** words":       false,
		"plain text":           false,
		"**a\nb**: no newline": false,
	} {
		if got := hasBridgePrefix(s); got != want {
			t.Errorf("hasBridgePrefix(%q)=%v", s, got)
		}
	}
}

func TestGatewayCloseCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := sha1.Sum([]byte(r.Header.Get("Sec-WebSocket-Key") + wsGUID))
		conn, brw, _ := w.(http.Hijacker).Hijack()
		defer conn.Close()
		fmt.Fprintf(brw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", base64.StdEncoding.EncodeToString(h[:]))
		brw.Write([]byte{0x88, 2, 0x0f, 0xa4}) // close 4004 authentication failed
		brw.Flush()
		bufio.NewReader(conn).ReadByte()
	}))
	defer srv.Close()
	err := gatewayIdentifyOnce(context.Background(), "ws"+strings.TrimPrefix(srv.URL, "http")+"/", testDiscordToken)
	if err == nil || !strings.Contains(err.Error(), "4004") || strings.Contains(err.Error(), testDiscordToken) {
		t.Errorf("err %v", err)
	}
}

func TestRefused(t *testing.T) {
	for st, want := range map[int]bool{400: true, 413: true, 422: true, 401: false, 403: false, 404: false, 429: false, 500: false} {
		if got := refused(&httpStatusError{Status: st}); got != want {
			t.Errorf("refused(%d)=%v", st, got)
		}
	}
	if refused(fmt.Errorf("network")) {
		t.Error("network error refused")
	}
}
