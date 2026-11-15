package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fixture struct {
	srv    *httptest.Server
	sv     *server
	tokens map[string]string
}

func setup(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("AGENTSAGORA_CONFIG_DIR", filepath.Join(dir, "config"))
	t.Setenv("AGENTSAGORA_STATE_DIR", filepath.Join(dir, "state"))
	toks := map[string]string{}
	for _, m := range testMembers {
		p, err := tokenNew(m, "", false)
		if err != nil {
			t.Fatal(err)
		}
		st, _ := os.Stat(p)
		if st.Mode().Perm() != 0o600 {
			t.Fatalf("token file mode %v, want 0600", st.Mode().Perm())
		}
		tok, err := readToken(p)
		if err != nil {
			t.Fatal(err)
		}
		toks[m] = tok
	}
	sv, err := newServer(testMembers, nil, denyFile)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(sv.handler())
	t.Cleanup(srv.Close)
	t.Cleanup(func() { sv.ws.dropAll(); sv.ws.wait(5 * time.Second) }) // sockets end before the temp dir goes
	return &fixture{srv: srv, sv: sv, tokens: toks}
}

func (f *fixture) req(t *testing.T, method, path, tok string, body any) (int, map[string]any) {
	t.Helper()
	var rd *bytes.Reader
	if body != nil {
		var b []byte
		switch v := body.(type) {
		case []byte:
			b = v
		default:
			b, _ = json.Marshal(v)
		}
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	r, _ := http.NewRequest(method, f.srv.URL+path, rd)
	if tok != "" {
		r.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out := map[string]any{}
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func msgs(t *testing.T, out map[string]any) []map[string]any {
	t.Helper()
	raw, _ := out["messages"].([]any)
	var res []map[string]any
	for _, r := range raw {
		res = append(res, r.(map[string]any))
	}
	return res
}

func TestAuthReject(t *testing.T) {
	f := setup(t)
	for _, tok := range []string{"", "sc_wrong", f.tokens["alice"] + "x"} {
		for _, ep := range []struct{ m, p string }{{"POST", "/send"}, {"GET", "/inbox"}, {"GET", "/members"}} {
			code, _ := f.req(t, ep.m, ep.p, tok, map[string]string{"to": "all", "text": "hi"})
			if code != http.StatusUnauthorized {
				t.Errorf("%s %s with token %q: code %d, want 401", ep.m, ep.p, tok, code)
			}
		}
	}
	if code, _ := f.req(t, "GET", "/healthz", "", nil); code != 200 {
		t.Errorf("healthz %d", code)
	}
	// A token whose member is not in the member list is rejected too.
	sv2, err := newServer([]string{"alice"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if sv2.tokens.member(f.tokens["bob"]) != "" {
		t.Error("token of a non-member accepted")
	}
}

func TestSpoofReject(t *testing.T) {
	f := setup(t)
	code, out := f.req(t, "POST", "/send", f.tokens["bob"], map[string]string{"from": "alice", "to": "carol", "text": "hello"})
	if code != 200 {
		t.Fatalf("send %d %v", code, out)
	}
	if out["from"] != "bob" {
		t.Errorf("from = %v, want bob (token's member)", out["from"])
	}
	_, in := f.req(t, "GET", "/inbox", f.tokens["carol"], nil)
	got := msgs(t, in)
	if len(got) != 1 || got[0]["from"] != "bob" {
		t.Errorf("inbox %v", got)
	}
	// The JSONL on disk also has the real sender.
	b, _ := os.ReadFile(filepath.Join(stateDir(), "messages.jsonl"))
	if !strings.Contains(string(b), `"from":"bob"`) || strings.Contains(string(b), `"from":"alice"`) {
		t.Errorf("jsonl %s", b)
	}
}

func TestInboxVisibility(t *testing.T) {
	f := setup(t)
	f.req(t, "POST", "/send", f.tokens["alice"], map[string]string{"to": "bob", "text": "dm"})
	f.req(t, "POST", "/send", f.tokens["alice"], map[string]string{"to": "all", "text": "everyone"})
	_, in := f.req(t, "GET", "/inbox", f.tokens["carol"], nil)
	if got := msgs(t, in); len(got) != 1 || got[0]["text"] != "everyone" {
		t.Errorf("carol sees %v, want only the 'all' message", got)
	}
	_, in = f.req(t, "GET", "/inbox", f.tokens["alice"], nil)
	if got := msgs(t, in); len(got) != 0 {
		t.Errorf("sender sees own messages: %v", got)
	}
	f.req(t, "POST", "/send", f.tokens["alice"], map[string]string{"to": "alice", "text": "note to self"})
	_, in = f.req(t, "GET", "/inbox", f.tokens["alice"], nil)
	if got := msgs(t, in); len(got) != 1 || got[0]["text"] != "note to self" {
		t.Errorf("self DM not in own inbox: %v", got)
	}
	if code, _ := f.req(t, "POST", "/send", f.tokens["alice"], map[string]string{"to": "nobody", "text": "x"}); code != 400 {
		t.Errorf("unknown recipient code %d, want 400", code)
	}
}

func TestInboxCursor(t *testing.T) {
	f := setup(t)
	t.Setenv("AGENTSAGORA_URL", f.srv.URL)
	t.Setenv("AGENTSAGORA_TOKEN_FILE", filepath.Join(configDir(), "token-alice"))
	for _, s := range []string{"one", "two"} {
		f.req(t, "POST", "/send", f.tokens["bob"], map[string]string{"to": "alice", "text": s})
	}
	out := captureStdout(t, func() {
		if err := cmdInbox(nil); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "#1 ") || !strings.Contains(out, "#2 ") || !strings.Contains(out, "bob -> alice: two") {
		t.Errorf("first inbox: %q", out)
	}
	if n := readCursor(cursorPath(f.srv.URL, os.Getenv("AGENTSAGORA_TOKEN_FILE"))); n != 2 {
		t.Errorf("cursor %d, want 2", n)
	}
	out = captureStdout(t, func() { cmdInbox(nil) })
	if strings.TrimSpace(out) != "no new messages" {
		t.Errorf("second inbox: %q", out)
	}
	f.req(t, "POST", "/send", f.tokens["carol"], map[string]string{"to": "all", "text": "line1\nline2"})
	out = captureStdout(t, func() { cmdInbox([]string{"--peek"}) })
	if !strings.Contains(out, `#3 `) || !strings.Contains(out, `line1\nline2`) || strings.Contains(out, "#1 ") {
		t.Errorf("third inbox: %q", out)
	}
	if n := readCursor(cursorPath(f.srv.URL, os.Getenv("AGENTSAGORA_TOKEN_FILE"))); n != 2 {
		t.Errorf("--peek moved cursor to %d", n)
	}
	// since= on the server side.
	_, in := f.req(t, "GET", "/inbox?since=2", f.tokens["alice"], nil)
	if got := msgs(t, in); len(got) != 1 || got[0]["id"].(float64) != 3 {
		t.Errorf("since=2: %v", got)
	}
}

func TestSizeLimit(t *testing.T) {
	f := setup(t)
	ok := strings.Repeat("a", maxTextBytes)
	if code, out := f.req(t, "POST", "/send", f.tokens["alice"], map[string]string{"to": "all", "text": ok}); code != 200 {
		t.Errorf("16KB text: %d %v", code, out)
	}
	if code, _ := f.req(t, "POST", "/send", f.tokens["alice"], map[string]string{"to": "all", "text": ok + "b"}); code != http.StatusRequestEntityTooLarge {
		t.Errorf("16KB+1 text: %d, want 413", code)
	}
	big := []byte(`{"to":"all","text":"` + strings.Repeat("a", maxBodyBytes+10) + `"}`)
	if code, _ := f.req(t, "POST", "/send", f.tokens["alice"], big); code != http.StatusRequestEntityTooLarge {
		t.Errorf("huge body: %d, want 413", code)
	}
	if code, _ := f.req(t, "POST", "/send", f.tokens["alice"], map[string]string{"to": "all", "text": "  "}); code != 400 {
		t.Errorf("empty text: %d, want 400", code)
	}
}

func TestRateLimit(t *testing.T) {
	f := setup(t)
	now := time.Date(2026, 11, 12, 10, 0, 0, 0, jst)
	f.sv.now = func() time.Time { return now }
	for i := 0; i < rateBurst; i++ {
		if code, _ := f.req(t, "POST", "/send", f.tokens["alice"], map[string]string{"to": "all", "text": "x"}); code != 200 {
			t.Fatalf("send %d: %d", i, code)
		}
	}
	if code, _ := f.req(t, "POST", "/send", f.tokens["alice"], map[string]string{"to": "all", "text": "x"}); code != 429 {
		t.Errorf("over burst: %d, want 429", code)
	}
	if code, _ := f.req(t, "POST", "/send", f.tokens["bob"], map[string]string{"to": "all", "text": "x"}); code != 200 {
		t.Errorf("other member limited: %d", code)
	}
	now = now.Add(2 * time.Second) // 30/min = one token per 2 s
	if code, _ := f.req(t, "POST", "/send", f.tokens["alice"], map[string]string{"to": "all", "text": "x"}); code != 200 {
		t.Errorf("after refill: %d", code)
	}
}

func TestLongPoll(t *testing.T) {
	f := setup(t)
	done := make(chan []map[string]any, 1)
	go func() {
		r, _ := http.NewRequest("GET", f.srv.URL+"/inbox?since=0&wait=10", nil)
		r.Header.Set("Authorization", "Bearer "+f.tokens["alice"])
		var out struct {
			Messages []map[string]any `json:"messages"`
		}
		if resp, err := http.DefaultClient.Do(r); err == nil {
			json.NewDecoder(resp.Body).Decode(&out)
			resp.Body.Close()
		}
		done <- out.Messages
	}()
	time.Sleep(200 * time.Millisecond)
	start := time.Now()
	f.req(t, "POST", "/send", f.tokens["bob"], map[string]string{"to": "alice", "text": "wake"})
	select {
	case got := <-done:
		if len(got) != 1 || got[0]["text"] != "wake" {
			t.Errorf("long-poll got %v", got)
		}
		if time.Since(start) > 2*time.Second {
			t.Errorf("long-poll woke late")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("long-poll did not wake")
	}
	// A message for someone else does not end the wait early.
	start = time.Now()
	go f.sv.store.append(chTeam, "bob", "carol", "not yours", "", time.Now())
	_, in := f.req(t, "GET", "/inbox?since=1&wait=1", f.tokens["alice"], nil)
	if got := msgs(t, in); len(got) != 0 || time.Since(start) < 900*time.Millisecond {
		t.Errorf("woke for another member's message: %v", got)
	}
}

func TestPersistence(t *testing.T) {
	f := setup(t)
	f.req(t, "POST", "/send", f.tokens["alice"], map[string]string{"to": "bob", "text": "kept"})
	sv2, err := newServer(testMembers, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := sv2.store.inbox("bob", 0, allChannels, emptyNames())
	if len(got) != 1 || got[0].Text != "kept" || !strings.HasSuffix(got[0].TS, "+09:00") {
		t.Errorf("after restart %v", got)
	}
	m, _ := sv2.store.append(chTeam, "alice", "all", "next", "", time.Now())
	if m.ID != 2 {
		t.Errorf("id after restart %d, want 2", m.ID)
	}
}

func TestSourceAllow(t *testing.T) {
	f := setup(t)
	allow, _ := parseAllow("198.51.100.0/24")
	f.sv.allow = allow
	if code, _ := f.req(t, "GET", "/healthz", "", nil); code != 403 {
		t.Errorf("127.0.0.1 outside allow: %d, want 403", code)
	}
	f.sv.allow = []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}
	if code, _ := f.req(t, "GET", "/healthz", "", nil); code != 200 {
		t.Errorf("127.0.0.1 inside allow: %d", code)
	}
}

func TestTokenNew(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AGENTSAGORA_CONFIG_DIR", dir)
	p, err := tokenNew("bob", filepath.Join(dir, "handover"), false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tokenNew("bob", filepath.Join(dir, "other"), false); err == nil {
		t.Error("second token without --force accepted")
	}
	tok, _ := readToken(p)
	h, _ := readHashes(tokensPath())
	if h["bob"] != hashToken(tok) {
		t.Error("hash mismatch")
	}
	b, _ := os.ReadFile(tokensPath())
	if strings.Contains(string(b), tok) {
		t.Error("tokens.json holds the plain token")
	}
	if st, _ := os.Stat(tokensPath()); st.Mode().Perm() != 0o600 {
		t.Errorf("tokens.json mode %v", st.Mode().Perm())
	}
	out := captureStdout(t, func() {
		if rc := run([]string{"token", "new", "--force", "--out", filepath.Join(dir, "t2"), "bob"}); rc != 0 {
			t.Errorf("rc %d", rc)
		}
	})
	tok2, _ := readToken(filepath.Join(dir, "t2"))
	if strings.TrimSpace(out) != filepath.Join(dir, "t2") || strings.Contains(out, tok2) {
		t.Errorf("token new stdout %q: want only the path", out)
	}
	if rc := run([]string{"token", "new", "nobody"}); rc != 2 {
		t.Errorf("non-member token rc %d, want 2", rc)
	}
	os.Chmod(p, 0o644)
	if _, err := readToken(p); err == nil {
		t.Error("0644 token file accepted")
	}
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, _ := os.Pipe()
	old := os.Stdout
	os.Stdout = w
	fn()
	w.Close()
	os.Stdout = old
	var buf bytes.Buffer
	buf.ReadFrom(r)
	return buf.String()
}
