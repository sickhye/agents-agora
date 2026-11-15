package main

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestTagValidation(t *testing.T) {
	ok := [][]string{nil, {"approval"}, {"a", "b-1", "0"}, {strings.Repeat("a", 24)}, {"a", "b", "c", "d", "e"}, {"a", "a"}}
	for _, in := range ok {
		if _, err := normalizeTags(in); err != nil {
			t.Errorf("%v: %v", in, err)
		}
	}
	got, _ := normalizeTags([]string{"a", "b", "a"})
	if !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Errorf("repeat not dropped: %v", got)
	}
	bad := [][]string{{""}, {"Approval"}, {"a b"}, {"a_b"}, {"é"}, {strings.Repeat("a", 25)}, {"a", "b", "c", "d", "e", "f"}, {"<b>"}}
	for _, in := range bad {
		if _, err := normalizeTags(in); err == nil {
			t.Errorf("%v accepted", in)
		}
	}
	if !reflect.DeepEqual(splitTags("a, b"), []string{"a", "b"}) || splitTags("") != nil || len(splitTags("a,,b")) != 3 {
		t.Error("splitTags")
	}
}

func TestSendTagsStoredAndRoundTrip(t *testing.T) {
	f := setup(t)
	code, out := f.req(t, "POST", "/send", f.tokens["alice"], map[string]any{"to": "all", "text": "hello", "tags": []string{"approval", "blog"}})
	if code != 200 || fmt.Sprint(out["tags"]) != "[approval blog]" {
		t.Fatalf("send: %d %v", code, out)
	}
	root := int64(out["id"].(float64))
	// reply in the thread with a tag
	if code, _ = f.req(t, "POST", "/send", f.tokens["bob"], map[string]any{"to": "all", "text": "re", "reply_to": root, "tags": []string{"blog"}}); code != 200 {
		t.Fatalf("reply: %d", code)
	}
	// a message with no tags omits the field; bad tags store nothing
	_, plain := f.req(t, "POST", "/send", f.tokens["alice"], map[string]any{"to": "all", "text": "plain"})
	if _, has := plain["tags"]; has {
		t.Errorf("tags field present on a plain message: %v", plain)
	}
	n := len(f.sv.store.after(0))
	for _, bad := range [][]string{{"Bad"}, {"a", "b", "c", "d", "e", "f"}, {strings.Repeat("x", 25)}} {
		code, out := f.req(t, "POST", "/send", f.tokens["alice"], map[string]any{"to": "all", "text": "x", "tags": bad})
		if code != 400 || !strings.Contains(fmt.Sprint(out), "tag") {
			t.Errorf("%v: %d %v", bad, code, out)
		}
	}
	if len(f.sv.store.after(0)) != n {
		t.Error("a refused send was stored")
	}
	// reload from the file: tags survive, old lines without tags still load
	raw, _ := os.ReadFile(f.sv.store.path)
	if strings.Count(string(raw), `"tags"`) != 2 {
		t.Errorf("tags in file: %s", raw)
	}
	old := `{"id":99,"ts":"2026-11-01T09:00:00+09:00","channel":"team","from":"alice","to":"all","text":"old"}` + "\n"
	p2 := filepath.Join(t.TempDir(), "m.jsonl")
	os.WriteFile(p2, append(raw, old...), 0o600)
	s2, err := openStore(p2)
	if err != nil {
		t.Fatal(err)
	}
	m1, _ := s2.get(root)
	m99, _ := s2.get(99)
	if !reflect.DeepEqual(m1.Tags, []string{"approval", "blog"}) || m99.Tags != nil {
		t.Errorf("reload: %v %v", m1.Tags, m99.Tags)
	}
}

func TestInboxTagFilter(t *testing.T) {
	f := setup(t)
	t0 := time.Date(2026, 11, 16, 9, 0, 0, 0, jst)
	put := func(i int, from, to string, tags ...string) {
		if _, err := f.sv.store.appendMsg(Message{Channel: "team", From: from, To: to, Text: "m", Tags: tags}, t0.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	put(1, "bob", "all", "approval")
	put(2, "bob", "all")
	put(3, "bob", "all", "blog", "x")
	put(4, "bob", "all", "approval", "blog")
	put(5, "alice", "all", "approval") // own broadcast: only in --sent
	eq := func(name string, got []int64, want ...int64) {
		t.Helper()
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("%s: got %v want %v", name, got, want)
		}
	}
	_, ids := inboxIDs(t, f, "alice", "tag=approval")
	eq("one tag", ids, 1, 4)
	_, ids = inboxIDs(t, f, "alice", "tag=blog,x")
	eq("any of", ids, 3, 4)
	_, ids = inboxIDs(t, f, "alice", "tag=approval&sent=1")
	eq("with sent", ids, 5)
	_, ids = inboxIDs(t, f, "alice", "tag=approval&thread=4")
	eq("with thread", ids, 4)
	_, ids = inboxIDs(t, f, "alice", "tag=approval&from=bob&channel=team")
	eq("with from+channel", ids, 1, 4)
	if code, _ := inboxIDs(t, f, "alice", "tag=Bad"); code != 400 {
		t.Errorf("bad filter tag: %d", code)
	}
	_, ids = inboxIDs(t, f, "alice", "")
	eq("no filter", ids, 1, 2, 3, 4)
}

func TestTagsCommandRights(t *testing.T) {
	f := setup(t)
	t0 := time.Date(2026, 11, 16, 9, 0, 0, 0, jst)
	put := func(i int, ch, from, to string, tags ...string) {
		if _, err := f.sv.store.appendMsg(Message{Channel: ch, From: from, To: to, Text: "m", Tags: tags}, t0.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	put(1, "team", "bob", "all", "approval", "blog")
	put(2, "team", "bob", "all", "approval")
	put(3, "team", "bob", "dave", "secret") // a DM alice cannot read
	put(4, "general", "alice", "all", "blog")
	_, out := f.req(t, "GET", "/tags", f.tokens["alice"], nil)
	if s := fmt.Sprint(out["tags"]); s != "[map[count:2 tag:approval] map[count:2 tag:blog]]" {
		t.Errorf("alice: %s", s)
	}
	_, out = f.req(t, "GET", "/tags?channel=general", f.tokens["alice"], nil)
	if s := fmt.Sprint(out["tags"]); s != "[map[count:1 tag:blog]]" {
		t.Errorf("channel: %s", s)
	}
	_, out = f.req(t, "GET", "/tags?since=1", f.tokens["alice"], nil)
	if s := fmt.Sprint(out["tags"]); s != "[map[count:1 tag:approval] map[count:1 tag:blog]]" {
		t.Errorf("since: %s", s)
	}
	_, out = f.req(t, "GET", "/tags", f.tokens["dave"], nil)
	if !strings.Contains(fmt.Sprint(out["tags"]), "secret") {
		t.Errorf("recipient sees its DM tag: %v", out)
	}
}

func TestTagPrinting(t *testing.T) {
	m := Message{ID: 1230, TS: "t", Channel: "team", From: "alice", To: "all", Text: "text", Tags: []string{"approval", "blog"}}
	if got, want := formatLine(m), "[a2] #1230 [🔒team] t alice -> all [approval,blog]: text"; got != want {
		t.Errorf("%q", got)
	}
	m.ReplyTo = 5
	if got := formatLine(m); !strings.Contains(got, "-> all [approval,blog] (re #5):") {
		t.Errorf("%q", got)
	}
	m.Kind = kindLog
	if got := watchLine(m); !strings.Contains(got, "alice [approval,blog]:") {
		t.Errorf("%q", got)
	}
	if got := formatLine(Message{ID: 1, Channel: "c", From: "a", To: "b", Text: "x"}); strings.Contains(got, "[]") {
		t.Errorf("%q", got)
	}
}

func TestMCPTags(t *testing.T) {
	f := setup(t)
	m, err := f.sv.sendWith("alice", "team", "all", "hi", 0, sendOpts{tags: []string{"mcp"}})
	if err != nil || !reflect.DeepEqual(m.Tags, []string{"mcp"}) {
		t.Fatalf("%v %v", m, err)
	}
	_, au := f.sv.mcpCall("alice", "send", []byte(`{"channel":"team","text":"x","tags":["Bad"]}`))
	if au.err == nil {
		t.Error("bad MCP tag accepted")
	}
	out, au := f.sv.mcpCall("bob", "inbox", []byte(`{"tags":["mcp"]}`))
	if au.err != nil || len(out["messages"].([]Message)) != 1 {
		t.Errorf("mcp inbox filter: %v %v", out, au.err)
	}
	out, _ = f.sv.mcpCall("bob", "inbox", []byte(`{"tags":["other"]}`))
	if len(out["messages"].([]Message)) != 0 {
		t.Error("mcp inbox filter matched the wrong tag")
	}
}

func TestViewTagsEscapedAndFiltered(t *testing.T) {
	v := setupView(t)
	t0 := time.Date(2026, 11, 16, 9, 0, 0, 0, jst)
	v.sv.store.appendMsg(Message{Channel: "team", From: "bob", To: "all", Text: "a", Tags: []string{"approval"}}, t0)
	v.sv.store.appendMsg(Message{Channel: "team", From: "bob", To: "all", Text: "b"}, t0.Add(time.Minute))
	// a hand edited store line with markup in a tag: dropped on load, never served
	p := filepath.Join(t.TempDir(), "m.jsonl")
	os.WriteFile(p, []byte(`{"id":1,"ts":"2026-11-16T09:00:00+09:00","channel":"team","from":"a","to":"all","text":"x","tags":["<script>alert(1)</script>","ok"]}`+"\n"), 0o600)
	s2, err := openStore(p)
	if err != nil {
		t.Fatal(err)
	}
	if m, _ := s2.get(1); !reflect.DeepEqual(m.Tags, []string{"ok"}) {
		t.Errorf("markup tag kept: %v", m.Tags)
	}
	_, cookie := v.login(t, v.viewerTok, "")
	resp, body := v.do(t, "GET", "/view/api/messages?since=0&tag=approval", cookie, nil, "")
	if resp.StatusCode != 200 || !strings.Contains(body, `"tags":["approval"]`) || strings.Contains(body, `"text":"b"`) {
		t.Errorf("filtered list: %d %s", resp.StatusCode, body)
	}
	_, all := v.do(t, "GET", "/view/api/messages?since=0", cookie, nil, "")
	if !strings.Contains(all, `"text":"b"`) || !strings.Contains(all, `"text":"a"`) {
		t.Errorf("unfiltered list: %s", all)
	}
	if resp, _ := v.do(t, "GET", "/view/api/messages?since=0&tag=%3Cb%3E", cookie, nil, ""); resp.StatusCode != 400 {
		t.Errorf("bad tag: %d", resp.StatusCode)
	}
	// the page script builds labels with textContent (no innerHTML for tags) and reads ?tag=
	_, js := v.do(t, "GET", "/view/app.js", cookie, nil, "")
	i := strings.Index(js, "function tagNodes")
	if i < 0 || strings.Contains(js[i:i+400], "innerHTML") || !strings.Contains(js, `get("tag")`) {
		t.Error("view script tag handling")
	}
	if resp, _ := v.do(t, "GET", "/view?tag=approval", cookie, nil, ""); resp.StatusCode != 200 {
		t.Errorf("/view?tag=: %d", resp.StatusCode)
	}
}

func TestWatchTagFilter(t *testing.T) {
	var out strings.Builder
	wt := &watcher{out: &out, tags: []string{"approval"}, cp: filepath.Join(t.TempDir(), "cp"), holdInbox: true}
	wt.show(Message{ID: 1, Channel: "team", From: "a", To: "all", Text: "x"})
	wt.show(Message{ID: 2, Channel: "team", From: "a", To: "all", Text: "y", Tags: []string{"approval"}})
	if s := out.String(); strings.Contains(s, "#1 ") || !strings.Contains(s, "#2 ") || !strings.Contains(s, "[approval]") || wt.last != 2 {
		t.Errorf("watch: %q last=%d", s, wt.last)
	}
}
