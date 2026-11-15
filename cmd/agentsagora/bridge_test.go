package main

import (
	"encoding/base64"
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// discord-bridge (the a2-bridge relay) is a general-only member.

func TestBridgeMemberTable(t *testing.T) {
	if !slices.Contains(DefaultMembers, "discord-bridge") {
		t.Fatal("discord-bridge not in DefaultMembers")
	}
	if got := memberChannels["discord-bridge"]; !slices.Equal(got, []string{chGeneral}) {
		t.Fatalf("discord-bridge channels %v, want [general]", got)
	}
	// even if someone adds work to the table, the bridge never gets it
	t.Cleanup(func() { memberChannels["discord-bridge"] = []string{chGeneral} })
	memberChannels["discord-bridge"] = []string{chTeam, chGeneral}
	if got := emptyNames().channels("discord-bridge"); !slices.Equal(got, []string{chGeneral}) {
		t.Errorf("general-only filter: %v", got)
	}
}

func TestBridgeHTTPAndMCPRights(t *testing.T) {
	f := setup(t)
	send(t, f, "alice", "team", "all", "build is green")
	// HTTP
	if code, _ := send(t, f, "discord-bridge", "team", "all", "hi"); code != 403 {
		t.Errorf("bridge work send %d, want 403", code)
	}
	if code, _ := f.req(t, "GET", "/inbox?channel=team", f.tokens["discord-bridge"], nil); code != 403 {
		t.Errorf("bridge work read %d, want 403", code)
	}
	if code, _ := f.req(t, "GET", "/files?channel=team", f.tokens["discord-bridge"], nil); code != 403 {
		t.Errorf("bridge work files %d, want 403", code)
	}
	if code, out := send(t, f, "discord-bridge", "general", "all", "nice weather"); code != 200 || out["channel"] != "general" {
		t.Fatalf("bridge general send %d %v", code, out)
	}
	if _, out := f.req(t, "POST", "/send", f.tokens["discord-bridge"], map[string]string{"to": "all", "text": "no channel"}); out["channel"] != "general" {
		t.Errorf("bridge default channel %v", out["channel"])
	}
	if code, _ := send(t, f, "alice", "team", "discord-bridge", "x"); code != 400 {
		t.Errorf("work DM to bridge %d, want 400", code)
	}
	send(t, f, "alice", "general", "discord-bridge", "lunch?")
	_, in := f.req(t, "GET", "/inbox", f.tokens["discord-bridge"], nil)
	got := msgs(t, in)
	if len(got) == 0 {
		t.Fatal("bridge got no general messages")
	}
	for _, m := range got {
		if m["channel"] != "general" {
			t.Errorf("bridge inbox leaked %v", m)
		}
	}
	_, mem := f.req(t, "GET", "/members", f.tokens["discord-bridge"], nil)
	if ch, _ := mem["your_channels"].([]any); len(ch) != 1 || ch[0] != "general" {
		t.Errorf("bridge your_channels %v", mem["your_channels"])
	}
	// MCP
	if res, isErr := toolCall(t, f, "discord-bridge", "send", map[string]any{"channel": "team", "text": "x"}); !isErr || !strings.Contains(res["error"].(string), "not allowed") {
		t.Errorf("mcp bridge work send %v", res)
	}
	if res, isErr := toolCall(t, f, "discord-bridge", "inbox", map[string]any{"channel": "team"}); !isErr || !strings.Contains(res["error"].(string), "not allowed") {
		t.Errorf("mcp bridge work inbox %v", res)
	}
	if res, isErr := toolCall(t, f, "discord-bridge", "put_file", map[string]any{"channel": "team", "name": "d.png", "mime": "image/png", "base64": base64.StdEncoding.EncodeToString(tinyPNG)}); !isErr {
		t.Errorf("mcp bridge work put_file %v", res)
	}
	if res, isErr := toolCall(t, f, "discord-bridge", "list_files", map[string]any{"channel": "team"}); !isErr {
		t.Errorf("mcp bridge work list_files %v", res)
	}
	if res, isErr := toolCall(t, f, "discord-bridge", "send", map[string]any{"channel": "general", "text": "sunny today"}); isErr {
		t.Errorf("mcp bridge general send %v", res)
	}
	res, isErr := toolCall(t, f, "discord-bridge", "inbox", map[string]any{"channel": "general"})
	if isErr || len(res["messages"].([]any)) == 0 {
		t.Errorf("mcp bridge general inbox %v", res)
	}
}

func TestBridgeWSRights(t *testing.T) {
	f := setup(t)
	if code, _ := wsRaw(t, f.srv.URL, "/ws?channel=team", bearer(f.tokens["discord-bridge"])); code != 403 {
		t.Errorf("bridge /ws?channel=team %d, want 403", code)
	}
	_, fc := wsRaw(t, f.srv.URL, "/ws", bearer(f.tokens["discord-bridge"]))
	sendSince(t, fc, 0)
	send(t, f, "alice", "team", "all", "work item")
	_, g := send(t, f, "alice", "general", "all", "lunch?")
	m, err := nextMsg(t, fc, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if m.ID != idOf(g) || m.Channel != "general" {
		t.Errorf("bridge ws got %+v", m)
	}
	if m, err := nextMsg(t, fc, 300*time.Millisecond); err == nil {
		t.Errorf("unexpected extra message %+v", m)
	}
}

func TestBridgeCannotBeRenamed(t *testing.T) {
	f := setup(t)
	now := time.Now()
	for _, n := range []string{"dave", "work-bridge", "alice", "bridge2"} {
		if err := renameMember("discord-bridge", n, "local-cli", now); err == nil {
			t.Errorf("rename discord-bridge -> %s accepted", n)
		}
	}
	// a work member cannot take the bridge name either
	if err := renameMember("dave", "discord-bridge", "local-cli", now); err == nil {
		t.Error("rename dave -> discord-bridge accepted")
	}
	// a work member renamed away: its old name is an alias, not free for the bridge
	if err := renameMember("carol", "mac-r2", "local-cli", now); err != nil {
		t.Fatal(err)
	}
	if err := renameMember("discord-bridge", "carol", "local-cli", now); err == nil {
		t.Error("rename discord-bridge -> old work name accepted")
	}
	if !slices.Equal(channelsOf("discord-bridge"), []string{chGeneral}) {
		t.Errorf("bridge channels after attempts %v", channelsOf("discord-bridge"))
	}
	if code, _ := send(t, f, "discord-bridge", "team", "all", "x"); code != 403 {
		t.Errorf("bridge work send after rename attempts %d", code)
	}
}

// tools/list shows each member only its own channels.
func TestMCPToolsListOwnChannels(t *testing.T) {
	f := setup(t)
	list := func(who string) (string, []tool) {
		_, out := mcpCall(t, f, who, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
		b, _ := json.Marshal(out["result"].(map[string]any)["tools"])
		var ts []tool
		json.Unmarshal(b, &ts)
		return string(b), ts
	}
	for _, who := range []string{"erin", "discord-bridge", "frank"} {
		raw, ts := list(who)
		if strings.Contains(raw, "team") {
			t.Errorf("%s tools/list mentions work: %s", who, raw)
		}
		for _, tl := range ts {
			ch, ok := tl.InputSchema["properties"].(map[string]any)["channel"].(map[string]any)
			if !ok {
				continue
			}
			if enum, _ := json.Marshal(ch["enum"]); string(enum) != `["general"]` {
				t.Errorf("%s %s channel enum %s", who, tl.Name, enum)
			}
			if !strings.Contains(tl.Description, "your channels: #general = public") {
				t.Errorf("%s %s description %q", who, tl.Name, tl.Description)
			}
		}
	}
	_, ts := list("dave")
	for _, tl := range ts {
		if ch, ok := tl.InputSchema["properties"].(map[string]any)["channel"].(map[string]any); ok {
			if enum, _ := json.Marshal(ch["enum"]); string(enum) != `["team","general"]` {
				t.Errorf("dave %s channel enum %s", tl.Name, enum)
			}
			if d := tl.Description; !strings.Contains(d, "🔒team = private") || !strings.Contains(d, "#general = public") {
				t.Errorf("dave %s description lacks # / lock marks: %q", tl.Name, d)
			}
		}
	}
	_, out := mcpCall(t, f, "erin", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`)
	if ins := out["result"].(map[string]any)["instructions"].(string); strings.Contains(ins, "team") || !strings.Contains(ins, "your channels: #general") {
		t.Errorf("erin instructions %q", ins)
	}
}

// a2 send --help prints the caller's channels from the server.
func TestSendHelpYourChannels(t *testing.T) {
	f := setup(t)
	t.Setenv("AGENTSAGORA_URL", f.srv.URL)
	t.Setenv("AGENTSAGORA_TOKEN_FILE", filepath.Join(configDir(), "token-erin"))
	if got := yourChannels(); got != "you are erin; your channels: #general (only these are accepted)" {
		t.Errorf("erin: %q", got)
	}
	if err := cmdSend([]string{"--help"}); err != nil {
		t.Errorf("send --help returned %v", err)
	}
	t.Setenv("AGENTSAGORA_TOKEN_FILE", filepath.Join(configDir(), "token-dave"))
	if got := yourChannels(); !strings.Contains(got, "your channels: 🔒team, #general") {
		t.Errorf("dave: %q", got)
	}
}
