package main

import (
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func sendReply(t *testing.T, f *fixture, who, channel, to, text string, replyTo any) (int, map[string]any) {
	t.Helper()
	return f.req(t, "POST", "/send", f.tokens[who], map[string]any{"channel": channel, "to": to, "text": text, "reply_to": replyTo})
}

func idOf(out map[string]any) int64 { n, _ := out["id"].(float64); return int64(n) }

func TestReplyToHTTP(t *testing.T) {
	f := setup(t)
	code, root := send(t, f, "alice", "team", "all", "root")
	if code != 200 {
		t.Fatal("root send")
	}
	if _, ok := root["reply_to"]; ok {
		t.Fatalf("a message without reply_to must not carry the field: %v", root)
	}
	rid := idOf(root)
	code, r1 := sendReply(t, f, "bob", "team", "all", "first reply", rid)
	if code != 200 || idOf(r1) == 0 || int64(r1["reply_to"].(float64)) != rid {
		t.Fatalf("reply: %d %v", code, r1)
	}
	// A reply to a reply is stored under the root (threads are one level deep).
	code, r2 := sendReply(t, f, "dave", "team", "alice", "reply to the reply", idOf(r1))
	if code != 200 || int64(r2["reply_to"].(float64)) != rid {
		t.Fatalf("nested reply: %d %v", code, r2)
	}
	// Refusals: missing id, wrong channel, negative, a DM the sender cannot see.
	if code, out := sendReply(t, f, "alice", "team", "all", "x", 9999); code != 400 || !strings.Contains(out["error"].(string), "unknown reply_to") {
		t.Fatalf("missing parent: %d %v", code, out)
	}
	if code, out := sendReply(t, f, "alice", "general", "all", "x", rid); code != 400 || !strings.Contains(out["error"].(string), "channel team") {
		t.Fatalf("other channel: %d %v", code, out)
	}
	if code, _ := sendReply(t, f, "alice", "team", "all", "x", -1); code != 400 {
		t.Fatalf("negative reply_to: %d", code)
	}
	_, dm := send(t, f, "bob", "team", "carol", "private")
	if code, out := sendReply(t, f, "alice", "team", "all", "x", idOf(dm)); code != 400 || !strings.Contains(out["error"].(string), "unknown reply_to") {
		t.Fatalf("hidden DM parent must look missing: %d %v", code, out)
	}
	if code, _ := sendReply(t, f, "carol", "team", "bob", "ok", idOf(dm)); code != 200 {
		t.Fatalf("DM recipient replying in its thread: %d", code)
	}
	// Old clients: no field, same answer as before.
	if code, out := send(t, f, "alice", "team", "all", "plain"); code != 200 || out["reply_to"] != nil {
		t.Fatalf("old client: %d %v", code, out)
	}
	// Inbox carries reply_to.
	_, in := f.req(t, "GET", "/inbox?since="+strconv.FormatInt(rid, 10), f.tokens["carol"], nil)
	found := false
	for _, m := range msgs(t, in) {
		if m["text"] == "first reply" && int64(m["reply_to"].(float64)) == rid {
			found = true
		}
	}
	if !found {
		t.Fatal("inbox lost reply_to")
	}
	// reply_to survives a restart; lines without it load as 0.
	st, err := openStore(filepath.Join(os.Getenv("AGENTSAGORA_STATE_DIR"), "messages.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if m, ok := st.get(idOf(r2)); !ok || m.ReplyTo != rid {
		t.Fatalf("after reopen: %v %v", m, ok)
	}
	if m, ok := st.get(rid); !ok || m.ReplyTo != 0 {
		t.Fatalf("root after reopen: %v", m)
	}
	if _, ok := st.get(0); ok {
		t.Fatal("id 0 found")
	}
}

func TestReplyToMCPAndClientLine(t *testing.T) {
	f := setup(t)
	_, out := mcpCall(t, f, "bob", `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	b, _ := json.Marshal(out)
	if !strings.Contains(string(b), `"reply_to"`) {
		t.Fatal("send tool schema lacks reply_to")
	}
	root, isErr := toolCall(t, f, "bob", "send", map[string]any{"channel": "general", "text": "lunch?"})
	if isErr {
		t.Fatal(root)
	}
	rid := int64(root["message"].(map[string]any)["id"].(float64))
	rep, isErr := toolCall(t, f, "erin", "send", map[string]any{"channel": "general", "text": "yes", "reply_to": rid})
	if isErr || int64(rep["message"].(map[string]any)["reply_to"].(float64)) != rid {
		t.Fatalf("mcp reply: %v", rep)
	}
	// Old MCP clients send no reply_to.
	if res, isErr := toolCall(t, f, "erin", "send", map[string]any{"channel": "general", "text": "plain"}); isErr {
		t.Fatal(res)
	}
	if res, isErr := toolCall(t, f, "erin", "send", map[string]any{"channel": "general", "text": "x", "reply_to": 999}); !isErr {
		t.Fatalf("mcp missing parent accepted: %v", res)
	}
	line := formatLine(Message{ID: 5, Channel: "general", TS: "t", From: "a", To: "all", Text: "x", ReplyTo: 3})
	if !strings.Contains(line, "a -> all (re #3): x") {
		t.Fatalf("watch line: %s", line)
	}
	if line := formatLine(Message{ID: 5, Channel: "general", TS: "t", From: "a", To: "all", Text: "x"}); strings.Contains(line, "re #") {
		t.Fatalf("plain watch line changed: %s", line)
	}
}

func TestViewSendReplyTo(t *testing.T) {
	v := setupView(t)
	_, root := send(t, v.fixture, "alice", "team", "all", "root")
	_, c := v.login(t, v.viewerTok, "")
	tok := v.csrf(t, c)
	rid := strconv.FormatInt(idOf(root), 10)
	code, out := v.viewSend(t, c, v.origin, url.Values{"csrf": {tok}, "channel": {"team"}, "to": {"all"}, "text": {"thread answer"}, "reply_to": {rid}})
	if code != 200 || strconv.FormatInt(int64(out["reply_to"].(float64)), 10) != rid || out["from"] != viewerName {
		t.Fatalf("view reply: %d %v", code, out)
	}
	// The viewer may reply to any message, DMs included.
	_, dm := send(t, v.fixture, "bob", "team", "carol", "dm")
	if code, out := v.viewSend(t, c, v.origin, url.Values{"csrf": {tok}, "channel": {"team"}, "to": {"bob"}, "text": {"seen"}, "reply_to": {strconv.FormatInt(idOf(dm), 10)}}); code != 200 {
		t.Fatalf("view reply to DM: %d %v", code, out)
	}
	for _, bad := range []string{"abc", "-2", "99999"} {
		if code, _ := v.viewSend(t, c, v.origin, url.Values{"csrf": {tok}, "channel": {"team"}, "to": {"all"}, "text": {"x"}, "reply_to": {bad}}); code != 400 {
			t.Fatalf("reply_to %q: %d, want 400", bad, code)
		}
	}
	// The messages API shows the field.
	_, ms := v.messages(t, c)
	ok := false
	for _, m := range ms {
		if m["text"] == "thread answer" && m["reply_to"] != nil {
			ok = true
		}
	}
	if !ok {
		t.Fatal("view messages lack reply_to")
	}
}

func TestViewMessagesBefore(t *testing.T) {
	v := setupView(t)
	now := time.Now()
	for i := 0; i < viewMaxMessages+5; i++ {
		if _, err := v.sv.store.append(chTeam, "alice", "all", "m"+strconv.Itoa(i+1), "", now); err != nil {
			t.Fatal(err)
		}
	}
	_, c := v.login(t, v.viewerTok, "")
	get := func(q string) (int, []Message, bool) {
		resp, body := v.do(t, "GET", "/view/api/messages?"+q, c, nil, "")
		var out struct {
			Messages  []Message `json:"messages"`
			Truncated bool      `json:"truncated"`
		}
		json.Unmarshal([]byte(body), &out)
		return resp.StatusCode, out.Messages, out.Truncated
	}
	code, ms, tr := get("since=0")
	if code != 200 || len(ms) != viewMaxMessages || !tr || ms[0].ID != 6 {
		t.Fatalf("newest page: %d n=%d truncated=%v first=%d", code, len(ms), tr, ms[0].ID)
	}
	code, ms, tr = get("before=6")
	if code != 200 || len(ms) != 5 || tr || ms[0].ID != 1 || ms[4].ID != 5 {
		t.Fatalf("older page: %d %v %v", code, ms, tr)
	}
	code, ms, tr = get("before=" + strconv.Itoa(viewMaxMessages+3))
	if code != 200 || len(ms) != viewMaxMessages || !tr || ms[len(ms)-1].ID != viewMaxMessages+2 {
		t.Fatalf("before with more left: %d n=%d %v", code, len(ms), tr)
	}
	for _, q := range []string{"before=x", "before=-1", "since=1&before=3"} {
		if code, _, _ := get(q); code != 400 {
			t.Fatalf("%s: %d, want 400", q, code)
		}
	}
	if code, _ := v.messages(t, ""); code != 401 {
		t.Fatalf("before without session: %d", code)
	}
	resp, _ := v.do(t, "GET", "/view/api/messages?before=3", "", nil, "")
	if resp.StatusCode != 401 {
		t.Fatalf("before without session: %d", resp.StatusCode)
	}
}

func TestViewPageLayout(t *testing.T) {
	v := setupView(t)
	_, c := v.login(t, v.viewerTok, "")
	_, body := v.do(t, "GET", "/view", c, nil, "")
	for _, want := range []string{`id="side"`, `id="chans"`, `id="dms"`, `id="main"`, `id="thread"`, `id="th-send"`,
		`id="replay"`, `<option value="600">600x</option>`, `id="newbtn"`, `id="theme"`, `id="back"`, `action="/view/logout"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("page lacks %s", want)
		}
	}
	for _, want := range []string{"prefers-color-scheme:dark", "[data-theme=dark]", "max-width:760px", "body[data-pane=chat]"} {
		if !strings.Contains(viewCSS, want) {
			t.Fatalf("css lacks %s", want)
		}
	}
	for _, want := range []string{"reply_to", "before=", "localStorage", "isComposing"} {
		if !strings.Contains(viewJS, want) {
			t.Fatalf("script lacks %s", want)
		}
	}
	// Template literals would end the Go raw string; markup from data is never built.
	if strings.Contains(viewJS, "`") || strings.Contains(viewJS, "innerHTML") || strings.Contains(viewJS, ".style.cssText") {
		t.Fatal("script has a backtick, innerHTML or cssText")
	}
	resp, css := v.do(t, "GET", "/view/app.css", c, nil, "")
	if resp.StatusCode != 200 || css != viewCSS || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/css") {
		t.Fatalf("css route: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	resp, js := v.do(t, "GET", "/view/app.js", c, nil, "")
	if resp.StatusCode != 200 || js != viewJS || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/javascript") {
		t.Fatalf("js route: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
}
