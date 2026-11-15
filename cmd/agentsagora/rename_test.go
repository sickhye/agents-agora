package main

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRenameNameCheck(t *testing.T) {
	setup(t)
	bad := []string{"Alice-admin", "alice_admin", "alice--admin", "-alice", "alice-", "", "a lice", strings.Repeat("a", 41), "all", "viewer", "carol", "alice-ädmin"}
	for _, n := range bad {
		if err := renameMember("bob", n, "local-cli", time.Now()); err == nil {
			t.Errorf("name %q accepted", n)
		}
	}
	if err := renameMember("bob", strings.Repeat("a", 40), "local-cli", time.Now()); err != nil {
		t.Errorf("40 characters refused: %v", err)
	}
	if err := renameMember("nobody", "alice-x", "local-cli", time.Now()); err == nil {
		t.Error("unknown member renamed")
	}
	if code := run([]string{"token", "rename", "viewer", "lead"}); code != 2 {
		t.Errorf("viewer rename exit %d, want 2", code)
	}
}

func TestRenameKeepsTokenRightsAndDelivery(t *testing.T) {
	f := setup(t)
	// before the rename: a DM to bob and an old message from bob
	send(t, f, "alice", "team", "bob", "before rename")
	send(t, f, "bob", "general", "all", "hello general")
	tokFile := filepath.Join(configDir(), "token-bob")

	if code := run([]string{"token", "rename", "bob", "alice-main"}); code != 0 {
		t.Fatalf("rename exit %d", code)
	}
	// same token, same file
	tok, err := readToken(tokFile)
	if err != nil || tok != f.tokens["bob"] {
		t.Fatalf("token file changed: %v", err)
	}
	code, out := f.req(t, "GET", "/members", tok, nil)
	if code != 200 || out["you"] != "alice-main" {
		t.Fatalf("members: %d %v", code, out)
	}
	names := []string{}
	for _, m := range out["members"].([]any) {
		mm := m.(map[string]any)
		names = append(names, mm["name"].(string))
		if mm["name"] == "alice-main" && mm["has_token"] != true {
			t.Error("renamed member shows no token")
		}
	}
	if strings.Contains(strings.Join(names, ","), "bob") || !strings.Contains(strings.Join(names, ","), "alice-main") {
		t.Errorf("members %v", names)
	}
	if chs := out["your_channels"].([]any); len(chs) != 2 {
		t.Errorf("channel rights changed: %v", chs)
	}
	// old messages show the new name; the DM to the old name is in the inbox
	code, out = f.req(t, "GET", "/inbox?since=0", tok, nil)
	ms := msgs(t, out)
	if code != 200 || len(ms) != 1 || ms[0]["to"] != "alice-main" || ms[0]["text"] != "before rename" {
		t.Fatalf("inbox after rename: %d %v", code, ms)
	}
	_, out = f.req(t, "GET", "/inbox?since=0", f.tokens["frank"], nil)
	if ms := msgs(t, out); len(ms) != 1 || ms[0]["from"] != "alice-main" {
		t.Errorf("old line not shown with the new name: %v", ms)
	}
	// --to <old> still delivers, with a notice
	code, out = send(t, f, "alice", "team", "bob", "after rename")
	if code != 200 || out["to"] != "alice-main" || !strings.Contains(out["notice"].(string), "alice-main") {
		t.Fatalf("send to old name: %d %v", code, out)
	}
	_, out = f.req(t, "GET", "/inbox?since=0", tok, nil)
	if ms := msgs(t, out); len(ms) != 2 {
		t.Errorf("message to the old name not delivered: %v", ms)
	}
	// the new name works without a notice; the stored line keeps no notice
	code, out = send(t, f, "alice", "team", "alice-main", "by new name")
	if code != 200 || out["notice"] != nil {
		t.Errorf("send by new name: %d %v", code, out)
	}
	b, _ := os.ReadFile(filepath.Join(stateDir(), "messages.jsonl"))
	if strings.Contains(string(b), "notice") {
		t.Error("notice stored in messages.jsonl")
	}
	// rights stay narrow for a general-only member
	if code := run([]string{"token", "rename", "frank", "frank2"}); code != 0 {
		t.Fatal("rename frank")
	}
	if code, _ := send(t, f, "frank", "team", "all", "hi"); code != 403 {
		t.Errorf("renamed general-only member wrote to work: %d", code)
	}
	if code, _ := send(t, f, "alice", "team", "frank2", "hi"); code != 400 {
		t.Errorf("work DM to a general-only member: %d", code)
	}
	// tokens.json moved the hash; the audit log has the rename
	h, _ := readHashes(tokensPath())
	if _, ok := h["bob"]; ok || h["alice-main"] != hashToken(tok) {
		t.Errorf("token hash not moved: %v", h)
	}
	a, _ := os.ReadFile(filepath.Join(stateDir(), "audit.jsonl"))
	if !strings.Contains(string(a), `"action":"member-rename"`) || !strings.Contains(string(a), `"target":"bob->alice-main"`) {
		t.Errorf("no rename audit line:\n%s", a)
	}
	// the old name is no longer a member name and cannot take a new token
	if code := run([]string{"token", "new", "bob"}); code != 2 {
		t.Errorf("token new for an old name: exit %d", code)
	}
}

func TestRenameChainAndConflicts(t *testing.T) {
	f := setup(t)
	send(t, f, "alice", "team", "dave", "to dave")
	for _, step := range [][2]string{{"dave", "alice-w"}, {"alice-w", "alice-dave"}} {
		if err := renameMember(step[0], step[1], "local-cli", time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	nt := currentNames()
	if nt.canon("dave") != "alice-dave" || nt.canon("alice-w") != "alice-dave" {
		t.Errorf("chain aliases %v", nt.Aliases)
	}
	_, out := f.req(t, "GET", "/inbox?since=0", f.tokens["dave"], nil)
	if ms := msgs(t, out); len(ms) != 1 || ms[0]["to"] != "alice-dave" {
		t.Errorf("inbox after chain: %v", ms)
	}
	// an old name of one member cannot go to another; the member itself may take it back
	if err := renameMember("erin", "alice-w", "local-cli", time.Now()); err == nil {
		t.Error("old name of alice-dave given to erin")
	}
	if err := renameMember("erin", "alice-dave", "local-cli", time.Now()); err == nil {
		t.Error("name in use accepted")
	}
	if err := renameMember("erin", "frank", "local-cli", time.Now()); err == nil {
		t.Error("base name of another member accepted")
	}
	if err := renameMember("alice-dave", "dave", "local-cli", time.Now()); err != nil {
		t.Errorf("rename back to the base name: %v", err)
	}
	nt = currentNames()
	if nt.canon("dave") != "dave" || nt.canon("alice-dave") != "dave" || len(nt.Names) != 0 {
		t.Errorf("after rename back: names %v aliases %v", nt.Names, nt.Aliases)
	}
	if code, out := f.req(t, "GET", "/members", f.tokens["dave"], nil); code != 200 || out["you"] != "dave" {
		t.Errorf("token after rename back: %d %v", code, out)
	}
}

func TestViewRename(t *testing.T) {
	v := setupView(t)
	post := func(cookie string, form url.Values) int {
		resp, _ := v.do(t, "POST", "/view/rename", cookie, map[string]string{
			"Origin": v.origin, "Content-Type": "application/x-www-form-urlencoded", "Sec-Fetch-Site": "same-origin",
		}, form.Encode())
		return resp.StatusCode
	}
	if code := post("", url.Values{"old": {"erin"}, "new": {"alice-erin"}}); code != 401 {
		t.Errorf("rename without a session: %d", code)
	}
	// a member token is no session
	if code, _ := v.req(t, "POST", "/view/rename", v.tokens["alice"], nil); code == 200 {
		t.Error("member token renamed")
	}
	_, c := v.login(t, v.viewerTok, "")
	tok := v.csrf(t, c)
	if code := post(c, url.Values{"csrf": {"wrong"}, "old": {"erin"}, "new": {"alice-erin"}}); code != 403 {
		t.Errorf("bad csrf: %d", code)
	}
	if code := post(c, url.Values{"csrf": {tok}, "old": {"erin"}, "new": {"Bad_Name"}}); code != 400 {
		t.Errorf("bad name: %d", code)
	}
	if code := post(c, url.Values{"csrf": {tok}, "old": {"viewer"}, "new": {"lead"}}); code != 400 {
		t.Errorf("viewer rename: %d", code)
	}
	if code := post(c, url.Values{"csrf": {tok}, "old": {"erin"}, "new": {"alice-erin"}}); code != 200 {
		t.Fatalf("rename: %d", code)
	}
	resp, body := v.do(t, "GET", "/view/api/messages?since=0", c, nil, "")
	if resp.StatusCode != 200 || !strings.Contains(body, `"alice-erin"`) || strings.Contains(body, `"erin"`) {
		t.Errorf("view members after rename: %s", body)
	}
	if !strings.Contains(body, `"server":{"app":"agentsagora"`) {
		t.Errorf("no server status in the view response: %s", body)
	}
	a, _ := os.ReadFile(filepath.Join(stateDir(), "audit.jsonl"))
	if !strings.Contains(string(a), `"member":"viewer"`) || !strings.Contains(string(a), `"target":"erin->alice-erin"`) {
		t.Errorf("no view rename audit line:\n%s", a)
	}
	// the page carries the rename form and the server line
	_, page := v.do(t, "GET", "/view", c, nil, "")
	if !strings.Contains(page, `id="rn"`) || !strings.Contains(page, `id="srv"`) {
		t.Error("page lacks the rename form or the server line")
	}
}
