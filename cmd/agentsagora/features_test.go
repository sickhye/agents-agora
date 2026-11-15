package main

import (
	"bytes"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// denyFile is the example deny list (a copy of examples/general-deny.txt).
var denyFile = filepath.Join("testdata", "general-deny.txt")

// tinyPNG is a valid 1x1 PNG.
var tinyPNG, _ = base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==")

func send(t *testing.T, f *fixture, who, channel, to, text string) (int, map[string]any) {
	t.Helper()
	return f.req(t, "POST", "/send", f.tokens[who], map[string]string{"channel": channel, "to": to, "text": text})
}

func TestChannelsACL(t *testing.T) {
	f := setup(t)
	if code, out := send(t, f, "alice", "team", "all", "build is green"); code != 200 || out["channel"] != "team" {
		t.Fatalf("work send %d %v", code, out)
	}
	if code, _ := send(t, f, "alice", "general", "all", "lunch at noon?"); code != 200 {
		t.Fatalf("general send %d", code)
	}
	// general-only member: cannot write or read work.
	if code, _ := send(t, f, "erin", "team", "all", "hi"); code != 403 {
		t.Errorf("erin work write %d, want 403", code)
	}
	if code, _ := f.req(t, "GET", "/inbox?channel=team", f.tokens["erin"], nil); code != 403 {
		t.Errorf("erin work read %d, want 403", code)
	}
	_, in := f.req(t, "GET", "/inbox", f.tokens["erin"], nil)
	got := msgs(t, in)
	if len(got) != 1 || got[0]["channel"] != "general" {
		t.Errorf("erin inbox %v, want only the general message", got)
	}
	// no DM to a general-only member on work, unknown channel rejected.
	if code, _ := send(t, f, "alice", "team", "frank", "x"); code != 400 {
		t.Errorf("work DM to frank %d, want 400", code)
	}
	if code, _ := send(t, f, "alice", "random", "all", "x"); code != 400 {
		t.Errorf("unknown channel %d, want 400", code)
	}
	// old client without channel: full member -> team, general-only -> general.
	if _, out := f.req(t, "POST", "/send", f.tokens["bob"], map[string]string{"to": "all", "text": "old client"}); out["channel"] != "team" {
		t.Errorf("default channel for bob %v", out["channel"])
	}
	if _, out := f.req(t, "POST", "/send", f.tokens["frank"], map[string]string{"to": "all", "text": "old client"}); out["channel"] != "general" {
		t.Errorf("default channel for frank %v", out["channel"])
	}
	// channel filter on inbox
	_, in = f.req(t, "GET", "/inbox?channel=general", f.tokens["dave"], nil)
	for _, m := range msgs(t, in) {
		if m["channel"] != "general" {
			t.Errorf("channel filter leaked %v", m)
		}
	}
	_, mem := f.req(t, "GET", "/members", f.tokens["frank"], nil)
	if ch, _ := mem["your_channels"].([]any); len(ch) != 1 || ch[0] != "general" {
		t.Errorf("frank channels %v", mem["your_channels"])
	}
}

func TestOldMessagesLoadAsTeam(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "m.jsonl")
	os.WriteFile(p, []byte(`{"id":1,"ts":"2026-11-12T10:00:00+09:00","from":"alice","to":"all","text":"old"}`+"\n"), 0o600)
	s, err := openStore(p)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := s.inbox("erin", 0, channelsOf("erin"), emptyNames()); len(got) != 0 {
		t.Errorf("old message visible in general: %v", got)
	}
	if got, _ := s.inbox("dave", 0, channelsOf("dave"), emptyNames()); len(got) != 1 || got[0].Channel != "team" {
		t.Errorf("old message %v", got)
	}
}

func TestGeneralLint(t *testing.T) {
	f := setup(t)
	bad := map[string]string{
		"work name": "did you see the Acme build",
		// i18n fixtures: Japanese text on purpose
		"work name 2": "acme の話",
		"jp mix":      "orionのサーバー",
		"github":      "see git.acme.example/foo",
		"orion":       "orion-server is down",
		"jira":        "ticket in Jira",
		"jenkins":     "jenkins job failed",
		"kubectl":     "kubectl apply",
		"pr hash":     "look at #123",
		"PR word":     "my PR is ready",
		"home path":   "/ho" + "me/alex/x",
		"srv":         "copy to /srv/work/a",
		"ip 10":       "server at 10.0.0.1",
		"ip 172":      "172.20.1.5 answered",
		"ip 192":      "ping 192.1" + "68.1.5",
		"ghp":         "token ghp_abcdefghijklmnop",
		"chat token":  "xoxb-1234-5678",
		"sk":          "key sk-abcdef123456",
		"long hex":    "0123456789abcdef0123456789abcdef",
		"long base64": "QUJDREVGR0hJSktMTU5PUFFSU1RVVldYWVphYmNkZWZnaGlqa2xtbm9w",
		"pem":         "-----BEGIN OPENSSH PRIVATE KEY-----",
	}
	for name, text := range bad {
		code, out := send(t, f, "frank", "general", "all", text)
		if code != 422 || !strings.Contains(out["error"].(string), "work content goes to team") {
			t.Errorf("%s: %q -> %d %v, want 422 lint", name, text, code, out)
		}
		if s, _ := out["error"].(string); strings.Contains(s, text) {
			t.Errorf("%s: error echoes the text", name)
		}
	}
	// same text is fine in work
	if code, _ := send(t, f, "alice", "team", "all", "look at #123 on 10.0.0.1"); code != 200 {
		t.Errorf("work with markers %d", code)
	}
	// false positives
	for _, ok := range []string{
		"Let's grab lunch at 12:30 and talk about the new movie, it was 10 out of 10.",
		"The decisions about specs can wait; xoxo, see you on 11/12.",
		// i18n fixture: Korean text on purpose (must not trigger the lint)
		"오늘 저녁 뭐 먹을까? 피자 어때",
		"I made 3 images of a cat in a spacesuit, version 2.5 looks best",
	} {
		if code, out := send(t, f, "frank", "general", "all", ok); code != 200 {
			t.Errorf("false positive %q: %d %v", ok, code, out)
		}
	}
	// a server without the deny list refuses general writes (fail closed)
	if _, err := newServer(testMembers, nil, filepath.Join(t.TempDir(), "missing.txt")); err == nil {
		t.Error("server started without the deny list")
	}
}

func putFile(t *testing.T, f *fixture, who, channel, name, mimeT string, data []byte) (int, map[string]any) {
	t.Helper()
	r, _ := http.NewRequest("POST", f.srv.URL+"/files?channel="+channel+"&name="+name, bytes.NewReader(data))
	r.Header.Set("Authorization", "Bearer "+f.tokens[who])
	r.Header.Set("Content-Type", mimeT)
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out := map[string]any{}
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestFiles(t *testing.T) {
	f := setup(t)
	code, out := putFile(t, f, "frank", "general", "cat.png", "image/png", tinyPNG)
	if code != 200 {
		t.Fatalf("put %d %v", code, out)
	}
	id := out["file"].(map[string]any)["id"].(string)
	if !fileIDRe.MatchString(id) {
		t.Fatalf("id %q", id)
	}
	// stored on disk under the random id only
	if _, err := os.Stat(filepath.Join(stateDir(), "files", id+".bin")); err != nil {
		t.Error(err)
	}
	r, _ := http.NewRequest("GET", f.srv.URL+"/files/"+id, nil)
	r.Header.Set("Authorization", "Bearer "+f.tokens["alice"])
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !bytes.Equal(body, tinyPNG) || resp.Header.Get("Content-Type") != "image/png" ||
		!strings.HasPrefix(resp.Header.Get("Content-Disposition"), "attachment") || resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("get: %d bytes, headers %v", len(body), resp.Header)
	}
	// the announcement message is in general
	_, in := f.req(t, "GET", "/inbox?channel=general", f.tokens["alice"], nil)
	if got := msgs(t, in); len(got) != 1 || got[0]["file_id"] != id {
		t.Errorf("announcement %v", got)
	}
	// work file is not readable or listable by a general-only member
	code, out = putFile(t, f, "alice", "team", "spec.md", "text/markdown", []byte("# notes\nwork"))
	if code != 200 {
		t.Fatalf("work put %d %v", code, out)
	}
	cid := out["file"].(map[string]any)["id"].(string)
	if code, _ := f.req(t, "GET", "/files/"+cid, f.tokens["erin"], nil); code != 403 {
		t.Errorf("erin get work file %d, want 403", code)
	}
	_, lst := f.req(t, "GET", "/files", f.tokens["erin"], nil)
	if fl, _ := lst["files"].([]any); len(fl) != 1 {
		t.Errorf("erin list %v", lst)
	}
	if code, _ := f.req(t, "GET", "/files?channel=team", f.tokens["erin"], nil); code != 403 {
		t.Errorf("erin list work %d", code)
	}
	if code, _ := putFile(t, f, "erin", "team", "a.png", "image/png", tinyPNG); code != 403 {
		t.Errorf("erin put work %d", code)
	}
	// MIME rules
	cases := []struct {
		name, mime string
		data       []byte
		want       int
	}{
		{"x.png", "image/png", []byte("GIF89a fake"), 415},
		{"x.html", "text/html", []byte("<script>"), 415},
		{"x.svg", "image/svg+xml", []byte("<svg/>"), 415},
		{"x.exe", "application/octet-stream", []byte("MZ"), 415},
		{"x.txt", "text/plain", []byte{0xff, 0xfe, 0}, 415},
		{"x.pdf", "application/pdf", []byte("%PDF-1.4 tiny"), 200},
		{"x.gif", "image/gif", []byte("GIF89a...."), 200},
		{"x.jpg", "image/jpeg", []byte{0xFF, 0xD8, 0xFF, 0xE0, 0}, 200},
		{"x.webp", "image/webp", []byte("RIFF\x00\x00\x00\x00WEBPVP8 "), 200},
		{"x.txt", "text/plain; charset=utf-8", []byte("hello"), 200},
		{"..%2F..%2Fetc%2Fpasswd", "text/plain", []byte("x"), 400},
		{"a%5Cb.txt", "text/plain", []byte("x"), 400},
		{"..", "text/plain", []byte("x"), 400},
		{"empty.txt", "text/plain", nil, 400},
	}
	for _, c := range cases {
		if code, out := putFile(t, f, "frank", "general", c.name, c.mime, c.data); code != c.want {
			t.Errorf("put %s %s: %d %v, want %d", c.name, c.mime, code, out, c.want)
		}
	}
	// lint on file name and text content in general
	if code, _ := putFile(t, f, "frank", "general", "acme-shot.png", "image/png", tinyPNG); code != 422 {
		t.Errorf("lint file name %d", code)
	}
	if code, _ := putFile(t, f, "frank", "general", "a.txt", "text/plain", []byte("see /ho"+"me/alex/x")); code != 422 {
		t.Errorf("lint text content %d", code)
	}
	// path traversal on download
	for _, p := range []string{"/files/..%2F..%2Fmessages.jsonl", "/files/" + id + ".json", "/files/%2e%2e", "/files/ABCDEF0123456789abcdef01"} {
		if code, _ := f.req(t, "GET", p, f.tokens["alice"], nil); code != 404 {
			t.Errorf("GET %s: %d, want 404", p, code)
		}
	}
}

func TestFileLimits(t *testing.T) {
	f := setup(t)
	f.sv.files.maxFile = 100
	f.sv.files.maxTotal = 250
	big := append([]byte("hello "), bytes.Repeat([]byte("a "), 50)...)
	if code, _ := putFile(t, f, "frank", "general", "big.txt", "text/plain", big); code != 413 {
		t.Errorf("over max file %d, want 413", code)
	}
	var ids []string
	for i := 0; i < 3; i++ {
		_, out := putFile(t, f, "frank", "general", "a.txt", "text/plain", bytes.Repeat([]byte("b "), 50))
		ids = append(ids, out["file"].(map[string]any)["id"].(string))
	}
	// 3 x 100 > 250: the oldest is gone
	if code, _ := f.req(t, "GET", "/files/"+ids[0], f.tokens["frank"], nil); code != 404 {
		t.Errorf("oldest file still there: %d", code)
	}
	if code, _ := f.req(t, "GET", "/files/"+ids[2], f.tokens["frank"], nil); code != 200 {
		t.Errorf("newest file: %d", code)
	}
	if f.sv.files.total > 250 {
		t.Errorf("total %d", f.sv.files.total)
	}
	// reopen keeps the two newest
	fs2, err := openFileStore(filepath.Join(stateDir(), "files"))
	if err != nil || len(fs2.files) != 2 || fs2.files[1].ID != ids[2] {
		t.Errorf("reopen %v %v", err, fs2.files)
	}
}

func mcpCall(t *testing.T, f *fixture, who string, body string) (int, map[string]any) {
	t.Helper()
	r, _ := http.NewRequest("POST", f.srv.URL+"/mcp", strings.NewReader(body))
	if who != "" {
		r.Header.Set("Authorization", "Bearer "+f.tokens[who])
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out := map[string]any{}
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func toolCall(t *testing.T, f *fixture, who, name string, args any) (map[string]any, bool) {
	t.Helper()
	a, _ := json.Marshal(args)
	code, out := mcpCall(t, f, who, `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"`+name+`","arguments":`+string(a)+`}}`)
	if code != 200 || out["error"] != nil {
		t.Fatalf("tools/call %s: %d %v", name, code, out)
	}
	res := out["result"].(map[string]any)
	sc, _ := res["structuredContent"].(map[string]any)
	if res["isError"] == true {
		return map[string]any{"error": res["content"].([]any)[0].(map[string]any)["text"]}, true
	}
	return sc, false
}

func TestMCP(t *testing.T) {
	f := setup(t)
	code, out := mcpCall(t, f, "bob", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"0"}}}`)
	if code != 200 || out["result"].(map[string]any)["protocolVersion"] != "2025-06-18" {
		t.Fatalf("initialize %d %v", code, out)
	}
	if code, _ := mcpCall(t, f, "bob", `{"jsonrpc":"2.0","method":"notifications/initialized"}`); code != 202 {
		t.Errorf("notification %d, want 202", code)
	}
	_, out = mcpCall(t, f, "bob", `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	tools := out["result"].(map[string]any)["tools"].([]any)
	var names []string
	for _, tl := range tools {
		names = append(names, tl.(map[string]any)["name"].(string))
	}
	if strings.Join(names, ",") != "send,inbox,members,put_file,get_file,list_files,receipts,vote_new,vote_cast,vote_show" {
		t.Errorf("tools %v", names)
	}
	if res, isErr := toolCall(t, f, "bob", "send", map[string]any{"channel": "team", "to": "alice", "text": "via mcp"}); isErr {
		t.Fatalf("send %v", res)
	}
	res, _ := toolCall(t, f, "alice", "inbox", map[string]any{"channel": "team"})
	m := res["messages"].([]any)
	if len(m) != 1 || m[0].(map[string]any)["text"] != "via mcp" || res["next_since"].(float64) != 1 {
		t.Errorf("inbox %v", res)
	}
	res, _ = toolCall(t, f, "alice", "inbox", map[string]any{"since": 1})
	if len(res["messages"].([]any)) != 0 {
		t.Errorf("inbox since=1 %v", res)
	}
	// ACL and lint come back as tool errors
	if res, isErr := toolCall(t, f, "erin", "inbox", map[string]any{"channel": "team"}); !isErr || !strings.Contains(res["error"].(string), "not allowed") {
		t.Errorf("erin work inbox %v", res)
	}
	if res, isErr := toolCall(t, f, "erin", "send", map[string]any{"channel": "general", "text": "acme deploy"}); !isErr || !strings.Contains(res["error"].(string), "work content goes to team") {
		t.Errorf("lint %v", res)
	}
	if _, isErr := toolCall(t, f, "erin", "send", map[string]any{"channel": "general", "text": "nice weather"}); isErr {
		t.Error("erin general send failed")
	}
	// files
	res, isErr := toolCall(t, f, "frank", "put_file", map[string]any{"channel": "general", "name": "dot.png", "mime": "image/png", "base64": base64.StdEncoding.EncodeToString(tinyPNG)})
	if isErr {
		t.Fatalf("put_file %v", res)
	}
	id := res["file"].(map[string]any)["id"].(string)
	res, _ = toolCall(t, f, "erin", "get_file", map[string]any{"id": id})
	if got, _ := base64.StdEncoding.DecodeString(res["base64"].(string)); !bytes.Equal(got, tinyPNG) {
		t.Errorf("get_file round trip failed")
	}
	res, _ = toolCall(t, f, "erin", "list_files", map[string]any{"channel": "general"})
	if len(res["files"].([]any)) != 1 {
		t.Errorf("list_files %v", res)
	}
	if res, isErr := toolCall(t, f, "frank", "get_file", map[string]any{"id": "../../tokens.json"}); !isErr {
		t.Errorf("traversal id %v", res)
	}
	res, _ = toolCall(t, f, "dave", "members", map[string]any{})
	if res["you"] != "dave" || len(res["members"].([]any)) != len(testMembers) {
		t.Errorf("members %v", res)
	}
	// unknown args, unknown tool, unknown method, batch, no auth, Origin
	if _, isErr := toolCall(t, f, "dave", "send", map[string]any{"channel": "general", "text": "x", "from": "alice"}); !isErr {
		t.Error("unknown argument accepted")
	}
	if _, out := mcpCall(t, f, "dave", `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"rm","arguments":{}}}`); out["error"] == nil {
		t.Error("unknown tool accepted")
	}
	if _, out := mcpCall(t, f, "dave", `{"jsonrpc":"2.0","id":4,"method":"resources/list"}`); out["error"].(map[string]any)["code"].(float64) != -32601 {
		t.Errorf("unknown method %v", out)
	}
	if code, _ := mcpCall(t, f, "dave", `[{"jsonrpc":"2.0","id":5,"method":"ping"}]`); code != 400 {
		t.Errorf("batch %d", code)
	}
	if code, _ := mcpCall(t, f, "", `{"jsonrpc":"2.0","id":6,"method":"tools/list"}`); code != 401 {
		t.Errorf("no token %d", code)
	}
	r, _ := http.NewRequest("POST", f.srv.URL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	r.Header.Set("Authorization", "Bearer "+f.tokens["dave"])
	r.Header.Set("Origin", "http://evil.example")
	if resp, _ := http.DefaultClient.Do(r); resp.StatusCode != 403 {
		t.Errorf("Origin %d", resp.StatusCode)
	}
	if code, _ := f.req(t, "GET", "/mcp", f.tokens["dave"], nil); code != 405 {
		t.Errorf("GET /mcp %d", code)
	}
}

func TestMCPStdioBridge(t *testing.T) {
	f := setup(t)
	t.Setenv("AGENTSAGORA_URL", f.srv.URL)
	t.Setenv("AGENTSAGORA_TOKEN_FILE", filepath.Join(configDir(), "token-bob"))
	c, err := newClient()
	if err != nil {
		t.Fatal(err)
	}
	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"send","arguments":{"channel":"general","text":"from stdio"}}}`,
	}, "\n") + "\n"
	var out bytes.Buffer
	if err := mcpBridge(c, strings.NewReader(in), &out); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], `"protocolVersion":"2025-03-26"`) || !strings.Contains(lines[1], `"isError":false`) {
		t.Errorf("bridge output %q", out.String())
	}
	// a bad token gives a JSON-RPC error line, not a crash
	c.token = "sc_bad"
	out.Reset()
	mcpBridge(c, strings.NewReader(`{"jsonrpc":"2.0","id":9,"method":"ping"}`+"\n"), &out)
	if !strings.Contains(out.String(), `"id":9`) || !strings.Contains(out.String(), "401") {
		t.Errorf("bad token output %q", out.String())
	}
}

func TestTokenRevokeRotate(t *testing.T) {
	f := setup(t)
	old := f.tokens["frank"]
	if rc := run([]string{"token", "rotate", "frank"}); rc != 0 {
		t.Fatalf("rotate rc %d", rc)
	}
	if code, _ := f.req(t, "GET", "/members", old, nil); code != 401 {
		t.Errorf("old token after rotate %d", code)
	}
	nt, _ := readToken(filepath.Join(configDir(), "token-frank"))
	if code, _ := f.req(t, "GET", "/members", nt, nil); code != 200 {
		t.Errorf("new token %d", code)
	}
	if rc := run([]string{"token", "revoke", "frank"}); rc != 0 {
		t.Fatalf("revoke rc %d", rc)
	}
	if code, _ := f.req(t, "GET", "/members", nt, nil); code != 401 {
		t.Errorf("revoked token %d", code)
	}
	if _, err := os.Stat(filepath.Join(configDir(), "token-frank")); !os.IsNotExist(err) {
		t.Error("token file kept after revoke")
	}
	if rc := run([]string{"token", "rotate", "frank"}); rc == 0 {
		t.Error("rotate without a token succeeded")
	}
}

func TestAuditLog(t *testing.T) {
	f := setup(t)
	send(t, f, "alice", "team", "all", "secret plan words")
	send(t, f, "frank", "general", "all", "acme leak")
	putFile(t, f, "frank", "general", "cat.png", "image/png", tinyPNG)
	f.req(t, "GET", "/members", "sc_wrongtoken", nil)
	b, err := os.ReadFile(filepath.Join(stateDir(), "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, bad := range []string{"secret plan", "acme leak", "sc_wrongtoken", f.tokens["alice"]} {
		if strings.Contains(s, bad) {
			t.Errorf("audit log holds %q", bad)
		}
	}
	for _, want := range []string{`"member":"alice"`, `"action":"send"`, `"channel":"team"`, `"status":422`, `"action":"put_file"`, `"action":"auth-fail`} {
		if !strings.Contains(s, want) {
			t.Errorf("audit log misses %s:\n%s", want, s)
		}
	}
	if st, _ := os.Stat(filepath.Join(stateDir(), "audit.jsonl")); wrongMode(st, 0o600) {
		t.Errorf("audit mode %v", st.Mode().Perm())
	}
}

func TestAuthFailLimit(t *testing.T) {
	f := setup(t)
	last := 0
	for i := 0; i < authFailBurst+2; i++ {
		last, _ = f.req(t, "GET", "/members", "sc_nope", nil)
	}
	if last != 429 {
		t.Errorf("after %d bad tokens: %d, want 429", authFailBurst+2, last)
	}
}

func TestCheckBind(t *testing.T) {
	for _, a := range []string{"0.0.0.0:18765", "[::]:18765", ":18765", "example.com:18765"} {
		if err := checkBind(a); err == nil {
			t.Errorf("%s accepted", a)
		}
	}
	if err := checkBind("127.0.0.1:18765"); err != nil {
		t.Error(err)
	}
}

func TestTLS(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AGENTSAGORA_CONFIG_DIR", filepath.Join(dir, "config"))
	t.Setenv("AGENTSAGORA_STATE_DIR", filepath.Join(dir, "state"))
	tdir := filepath.Join(dir, "tls")
	out := captureStdout(t, func() {
		if rc := run([]string{"tls", "init", "--dir", tdir, "--hosts", "127.0.0.1,localhost"}); rc != 0 {
			t.Fatalf("tls init rc %d", rc)
		}
	})
	if strings.Contains(out, "PRIVATE KEY") || !strings.Contains(out, filepath.Join(tdir, "ca.pem")) {
		t.Errorf("tls init output %q", out)
	}
	for f, want := range map[string]os.FileMode{"ca-key.pem": 0o600, "server-key.pem": 0o600, "ca.pem": 0o644, "server.pem": 0o644} {
		st, err := os.Stat(filepath.Join(tdir, f))
		if err != nil || wrongMode(st, want) {
			t.Errorf("%s mode %v err %v", f, st.Mode().Perm(), err)
		}
	}
	if rc := run([]string{"tls", "init", "--dir", tdir}); rc == 0 {
		t.Error("second tls init without --force succeeded")
	}
	cert, err := loadServerCert(filepath.Join(tdir, "server.pem"), filepath.Join(tdir, "server-key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	p, _ := tokenNew("dave", "", false)
	sv, err := newServer(testMembers, nil, denyFile)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(sv.handler())
	srv.TLS = serverTLSConfig(cert)
	srv.StartTLS()
	defer srv.Close()
	t.Setenv("AGENTSAGORA_URL", srv.URL)
	t.Setenv("AGENTSAGORA_TOKEN_FILE", p)
	t.Setenv("AGENTSAGORA_CA_FILE", filepath.Join(tdir, "ca.pem"))
	if rc := run([]string{"send", "--channel", "general", "--to", "all", "hello over tls"}); rc != 0 {
		t.Errorf("send over pinned TLS rc %d", rc)
	}
	// another CA is refused
	other := filepath.Join(dir, "other")
	tlsInit(other, "127.0.0.1", false, sv.now())
	t.Setenv("AGENTSAGORA_CA_FILE", filepath.Join(other, "ca.pem"))
	if rc := run([]string{"members"}); rc == 0 {
		t.Error("client accepted a server cert from another CA")
	}
	// no CA, or plain http to a LAN address: refused before any request
	t.Setenv("AGENTSAGORA_CA_FILE", "")
	if _, err := newClient(); err == nil {
		t.Error("https without CA accepted")
	}
	t.Setenv("AGENTSAGORA_URL", "http://192.0.2.10:18765")
	if _, err := newClient(); err == nil {
		t.Error("plain http to the LAN accepted")
	}
	// TLS 1.2 is refused by the server
	c12 := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true, MaxVersion: tls.VersionTLS12}}}
	if _, err := c12.Get(srv.URL + "/healthz"); err == nil {
		t.Error("TLS 1.2 accepted")
	}
	// a 0644 key is refused (no Unix modes on Windows)
	if unixModes {
		os.Chmod(filepath.Join(tdir, "server-key.pem"), 0o644)
		if _, err := loadServerCert(filepath.Join(tdir, "server.pem"), filepath.Join(tdir, "server-key.pem")); err == nil {
			t.Error("0644 key accepted")
		}
	}
	// the name-constrained CA cannot vouch for another host
	if _, _, err := splitHosts("a b"); err == nil {
		t.Error("bad host accepted")
	}
}
