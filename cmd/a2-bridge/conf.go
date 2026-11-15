package main

import (
	"bufio"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Only the a2 channel "general" is bridged. "team" (work) must never leave the private network.
const (
	bridgeChannel  = "general"
	defaultMember  = "discord-bridge"
	defaultAPI     = "https://discord.com/api/v10"
	defaultGateway = "wss://gateway.discord.gg/?v=10&encoding=json"
	appVersion     = "1112"
)

// config is bridge.conf plus url/ca_file from the normal a2 client.conf.
type config struct {
	ConfPath         string
	DiscordTokenFile string
	DiscordChannelID string
	A2TokenFile      string
	A2URL            string
	A2CAFile         string
	A2Member         string // expected a2 member name of the bridge token
	StateDir         string
	DiscordAPI       string // tests only (loopback http) unless https
	DiscordGateway   string
	PollEvery        time.Duration
}

func getenv(name string) string { return os.Getenv("AGENTSAGORA_" + name) }

func homeDir() string { h, _ := os.UserHomeDir(); return h }

func configDir() string {
	if d := getenv("CONFIG_DIR"); d != "" {
		return d
	}
	if d := os.Getenv("XDG_CONFIG_HOME"); filepath.IsAbs(d) {
		return filepath.Join(d, "agentsagora")
	}
	return filepath.Join(homeDir(), ".config", "agentsagora")
}

func stateDir() string {
	if d := getenv("STATE_DIR"); d != "" {
		return filepath.Join(d, "bridge")
	}
	if d := os.Getenv("XDG_STATE_HOME"); filepath.IsAbs(d) {
		return filepath.Join(d, "agentsagora", "bridge")
	}
	return filepath.Join(homeDir(), ".local", "state", "agentsagora", "bridge")
}

func expandHome(p string) string {
	// Also "~\dir" on Windows (filepath.Separator is '/' elsewhere, so nothing changes there).
	if p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, "~"+string(filepath.Separator)) {
		return filepath.Join(homeDir(), strings.TrimPrefix(p, "~"))
	}
	return p
}

// readKV reads key=value lines (# comments). A file readable by group or others is refused
// when mustBePrivate is set.
func readKV(path string, mustBePrivate bool) (map[string]string, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	// Windows has no Unix modes (Perm reports 0666 for every file); the user profile ACL
	// guards the config dir there, so the mode check is off.
	if mustBePrivate && runtime.GOOS != "windows" && st.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s must be mode 0600 (chmod 600 it)", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	kv := map[string]string{}
	sc := bufio.NewScanner(f)
	n := 0
	for sc.Scan() {
		n++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("%s line %d: want key=value", path, n)
		}
		kv[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return kv, sc.Err()
}

var knownKeys = map[string]bool{
	"discord_token_file": true, "discord_channel_id": true, "a2_token_file": true,
	"a2_channel": true, "a2_member": true, "state_dir": true, "poll_seconds": true,
	"discord_api": true, "discord_gateway": true, "a2_url": true, "a2_ca_file": true,
}

// loadConfig reads bridge.conf (path "" = default) and client.conf next to it.
func loadConfig(path string) (config, error) {
	if path == "" {
		path = filepath.Join(configDir(), "bridge.conf")
	}
	c := config{ConfPath: path, A2Member: defaultMember, StateDir: stateDir(),
		DiscordAPI: defaultAPI, DiscordGateway: defaultGateway, PollEvery: 3 * time.Second}
	kv, err := readKV(path, true)
	if err != nil {
		return c, err
	}
	for k := range kv {
		if !knownKeys[k] {
			return c, fmt.Errorf("%s: unknown key %q", path, k)
		}
	}
	if ch, ok := kv["a2_channel"]; ok && ch != bridgeChannel {
		return c, fmt.Errorf("%s: a2_channel=%q refused: only %q is bridged", path, ch, bridgeChannel)
	}
	c.DiscordTokenFile = expandHome(kv["discord_token_file"])
	c.DiscordChannelID = kv["discord_channel_id"]
	c.A2TokenFile = expandHome(kv["a2_token_file"])
	if v := kv["a2_member"]; v != "" {
		c.A2Member = v
	}
	if v := kv["state_dir"]; v != "" {
		c.StateDir = expandHome(v)
	}
	if v := kv["discord_api"]; v != "" {
		c.DiscordAPI = strings.TrimRight(v, "/")
	}
	if v := kv["discord_gateway"]; v != "" {
		c.DiscordGateway = v
	}
	if v := kv["poll_seconds"]; v != "" {
		var n int
		if _, err := fmt.Sscan(v, &n); err != nil || n < 1 || n > 300 {
			return c, fmt.Errorf("%s: poll_seconds must be 1..300", path)
		}
		c.PollEvery = time.Duration(n) * time.Second
	}
	var missing []string
	if c.DiscordTokenFile == "" {
		missing = append(missing, "discord_token_file")
	}
	if c.A2TokenFile == "" {
		missing = append(missing, "a2_token_file")
	}
	if !isSnowflake(c.DiscordChannelID) {
		missing = append(missing, "discord_channel_id (digits)")
	}
	if len(missing) > 0 {
		return c, fmt.Errorf("%s: missing or bad: %s", path, strings.Join(missing, ", "))
	}
	if err := checkURL(c.DiscordAPI, "discord_api"); err != nil {
		return c, err
	}
	// a2 server address and CA come from the normal client.conf (its token_file is ignored).
	cp := filepath.Join(filepath.Dir(path), "client.conf")
	ckv, err := readKV(cp, false)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return c, err
	}
	c.A2URL = strings.TrimRight(ckv["url"], "/")
	c.A2CAFile = expandHome(ckv["ca_file"])
	if v := getenv("URL"); v != "" {
		c.A2URL = strings.TrimRight(v, "/")
	}
	if v := getenv("CA_FILE"); v != "" {
		c.A2CAFile = expandHome(v)
	}
	// bridge.conf may name them itself (e.g. while the a2 config is still in the old dir).
	if v := kv["a2_url"]; v != "" {
		c.A2URL = strings.TrimRight(v, "/")
	}
	if v := kv["a2_ca_file"]; v != "" {
		c.A2CAFile = expandHome(v)
	}
	if c.A2URL == "" {
		return c, errors.New("no a2 server URL: set a2_url= in " + path + " or url= in " + cp)
	}
	return c, nil
}

func isSnowflake(s string) bool {
	if len(s) < 5 || len(s) > 20 {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// checkURL allows https anywhere and plain http only on loopback (tests).
func checkURL(raw, what string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("bad %s: %w", what, err)
	}
	switch u.Scheme {
	case "https", "wss":
		return nil
	case "http", "ws":
		ip, perr := netip.ParseAddr(u.Hostname())
		if u.Hostname() == "localhost" || (perr == nil && ip.IsLoopback()) {
			return nil
		}
	}
	return fmt.Errorf("%s must be https (plain http only on loopback)", what)
}

// readSecret reads a token file (0600 on Unix). The content is never printed.
func readSecret(path string) (string, error) {
	st, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if runtime.GOOS != "windows" && st.Mode().Perm()&0o077 != 0 { // no Unix modes on Windows (see readKV)
		return "", errors.New("token file " + path + " must be mode 0600 (chmod 600 it)")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	t := strings.TrimSpace(string(b))
	if t == "" || strings.ContainsAny(t, " \t\r\n") {
		return "", errors.New("token file " + path + " is empty or has more than one line")
	}
	return t, nil
}

func noRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// a2HTTPClient pins the a2 CA (no system roots, no proxy), like the a2 client.
func a2HTTPClient(c config) (*http.Client, error) {
	if err := checkURL(c.A2URL, "a2 url"); err != nil {
		return nil, err
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	if strings.HasPrefix(c.A2URL, "https://") {
		if c.A2CAFile == "" {
			return nil, errors.New("https a2 url needs ca_file= in client.conf")
		}
		b, err := os.ReadFile(c.A2CAFile)
		if err != nil {
			return nil, fmt.Errorf("CA file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(b) {
			return nil, fmt.Errorf("CA file %s has no certificate", c.A2CAFile)
		}
		tr.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS13}
	}
	return &http.Client{Timeout: 90 * time.Second, Transport: tr, CheckRedirect: noRedirect}, nil
}

// discordHTTPClient uses system roots; redirects are not followed (the token stays on Discord).
func discordHTTPClient() *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.DialContext = (&net.Dialer{Timeout: 15 * time.Second}).DialContext
	return &http.Client{Timeout: 30 * time.Second, Transport: tr, CheckRedirect: noRedirect}
}
