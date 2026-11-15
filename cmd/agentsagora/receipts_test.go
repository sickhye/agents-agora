package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// receiptsOf asks GET /receipts/{id} as who; returns code, name -> status, and the body.
func receiptsAs(t *testing.T, f *fixture, who string, id any) (int, map[string]string, map[string]any) {
	t.Helper()
	code, out := f.req(t, "GET", fmt.Sprintf("/receipts/%v", id), f.tokens[who], nil)
	st := map[string]string{}
	if rs, ok := out["receipts"].([]any); ok {
		for _, r := range rs {
			rm := r.(map[string]any)
			st[rm["name"].(string)] = rm["status"].(string)
		}
	}
	return code, st, out
}

func msgID(t *testing.T, out map[string]any) int64 {
	t.Helper()
	id, ok := out["id"].(float64)
	if !ok {
		t.Fatalf("no id in %v", out)
	}
	return int64(id)
}

// restartFixture builds a new server on the same config and state dirs.
func restartFixture(t *testing.T, f *fixture) {
	t.Helper()
	sv, err := newServer(testMembers, nil, denyFile)
	if err != nil {
		t.Fatal(err)
	}
	f.srv.Close()
	srv := httptest.NewServer(sv.handler())
	t.Cleanup(srv.Close)
	t.Cleanup(func() { sv.ws.dropAll(); sv.ws.wait(5 * time.Second) })
	f.srv, f.sv = srv, sv
}

func TestReceiptsDMAndWhoMaySee(t *testing.T) {
	f := setup(t)
	_, out := send(t, f, "alice", "team", "dave", "dm for dave")
	id := msgID(t, out)
	code, st, _ := receiptsAs(t, f, "alice", id)
	if code != 200 || len(st) != 1 || st["dave"] != rcPending {
		t.Fatalf("before fetch: %d %v", code, st)
	}
	// dave fetches over /inbox -> delivered
	if code, out := fetchInbox(t, f, "dave"); code != 200 || len(msgs(t, out)) != 1 {
		t.Fatalf("dave inbox %d %v", code, out)
	}
	code, st, body := receiptsAs(t, f, "alice", id)
	if code != 200 || st["dave"] != rcDelivered {
		t.Fatalf("after fetch: %d %v", code, st)
	}
	rs := body["receipts"].([]any)[0].(map[string]any)
	if ts, _ := time.Parse(time.RFC3339, rs["delivered"].(string)); ts.IsZero() || !strings.HasSuffix(rs["delivered"].(string), "+09:00") {
		t.Errorf("delivered time %v", rs["delivered"])
	}
	// nobody but the sender: recipient, other members, general-only members, a missing id
	_, missing := f.req(t, "GET", "/receipts/9999", f.tokens["alice"], nil)
	for _, who := range []string{"dave", "bob", "erin", "discord-bridge"} {
		code, st, out := receiptsAs(t, f, who, id)
		if code != 404 || len(st) != 0 || out["error"] != strings.Replace(missing["error"].(string), "9999", fmt.Sprint(id), 1) {
			t.Errorf("%s sees receipts of another's DM: %d %v", who, code, out)
		}
	}
	if code, _, _ := receiptsAs(t, f, "alice", "abc"); code != 400 {
		t.Errorf("bad id %d", code)
	}
	if code, _ := f.req(t, "GET", fmt.Sprintf("/receipts/%d", id), "", nil); code != 401 {
		t.Errorf("no token %d, want 401", code)
	}
	// MCP tool: same rule
	res, isErr := toolCall(t, f, "alice", "receipts", map[string]any{"id": id})
	if isErr || len(res["receipts"].([]any)) != 1 {
		t.Errorf("mcp receipts %v", res)
	}
	if res, isErr := toolCall(t, f, "dave", "receipts", map[string]any{"id": id}); !isErr {
		t.Errorf("mcp receipts for non-sender %v", res)
	}
}

func TestReceiptsToAllAndGeneralOnly(t *testing.T) {
	f := setup(t)
	_, out := send(t, f, "alice", "team", "all", "team to all")
	cid := msgID(t, out)
	_, st, _ := receiptsAs(t, f, "alice", cid)
	if len(st) != 3 || st["bob"] == "" || st["carol"] == "" || st["dave"] == "" {
		t.Fatalf("work recipients %v (want bob, carol, dave; no sender, no general-only)", st)
	}
	// bob over /ws, dave over the MCP inbox; carol fetches nothing
	_, fc := wsRaw(t, f.srv.URL, "/ws", bearer(f.tokens["bob"]))
	sendSince(t, fc, 0)
	if m, err := nextMsg(t, fc, 2*time.Second); err != nil || m.ID != cid {
		t.Fatalf("ws push %+v %v", m, err)
	}
	if res, isErr := toolCall(t, f, "dave", "inbox", map[string]any{"channel": "team"}); isErr || len(res["messages"].([]any)) != 1 {
		t.Fatalf("mcp inbox %v", res)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		_, st, _ = receiptsAs(t, f, "alice", cid)
		if st["bob"] == rcDelivered || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if st["bob"] != rcDelivered || st["dave"] != rcDelivered || st["carol"] != rcPending {
		t.Fatalf("to=all receipts %v", st)
	}
	// general to all: everyone but the sender, general-only members included
	_, out = send(t, f, "alice", "general", "all", "lunch at noon?")
	gid := msgID(t, out)
	_, st, _ = receiptsAs(t, f, "alice", gid)
	if len(st) != 6 || st["discord-bridge"] == "" || st["erin"] == "" || st["alice"] != "" {
		t.Errorf("general recipients %v", st)
	}
	// a general-only member: never sees work receipts, and its own general message's
	// receipts name only general members and say nothing about work
	for _, who := range []string{"discord-bridge", "erin"} {
		if code, st, _ := receiptsAs(t, f, who, cid); code != 404 || len(st) != 0 {
			t.Errorf("%s got work receipts %d %v", who, code, st)
		}
	}
	_, out = send(t, f, "erin", "general", "all", "hi all")
	xid := msgID(t, out)
	fetchInbox(t, f, "discord-bridge")
	code, st, body := receiptsAs(t, f, "erin", xid)
	raw, _ := json.Marshal(body)
	if code != 200 || st["discord-bridge"] != rcDelivered || st["erin"] != "" || strings.Contains(string(raw), "team") {
		t.Errorf("erin receipts %d %s", code, raw)
	}
	// discord-bridge fetched only general: no work delivery may be recorded for it
	if got, _ := f.sv.deliv.of(cid); len(got) != 2 || !hasKey(got, "bob") || !hasKey(got, "dave") {
		t.Errorf("work deliveries %v", got)
	}
}

func hasKey(m map[string]time.Time, k string) bool { _, ok := m[k]; return ok }

func TestReceiptsSurviveRestartAndRename(t *testing.T) {
	f := setup(t)
	_, out := send(t, f, "alice", "team", "bob", "before restart")
	id := msgID(t, out)
	fetchInbox(t, f, "bob")
	p := filepath.Join(stateDir(), deliveriesFile)
	if st, err := os.Stat(p); err != nil || wrongMode(st, 0o600) {
		t.Fatalf("deliveries file %v %v", st, err)
	}
	b, _ := os.ReadFile(p)
	if strings.Contains(string(b), "before restart") {
		t.Error("deliveries file holds message text")
	}
	restartFixture(t, f)
	if _, st, _ := receiptsAs(t, f, "alice", id); st["bob"] != rcDelivered {
		t.Fatalf("after restart %v", st)
	}
	if code := run([]string{"token", "rename", "bob", "alice-main"}); code != 0 {
		t.Fatalf("rename exit %d", code)
	}
	if _, st, _ := receiptsAs(t, f, "alice", id); len(st) != 1 || st["alice-main"] != rcDelivered {
		t.Fatalf("after rename %v", st)
	}
	// mail to the old name: pending, then delivered when the renamed member fetches
	_, out = send(t, f, "alice", "team", "bob", "to the old name")
	id2 := msgID(t, out)
	if _, st, _ := receiptsAs(t, f, "alice", id2); st["alice-main"] != rcPending {
		t.Fatalf("old-name DM %v", st)
	}
	f.req(t, "GET", fmt.Sprintf("/inbox?since=%d", id), f.tokens["bob"], nil) // same token after rename
	// the delivery is recorded right after the response is flushed (see fetchInbox)
	for end := time.Now().Add(2 * time.Second); time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
		if got, _ := f.sv.deliv.of(id2); len(got) > 0 {
			break
		}
	}
	if _, st, _ := receiptsAs(t, f, "alice", id2); st["alice-main"] != rcDelivered {
		t.Fatalf("old-name DM after fetch %v", st)
	}
	// the renamed sender still sees receipts of what it sent under the old name
	_, out = f.req(t, "POST", "/send", f.tokens["bob"], map[string]string{"channel": "team", "to": "dave", "text": "x"})
	if code, st, _ := receiptsAs(t, f, "bob", msgID(t, out)); code != 200 || st["dave"] != rcPending {
		t.Errorf("renamed sender %d %v", code, st)
	}
	restartFixture(t, f)
	if _, st, _ := receiptsAs(t, f, "alice", id2); st["alice-main"] != rcDelivered {
		t.Fatalf("after second restart %v", st)
	}
}

func TestReceiptsUnrecordedBeforeLog(t *testing.T) {
	f := setup(t)
	_, out := send(t, f, "alice", "team", "dave", "old message")
	id := msgID(t, out)
	os.Remove(filepath.Join(stateDir(), deliveriesFile)) // as on a server older than receipts
	restartFixture(t, f)
	if _, st, _ := receiptsAs(t, f, "alice", id); st["dave"] != rcUnrecorded {
		t.Fatalf("old message %v", st)
	}
	fetchInbox(t, f, "dave")
	if _, st, _ := receiptsAs(t, f, "alice", id); st["dave"] != rcDelivered {
		t.Fatalf("old message after fetch %v", st)
	}
}

func TestReceiptsView(t *testing.T) {
	v := setupView(t)
	_, cookie := v.login(t, v.viewerTok, "")
	_, out := send(t, v.fixture, "alice", "team", "dave", "dm")
	id := msgID(t, out)
	get := func(q string) (map[string][]receipt, int) {
		t.Helper()
		resp, body := v.do(t, "GET", "/view/api/messages?"+q, cookie, nil, "")
		var d struct {
			Receipts map[string][]receipt `json:"receipts"`
			RSeq     int                  `json:"rseq"`
		}
		if resp.StatusCode != 200 || json.Unmarshal([]byte(body), &d) != nil {
			t.Fatalf("view messages %d %s", resp.StatusCode, body)
		}
		return d.Receipts, d.RSeq
	}
	rc, seq := get("since=0")
	if r := rc[fmt.Sprint(id)]; len(r) != 1 || r[0].Name != "dave" || r[0].Status != rcPending {
		t.Fatalf("view pending %v", rc)
	}
	// live: the view socket gets a receipts frame when dave fetches
	csrf := v.csrf(t, cookie)
	_, fc := wsRaw(t, v.srv.URL, "/view/ws", map[string]string{"Cookie": viewCookie + "=" + cookie, "Origin": v.origin, "Sec-Fetch-Site": "same-origin"})
	b, _ := json.Marshal(map[string]any{"since": id, "csrf": csrf})
	fc.writeFrame(wsOpText, b)
	time.Sleep(100 * time.Millisecond)
	fetchInbox(t, v.fixture, "dave")
	fc.c.SetReadDeadline(time.Now().Add(2 * time.Second))
	got := ""
	for got == "" {
		op, p, err := fc.readFrame()
		if err != nil {
			t.Fatalf("view ws: %v", err)
		}
		if op == wsOpText {
			got = string(p)
		}
	}
	if got != `{"receipts":true}` {
		t.Errorf("view frame %s", got)
	}
	// the change shows with rseq even though no new message came
	rc, seq2 := get(fmt.Sprintf("since=%d&rseq=%d", id, seq))
	if r := rc[fmt.Sprint(id)]; seq2 <= seq || len(r) != 1 || r[0].Status != rcDelivered {
		t.Fatalf("view delivered %v seq %d->%d", rc, seq, seq2)
	}
	if rc, _ := get(fmt.Sprintf("since=%d&rseq=%d", id, seq2)); len(rc) != 0 {
		t.Errorf("no change, got %v", rc)
	}
	if resp, _ := v.do(t, "GET", "/view/api/messages?rseq=x", cookie, nil, ""); resp.StatusCode != 400 {
		t.Errorf("bad rseq %d", resp.StatusCode)
	}
	// the page script draws the marks
	_, js := v.do(t, "GET", "/view/app.js", cookie, nil, "")
	for _, want := range []string{`"rcpt"`, `"✓ "`, `"· "`, "rseq="} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js lacks %s", want)
		}
	}
	// an agent token does not open the view API
	if code, _ := v.req(t, "GET", "/view/api/messages?since=0", v.tokens["alice"], nil); code == 200 {
		t.Error("agent token read the view API")
	}
}

func TestReceiptsCLI(t *testing.T) {
	f := setup(t)
	_, out := send(t, f, "alice", "team", "all", "hello")
	id := msgID(t, out)
	fetchInbox(t, f, "dave")
	t.Setenv("AGENTSAGORA_URL", f.srv.URL)
	t.Setenv("AGENTSAGORA_TOKEN_FILE", filepath.Join(configDir(), "token-alice"))
	var code int
	got := captureStdout(t, func() { code = run([]string{"receipts", fmt.Sprint(id)}) })
	lines := strings.Split(strings.TrimSpace(got), "\n")
	if code != 0 || len(lines) != 4 || lines[0] != "undelivered 2 / 3" {
		t.Fatalf("exit %d output %q", code, got)
	}
	lines = lines[1:]
	want := map[string]bool{"bob: not yet": true, "carol: not yet": true}
	for _, l := range lines {
		if strings.HasPrefix(l, "dave: delivered ") && len(l) == len("dave: delivered 15:04:05") {
			continue
		}
		if !want[l] {
			t.Errorf("line %q", l)
		}
	}
	if code := run([]string{"receipts"}); code != 2 {
		t.Errorf("no id exit %d", code)
	}
	t.Setenv("AGENTSAGORA_TOKEN_FILE", filepath.Join(configDir(), "token-dave"))
	if code := run([]string{"receipts", fmt.Sprint(id)}); code != 1 {
		t.Errorf("non-sender exit %d", code)
	}
}

// failWriter is a ResponseWriter whose body writes always fail (client gone).
type failWriter struct{ h http.Header }

func (w *failWriter) Header() http.Header       { return w.h }
func (w *failWriter) WriteHeader(int)           {}
func (w *failWriter) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }

// MCP inbox records delivery only after the response write succeeded,
// the same rule as /inbox and /ws.
func TestReceiptsMCPInboxOnlyAfterWrite(t *testing.T) {
	f := setup(t)
	_, out := send(t, f, "alice", "team", "dave", "dm for dave over mcp")
	id := msgID(t, out)
	body := `{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"inbox","arguments":{"channel":"team"}}}`
	mk := func() *http.Request {
		r := httptest.NewRequest("POST", "/mcp", strings.NewReader(body))
		return r.WithContext(context.WithValue(r.Context(), ctxKey{}, "dave"))
	}
	// failed write: not delivered
	f.sv.handleMCP(&failWriter{h: http.Header{}}, mk())
	if got, _ := f.sv.deliv.of(id); len(got) != 0 {
		t.Fatalf("failed MCP write recorded delivery: %v", got)
	}
	if _, st, _ := receiptsAs(t, f, "alice", id); st["dave"] != rcPending {
		t.Fatalf("after failed write: %v", st)
	}
	// successful write: delivered
	rec := httptest.NewRecorder()
	f.sv.handleMCP(rec, mk())
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "dm for dave over mcp") {
		t.Fatalf("mcp inbox %d %s", rec.Code, rec.Body.String())
	}
	if got, _ := f.sv.deliv.of(id); len(got) != 1 || !hasKey(got, "dave") {
		t.Fatalf("successful MCP write not recorded: %v", got)
	}
	if _, st, _ := receiptsAs(t, f, "alice", id); st["dave"] != rcDelivered {
		t.Fatalf("after successful write: %v", st)
	}
}
