package main

// Unread-count badge: the number of recipients a message was not delivered to yet,
// in the viewer UI (/view/api/messages "undelivered") and in GET /receipts/{id} and
// a2 receipts (first line "undelivered N / M").

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// viewBadges reads the badge map of the viewer UI with a fresh login.
func viewBadges(t *testing.T, v *viewFixture) map[string]int {
	t.Helper()
	v.sv.viewOrigin = "https://" + v.srv.Listener.Addr().String()
	v.origin = v.sv.viewOrigin
	_, cookie := v.login(t, v.viewerTok, "")
	resp, body := v.do(t, "GET", "/view/api/messages?since=0", cookie, nil, "")
	var d struct {
		Undelivered map[string]int `json:"undelivered"`
	}
	if resp.StatusCode != 200 || json.Unmarshal([]byte(body), &d) != nil || d.Undelivered == nil {
		t.Fatalf("view messages %d %s", resp.StatusCode, body)
	}
	return d.Undelivered
}

// fetchInbox is GET /inbox?since=0 as who, then waits until the server has recorded the
// delivery: /inbox records it after the response is flushed (server.go handleInbox), so a
// check made right after the response can run before the record.
func fetchInbox(t *testing.T, f *fixture, who string) (int, map[string]any) {
	t.Helper()
	code, out := f.req(t, "GET", "/inbox?since=0", f.tokens[who], nil)
	base, ok := currentNames().baseOf(who)
	if code != 200 || !ok {
		return code, out
	}
	ms, _ := out["messages"].([]any)
	deadline := time.Now().Add(2 * time.Second)
	for _, m := range ms {
		id := int64(m.(map[string]any)["id"].(float64))
		for {
			got, _ := f.sv.deliv.of(id)
			if _, done := got[base]; done {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("delivery of #%d to %s not recorded", id, who)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	return code, out
}

func badgeOf(t *testing.T, b map[string]int, id int64) (int, bool) {
	t.Helper()
	n, ok := b[fmt.Sprint(id)]
	return n, ok
}

func TestBadgeCountsAndDecrease(t *testing.T) {
	v := setupView(t)
	f := v.fixture
	_, out := send(t, f, "alice", "team", "dave", "dm for dave")
	dm := msgID(t, out)
	_, out = send(t, f, "alice", "team", "all", "team to all")
	ca := msgID(t, out)
	_, out = send(t, f, "alice", "general", "all", "general to all")
	ga := msgID(t, out)

	b := viewBadges(t, v)
	if n, ok := badgeOf(t, b, dm); !ok || n != 1 {
		t.Errorf("to=member badge %d %v, want 1", n, ok)
	}
	// work to=all: bob, carol, dave (no sender, no general-only member)
	if n, _ := badgeOf(t, b, ca); n != 3 {
		t.Errorf("work to=all badge %d, want 3", n)
	}
	if n, _ := badgeOf(t, b, ga); n != 6 {
		t.Errorf("general to=all badge %d, want 6", n)
	}
	// a general-only member's fetch changes general counts only
	fetchInbox(t, f, "discord-bridge")
	b = viewBadges(t, v)
	if n, _ := badgeOf(t, b, ca); n != 3 {
		t.Errorf("work badge after general-only fetch %d, want 3", n)
	}
	if n, _ := badgeOf(t, b, ga); n != 5 {
		t.Errorf("general badge after general-only fetch %d, want 5", n)
	}
	// dave fetches: the DM goes to 0 (kept as 0 so the page hides the badge), work to 2
	fetchInbox(t, f, "dave")
	b = viewBadges(t, v)
	if n, ok := badgeOf(t, b, dm); !ok || n != 0 {
		t.Errorf("to=member after fetch %d %v, want 0", n, ok)
	}
	if n, _ := badgeOf(t, b, ca); n != 2 {
		t.Errorf("work after dave fetch %d, want 2", n)
	}
	// the sender sees the same numbers; general-only members get the usual 404
	_, _, body := receiptsAs(t, f, "alice", ca)
	if body["undelivered"] != float64(2) || body["recipients"] != float64(3) {
		t.Errorf("receipts counts %v / %v", body["undelivered"], body["recipients"])
	}
	for _, who := range []string{"discord-bridge", "erin"} {
		code, _, out := receiptsAs(t, f, who, ca)
		raw, _ := json.Marshal(out)
		if code != 404 || strings.Contains(string(raw), "undelivered") || strings.Contains(string(raw), "recipients") {
			t.Errorf("%s got work counts %d %s", who, code, raw)
		}
	}
	// restart keeps the counts
	restartFixture(t, f)
	b = viewBadges(t, v)
	if n, _ := badgeOf(t, b, dm); n != 0 {
		t.Errorf("after restart dm %d", n)
	}
	if n, _ := badgeOf(t, b, ca); n != 2 {
		t.Errorf("after restart work %d", n)
	}
	if n, _ := badgeOf(t, b, ga); n != 4 {
		t.Errorf("after restart general %d, want 4", n)
	}
}

func TestBadgeNoneForOldMessages(t *testing.T) {
	v := setupView(t)
	f := v.fixture
	_, out := send(t, f, "alice", "team", "dave", "old message")
	id := msgID(t, out)
	os.Remove(filepath.Join(stateDir(), deliveriesFile)) // as on a server older than receipts
	restartFixture(t, f)
	if _, ok := badgeOf(t, viewBadges(t, v), id); ok {
		t.Error("badge for a message older than the delivery log")
	}
	_, _, body := receiptsAs(t, f, "alice", id)
	if _, ok := body["undelivered"]; ok || body["recipients"] != float64(1) {
		t.Errorf("old message counts %v", body)
	}
	if got := undeliveredLine(nil, 1); got != "undelivered ? / 1 (message predates recording)" {
		t.Errorf("old line %q", got)
	}
}

func TestBadgeUndeliveredHelper(t *testing.T) {
	rs := []receipt{{Status: rcDelivered}, {Status: rcPending}, {Status: rcPending}}
	if n, total, known := undelivered(rs); n != 2 || total != 3 || !known {
		t.Errorf("got %d %d %v", n, total, known)
	}
	if n, _, known := undelivered(rs[:1]); n != 0 || !known {
		t.Errorf("all delivered %d %v", n, known)
	}
	if _, _, known := undelivered([]receipt{{Status: rcDelivered}, {Status: rcUnrecorded}}); known {
		t.Error("unrecorded counted")
	}
}

func TestBadgePageScript(t *testing.T) {
	v := setupView(t)
	_, cookie := v.login(t, v.viewerTok, "")
	_, js := v.do(t, "GET", "/view/app.js", cookie, nil, "")
	for _, want := range []string{`"unr"`, "d.undelivered", "un > 0", "S.rcOpen"} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js lacks %s", want)
		}
	}
	_, css := v.do(t, "GET", "/view/app.css", cookie, nil, "")
	if !strings.Contains(css, ".unr{") {
		t.Error("app.css lacks .unr")
	}
}
