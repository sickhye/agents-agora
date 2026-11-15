package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Global --token-file: flag > AGENTSAGORA_TOKEN_FILE > token_file= in client.conf,
// and the flag reaches a re-exec after an update through the env.

func withClientConf(t *testing.T, tokenFile string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv(envPrefix+"CONFIG_DIR", dir)
	conf := "url=http://127.0.0.1:1\ntoken_file=" + tokenFile + "\n"
	if err := os.WriteFile(filepath.Join(dir, "client.conf"), []byte(conf), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envPrefix+"URL", "")
	t.Setenv(envPrefix+"TOKEN_FILE", "")
	tokenFileFlag = ""
	t.Cleanup(func() { tokenFileFlag = "" })
}

func TestTokenFileFlagSetsTokenFile(t *testing.T) {
	withClientConf(t, "/conf/token")
	rest, err := parseGlobalFlags([]string{"--token-file", "/flag/token", "watch", "--wait", "5"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(rest, " ") != "watch --wait 5" {
		t.Fatalf("rest = %q", rest)
	}
	c, err := loadClientConfig()
	if err != nil {
		t.Fatal(err)
	}
	if c.TokenFile != "/flag/token" {
		t.Fatalf("TokenFile = %q, want /flag/token", c.TokenFile)
	}
}

func TestTokenFileFlagBeatsEnv(t *testing.T) {
	withClientConf(t, "/conf/token")
	t.Setenv(envPrefix+"TOKEN_FILE", "/env/token")
	c, err := loadClientConfig()
	if err != nil || c.TokenFile != "/env/token" {
		t.Fatalf("env only: %q %v", c.TokenFile, err)
	}
	if _, err := parseGlobalFlags([]string{"--token-file", "/flag/token", "send"}); err != nil {
		t.Fatal(err)
	}
	c, err = loadClientConfig()
	if err != nil || c.TokenFile != "/flag/token" {
		t.Fatalf("flag + env: %q %v, want /flag/token", c.TokenFile, err)
	}
}

func TestTokenFileFlagEqualsFormAndHome(t *testing.T) {
	withClientConf(t, "/conf/token")
	rest, err := parseGlobalFlags([]string{"--token-file=~/x/member.token", "inbox", "--peek"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(rest, " ") != "inbox --peek" {
		t.Fatalf("rest = %q", rest)
	}
	want := filepath.Join(homeDir(), "x", "member.token")
	c, err := loadClientConfig()
	if err != nil || c.TokenFile != want {
		t.Fatalf("TokenFile = %q %v, want %q", c.TokenFile, err, want)
	}
}

func TestTokenFileFlagMissingValueIsUsageError(t *testing.T) {
	withClientConf(t, "/conf/token")
	for _, args := range [][]string{
		{"--token-file"},
		{"--token-file", "--help"},
		{"--token-file="},
		{"--token-file", ""},
	} {
		if _, err := parseGlobalFlags(args); err == nil {
			t.Errorf("%q: no error", args)
		} else if _, ok := err.(usageError); !ok {
			t.Errorf("%q: %T, want usageError", args, err)
		}
		if code := run(args); code != 2 {
			t.Errorf("run(%q) = %d, want 2", args, code)
		}
	}
	// a flag with no command after it is a usage error too
	if code := run([]string{"--token-file", "/flag/token"}); code != 2 {
		t.Errorf("no command: %d, want 2", code)
	}
}

func TestTokenFileFlagNoFlagLeavesArgs(t *testing.T) {
	withClientConf(t, "/conf/token")
	rest, err := parseGlobalFlags([]string{"send", "--token-file", "x"})
	if err != nil || strings.Join(rest, " ") != "send --token-file x" || tokenFileFlag != "" {
		t.Fatalf("rest=%q err=%v flag=%q", rest, err, tokenFileFlag)
	}
	c, err := loadClientConfig()
	if err != nil || c.TokenFile != "/conf/token" {
		t.Fatalf("config only: %q %v", c.TokenFile, err)
	}
}

func TestTokenFileFlagSurvivesReexec(t *testing.T) {
	withClientConf(t, "/conf/token")
	base := []string{"PATH=/bin", envPrefix + "TOKEN_FILE=/env/token", "HOME=/h"}
	env := reexecEnvFrom(base)
	if !hasEnv(env, envPrefix+"TOKEN_FILE=/env/token") || !hasEnv(env, envReexeced+"=1") {
		t.Fatalf("no flag: env changed: %q", env)
	}
	if _, err := parseGlobalFlags([]string{"--token-file", "/flag/token", "watch"}); err != nil {
		t.Fatal(err)
	}
	env = reexecEnvFrom(base)
	n := 0
	for _, kv := range env {
		if strings.HasPrefix(kv, envPrefix+"TOKEN_FILE=") {
			n++
		}
	}
	if n != 1 || !hasEnv(env, envPrefix+"TOKEN_FILE=/flag/token") || !hasEnv(env, envReexeced+"=1") || !hasEnv(env, "PATH=/bin") {
		t.Fatalf("re-exec env = %q", env)
	}
	if base[1] != envPrefix+"TOKEN_FILE=/env/token" {
		t.Fatalf("caller env changed: %q", base)
	}
	// the re-exec'd process (args from the command on, env as above) gets the flag's file
	tokenFileFlag = ""
	t.Setenv(envPrefix+"TOKEN_FILE", "/flag/token")
	c, err := loadClientConfig()
	if err != nil || c.TokenFile != "/flag/token" {
		t.Fatalf("after re-exec: %q %v", c.TokenFile, err)
	}
}

func TestTokenFileFlagErrorNamesOnlyPath(t *testing.T) {
	skipWithoutUnixModes(t) // the error under test is the mode refusal
	withClientConf(t, "/conf/token")
	p := filepath.Join(t.TempDir(), "member.token")
	secret := "s3cr3t-token-value"
	if err := os.WriteFile(p, []byte(secret+"\n"), 0o644); err != nil { // wrong mode on purpose
		t.Fatal(err)
	}
	if _, err := parseGlobalFlags([]string{"--token-file", p, "members"}); err != nil {
		t.Fatal(err)
	}
	_, err := newClient()
	if err == nil {
		t.Fatal("mode 0644 token file accepted")
	}
	if strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), p) {
		t.Fatalf("error text: %q", err)
	}
}

func hasEnv(env []string, kv string) bool {
	for _, e := range env {
		if e == kv {
			return true
		}
	}
	return false
}
