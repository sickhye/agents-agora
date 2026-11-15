package main

import (
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The channel table is explicit: visibility is data, not a property of the name.
func TestChannelVisibilityTable(t *testing.T) {
	want := map[string]string{chGeneral: visPublic, chTeam: visPrivate}
	if len(channelDefs) != len(want) {
		t.Fatalf("channelDefs %v", channelDefs)
	}
	for _, d := range channelDefs {
		if want[d.Name] != d.Visibility {
			t.Errorf("channel %s visibility %q, want %q", d.Name, d.Visibility, want[d.Name])
		}
	}
	if !slices.Equal(allChannels, []string{"team", "general"}) {
		t.Errorf("allChannels %v", allChannels)
	}
	if !isPublic("general") || isPublic("team") || isPublic("nope") {
		t.Error("isPublic wrong")
	}
	if got := channelLabel("general"); got != "#general" {
		t.Errorf("general label %q", got)
	}
	if got := channelLabel("team"); got != "🔒team" {
		t.Errorf("team label %q", got)
	}
	if got := strings.Join(channelLabels([]string{"team", "general"}), " "); got != "🔒team #general" {
		t.Errorf("labels %q", got)
	}
}

// The deny-list lint follows the visibility of the channel, not its name.
func TestLintFollowsVisibility(t *testing.T) {
	f := setup(t)
	const work = "orion release is blocked in jenkins, see jira"
	if err := f.sv.lint("general", work); err == nil {
		t.Error("public channel accepted work content")
	}
	if err := f.sv.lint("team", work); err != nil {
		t.Errorf("private channel linted: %v", err)
	}
	// swap the visibilities: the lint swaps with them.
	old := slices.Clone(channelDefs)
	t.Cleanup(func() { channelDefs = old })
	channelDefs = []channelDef{{chTeam, visPublic, "x"}, {chGeneral, visPrivate, "y"}}
	if err := f.sv.lint("team", work); err == nil {
		t.Error("public team channel accepted work content")
	}
	if err := f.sv.lint("general", work); err != nil {
		t.Errorf("private general channel linted: %v", err)
	}
}

// A member that is limited to public channels never gets a private one.
func TestPublicOnlyMemberGetsNoPrivate(t *testing.T) {
	if got := currentNames().channels("discord-bridge"); !slices.Equal(got, []string{chGeneral}) {
		t.Errorf("discord-bridge channels %v", got)
	}
}

// /members and /view/messages carry the visibility table.
func TestChannelInfoInAPI(t *testing.T) {
	f := setup(t)
	_, out := f.req(t, "GET", "/members", f.tokens["alice"], nil)
	info, _ := out["channel_info"].([]any)
	if len(info) != 2 {
		t.Fatalf("channel_info %v", out["channel_info"])
	}
	got := map[string]string{}
	for _, x := range info {
		m := x.(map[string]any)
		got[m["name"].(string)] = m["visibility"].(string)
	}
	if got["general"] != "public" || got["team"] != "private" {
		t.Errorf("channel_info %v", got)
	}
}

// Client listings show # for public and a lock for private channels.
func TestMembersListingMarks(t *testing.T) {
	f := setup(t)
	t.Setenv("AGENTSAGORA_URL", f.srv.URL)
	t.Setenv("AGENTSAGORA_TOKEN_FILE", filepath.Join(configDir(), "token-alice"))
	out := captureStdout(t, func() {
		if code := run([]string{"members"}); code != 0 {
			t.Errorf("members exit %d", code)
		}
	})
	if !strings.Contains(out, "🔒team #general") || !strings.Contains(out, "#general\n") {
		t.Errorf("members output:\n%s", out)
	}
}

// A release build sets the version with -ldflags "-X main.version=..."; both spellings print it.
func TestVersionFlag(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	bin := filepath.Join(t.TempDir(), "a2")
	if out, err := exec.Command("go", "build", "-ldflags", "-X main.version=v1.2.3", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	for _, a := range []string{"version", "--version"} {
		out, err := exec.Command(bin, a).Output()
		if err != nil || !strings.HasPrefix(string(out), "agentsagora v1.2.3 ") {
			t.Errorf("%s: %q, %v", a, out, err)
		}
	}
	if version != "dev" {
		t.Errorf("default version %q, want dev", version)
	}
}
