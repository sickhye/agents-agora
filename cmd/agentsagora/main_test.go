package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestMain keeps every test away from the live dirs: CLI helpers such as `token new`
// write audit.jsonl under stateDir()), so HOME, the config dir and the state dir all point
// into a temp dir.
func TestMain(m *testing.M) {
	// helper mode for the self-update version check (autoupdate_test.go): the test binary
	// stands in for a new server binary.
	switch os.Getenv("A2_TEST_HELPER") {
	case "version":
		os.Stdout.WriteString("agentsagora test-helper build 000000000000\n")
		os.Exit(0)
	case "hang":
		time.Sleep(30 * time.Second)
		os.Exit(0)
	case "bad":
		os.Stdout.WriteString("something else\n")
		os.Exit(0)
	}
	os.Setenv("A2_NO_AUTOUPDATE", "1") // no test may replace the test binary
	dir, err := os.MkdirTemp("", "agentsagora-test-")
	if err != nil {
		panic(err)
	}
	for _, d := range []string{"home", "config", "state"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o700); err != nil {
			panic(err)
		}
	}
	for _, k := range []string{"CONFIG_DIR", "STATE_DIR", "URL", "TOKEN_FILE", "CA_FILE"} {
		os.Unsetenv(envPrefix + k)
	}
	os.Setenv("HOME", filepath.Join(dir, "home"))
	os.Setenv("USERPROFILE", filepath.Join(dir, "home"))
	os.Setenv("AGENTSAGORA_CONFIG_DIR", filepath.Join(dir, "config"))
	os.Setenv("AGENTSAGORA_STATE_DIR", filepath.Join(dir, "state"))
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
