package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestLoadMemberTable(t *testing.T) {
	oldM, oldG, oldD := memberChannels, generalOnlyMembers, DefaultMembers
	t.Cleanup(func() { memberChannels, generalOnlyMembers, DefaultMembers = oldM, oldG, oldD })
	p := filepath.Join(configDir(), memberTableFile)
	t.Cleanup(func() { os.Remove(p) })

	if err := loadMemberTable(); err != nil { // no file: built-in table stays
		t.Fatal(err)
	}
	if !slices.Contains(DefaultMembers, "alice") {
		t.Fatalf("built-in table lost: %v", DefaultMembers)
	}

	good := `{"members": {"zed": ["team", "general"], "amy": ["general"]}, "general_only": ["amy"]}`
	if err := os.WriteFile(p, []byte(good), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := loadMemberTable(); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(DefaultMembers, []string{"amy", "zed"}) || !slices.Equal(memberChannels["zed"], []string{chTeam, chGeneral}) ||
		!slices.Equal(generalOnlyMembers, []string{"amy"}) {
		t.Fatalf("table not loaded: %v %v %v", DefaultMembers, memberChannels, generalOnlyMembers)
	}

	for _, bad := range []string{`{`, `{"members": {}}`, `{"members": {"Bad Name": ["team"]}}`, `{"members": {"amy": ["nope"]}}`, `{"members": {"viewer": ["team"]}}`} {
		if err := os.WriteFile(p, []byte(bad), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := loadMemberTable(); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}
