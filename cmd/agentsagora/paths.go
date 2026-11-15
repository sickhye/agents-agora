package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
)

// Channels are fixed in the server; there is no API to create one. Each channel has an
// explicit visibility (channelDefs), never inferred from its name:
//
//	general: public. Outside agents may join; work content is rejected (deny-list lint).
//	team: private. Registered members only.
//
// Display: "#" before a public name, a lock before a private one (channelLabel).
const (
	chTeam    = "team"
	chGeneral = "general"
)

// Visibility of a channel.
const (
	visPublic  = "public"
	visPrivate = "private"
)

// channelDef is one fixed channel.
type channelDef struct {
	Name       string `json:"name"`
	Visibility string `json:"visibility"` // visPublic or visPrivate
	Purpose    string `json:"purpose"`
}

// channelDefs is the channel table, in display order.
var channelDefs = []channelDef{
	{chTeam, visPrivate, "work only"},
	{chGeneral, visPublic, "anything that is not work"},
}

var allChannels = func() []string {
	var out []string
	for _, d := range channelDefs {
		out = append(out, d.Name)
	}
	return out
}()

// channelDefOf returns the definition of a channel name.
func channelDefOf(name string) (channelDef, bool) {
	for _, d := range channelDefs {
		if d.Name == name {
			return d, true
		}
	}
	return channelDef{}, false
}

// isPublic reports whether the channel exists and is public. The work-content lint and the
// "outside agents may join" rule hang on this, not on a channel name.
func isPublic(name string) bool {
	d, ok := channelDefOf(name)
	return ok && d.Visibility == visPublic
}

// visMark is the prefix shown before a channel name: "#" public, a lock private.
func visMark(visibility string) string {
	if visibility == visPrivate {
		return "\U0001F512"
	}
	return "#"
}

// channelLabel is the display form of a channel: "#general", "🔒team". An unknown name is
// shown with "#".
func channelLabel(name string) string {
	if d, ok := channelDefOf(name); ok {
		return visMark(d.Visibility) + name
	}
	return "#" + name
}

// channelLabels maps channelLabel over a list.
func channelLabels(names []string) []string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = channelLabel(n)
	}
	return out
}

// memberChannels is the built-in member table: which channels each member may read and
// write. Replace it with your own members.json in the config dir (loadMemberTable).
// Other tools and bots outside the table are not members.
var memberChannels = map[string][]string{
	"alice":   {chTeam, chGeneral},
	"bob":     {chTeam, chGeneral},
	"carol":   {chTeam, chGeneral},
	"dave":    {chTeam, chGeneral},
	"erin":    {chGeneral},
	"frank":   {chGeneral},
	"advisor": {chTeam},
	// discord-bridge is the a2-bridge relay (cmd/a2-bridge). It is general only and stays
	// so: generalOnlyMembers strips team even if the table is edited, and it cannot be
	// renamed (names.go renameMember).
	"discord-bridge": {chGeneral},
}

// generalOnlyMembers never get a private channel (team), whatever memberChannels says (nameTable.channels).
var generalOnlyMembers = []string{"discord-bridge"}

// DefaultMembers is the member table in a fixed order.
var DefaultMembers = []string{"alice", "bob", "carol", "dave", "erin", "frank", "advisor", "discord-bridge"}

// memberTableFile is the optional member table in the config dir:
//
//	{"members": {"alice": ["team", "general"], "bob": ["general"]}, "general_only": ["discord-bridge"]}
//
// The key order of "members" is not kept; members are listed sorted. Without the file the
// built-in table above is used.
const memberTableFile = "members.json"

// loadMemberTable replaces the built-in member table by <config dir>/members.json when
// that file exists. A bad file is an error (the server must not start with a wrong table).
func loadMemberTable() error {
	b, err := os.ReadFile(filepath.Join(configDir(), memberTableFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var f struct {
		Members     map[string][]string `json:"members"`
		GeneralOnly []string            `json:"general_only"`
	}
	if err := json.Unmarshal(b, &f); err != nil {
		return fmt.Errorf("%s: %w", memberTableFile, err)
	}
	if len(f.Members) == 0 {
		return fmt.Errorf("%s: no members", memberTableFile)
	}
	table := map[string][]string{}
	var names []string
	for n, chs := range f.Members {
		if err := checkMemberName(n); err != nil {
			return fmt.Errorf("%s: %w", memberTableFile, err)
		}
		for _, c := range chs {
			if !slices.Contains(allChannels, c) {
				return fmt.Errorf("%s: member %s: unknown channel %q", memberTableFile, n, c)
			}
		}
		table[n] = chs
		names = append(names, n)
	}
	slices.Sort(names)
	memberChannels, generalOnlyMembers, DefaultMembers = table, f.GeneralOnly, names
	return nil
}

// channelsOf returns the channels of a current member name (nil for a non-member or an old
// name; renames: names.go).
func channelsOf(member string) []string { return currentNames().channels(member) }

func canUse(member, channel string) bool { return slices.Contains(channelsOf(member), channel) }

func tlsDir() string { return filepath.Join(configDir(), "tls") }

const allTarget = "all"

// Name and paths. Config lives in $AGENTSAGORA_CONFIG_DIR or $XDG_CONFIG_HOME/agentsagora
// (default ~/.config/agentsagora), state in $AGENTSAGORA_STATE_DIR or
// $XDG_STATE_HOME/agentsagora (default ~/.local/state/agentsagora).
const (
	appName     = "agentsagora"
	displayName = "AgentsAgora (A2)"
	envPrefix   = "AGENTSAGORA_"
)

// version is the release version. A release build sets it with
// -ldflags "-X main.version=v0.1.0"; a plain go build or go run reports "dev".
var version = "dev"

// userAgent is sent on client requests.
func userAgent() string { return "AgentsAgora-A2/" + version }

// getenv reads AGENTSAGORA_<name>.
func getenv(name string) string { return os.Getenv(envPrefix + name) }

func homeDir() string { h, _ := os.UserHomeDir(); return h }

// xdgDir returns $<env>/agentsagora when env is set to an absolute path, else
// ~/<fallback...>/agentsagora.
func xdgDir(env string, fallback ...string) string {
	if d := os.Getenv(env); filepath.IsAbs(d) {
		return filepath.Join(d, appName)
	}
	return filepath.Join(append(append([]string{homeDir()}, fallback...), appName)...)
}

func defaultConfigDir() string { return xdgDir("XDG_CONFIG_HOME", ".config") }
func defaultStateDir() string  { return xdgDir("XDG_STATE_HOME", ".local", "state") }

func configDir() string {
	if d := getenv("CONFIG_DIR"); d != "" {
		return d
	}
	return defaultConfigDir()
}

func stateDir() string {
	if d := getenv("STATE_DIR"); d != "" {
		return d
	}
	return defaultStateDir()
}

func tokensPath() string { return filepath.Join(configDir(), "tokens.json") }

// clientConfig is the client side setting: server URL and token file path.
type clientConfig struct {
	URL       string
	TokenFile string
	CAFile    string // CA certificate to pin for https URLs
}

// loadClientConfig reads client.conf (key=value, # comments), then env overrides it.
func loadClientConfig() (clientConfig, error) {
	var c clientConfig
	p := filepath.Join(configDir(), "client.conf")
	if f, err := os.Open(p); err == nil {
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			k, v, ok := strings.Cut(line, "=")
			if !ok {
				continue
			}
			k, v = strings.TrimSpace(k), strings.TrimSpace(v)
			switch k {
			case "url":
				c.URL = v
			case "token_file":
				c.TokenFile = expandHome(v)
			case "ca_file":
				c.CAFile = expandHome(v)
			}
		}
		f.Close()
	} else if !errors.Is(err, os.ErrNotExist) {
		return c, err
	}
	if v := getenv("URL"); v != "" {
		c.URL = v
	}
	if v := getenv("TOKEN_FILE"); v != "" {
		c.TokenFile = expandHome(v)
	}
	if tokenFileFlag != "" { // a2 --token-file FILE (main.go parseGlobalFlags): flag > env > config
		c.TokenFile = tokenFileFlag
	}
	if v := getenv("CA_FILE"); v != "" {
		c.CAFile = expandHome(v)
	}
	if c.URL == "" {
		return c, errors.New("no server URL: set AGENTSAGORA_URL or url= in " + p)
	}
	if c.TokenFile == "" {
		return c, errors.New("no token file: use --token-file FILE, set AGENTSAGORA_TOKEN_FILE or token_file= in " + p)
	}
	c.URL = strings.TrimRight(c.URL, "/")
	return c, nil
}

func expandHome(p string) string {
	// Also "~\dir" on Windows (filepath.Separator is '/' elsewhere, so nothing changes there).
	if p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, "~"+string(filepath.Separator)) {
		h, _ := os.UserHomeDir()
		return filepath.Join(h, strings.TrimPrefix(p, "~"))
	}
	return p
}

// openToOthers reports whether a secret file (token, TLS key) has group or other mode bits.
// Windows has no Unix modes: Perm reports 0666 (0444 when read-only) for every file and
// Chmod only toggles the read-only flag, so the check would refuse every file there. It is
// off on Windows, where the ACL of the user profile (config dir) guards the file instead.
func openToOthers(fi os.FileInfo) bool {
	return runtime.GOOS != "windows" && fi.Mode().Perm()&0o077 != 0
}

// readToken reads a token file. It refuses files readable by group or others (not on Windows,
// see openToOthers).
func readToken(path string) (string, error) {
	st, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if openToOthers(st) {
		return "", errors.New("token file " + path + " must be mode 0600 (chmod 600 it)")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	t := strings.TrimSpace(string(b))
	if t == "" {
		return "", errors.New("token file " + path + " is empty")
	}
	return t, nil
}
