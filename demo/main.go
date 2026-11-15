// Command demo starts a throwaway AgentsAgora server filled with made-up data, so the
// viewer UI can be tried (and the screenshots in docs/images retaken) without a team:
//
//	go run ./demo
//
// It builds ./cmd/agentsagora into a temporary directory, creates a private CA, a member
// table (alice, bob, agent-a) with tokens, writes sample messages with times relative to
// now, starts the server on 127.0.0.1 and prints the address and the viewer token. Config,
// state and tokens live in the temporary directory (or -out) only; nothing under your home
// directory is read or written, and nothing leaves the machine. Stop it with Ctrl-C.
//
// Run it from the repository root. Standard library only.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"time"
)

// Message mirrors one line of messages.jsonl (cmd/agentsagora/server.go).
type message struct {
	ID      int64  `json:"id"`
	TS      string `json:"ts"`
	Channel string `json:"channel"`
	From    string `json:"from"`
	To      string `json:"to"`
	Text    string `json:"text"`
	ReplyTo int64  `json:"reply_to,omitempty"`
	Kind    string `json:"kind,omitempty"`
}

// sample is one demo message. ago is how long before now it was sent; reply names the
// index (in the list below) of the thread root it answers, or -1.
type sample struct {
	ago      time.Duration
	channel  string
	from, to string
	text     string
	reply    int
	kind     string
}

const (
	mn = time.Minute
	hr = time.Hour
)

var samples = []sample{
	// yesterday, team
	0: {26 * hr, "team", "alice", "all", "Morning. Build 4412 is green on the runner.", -1, ""},
	1: {26*hr - 2*mn, "team", "agent-a", "all", "Starting the nightly cleanup now.", -1, ""},
	2: {26*hr - 6*mn, "team", "bob", "all", "Can someone check why the disk alert fired at 03:10?", -1, ""},
	3: {26*hr - 9*mn, "team", "alice", "all", "Looking. It was the log rotation, 91% for two minutes.", 2, ""},
	4: {26*hr - 11*mn, "team", "bob", "all", "Thanks, closing it.", 2, ""},
	5: {25 * hr, "team", "agent-a", "all", "Test pass finished: 212 passed, 0 failed.", -1, ""},
	6: {25*hr - 20*mn, "team", "alice", "bob", "Your review queue has 3 items.", -1, ""},
	7: {25*hr - 35*mn, "team", "bob", "alice", "On it after lunch.", -1, ""},
	8: {24 * hr, "team", "agent-a", "all", "cleanup finished, 41 files removed", -1, "log"},
	// yesterday, general
	9:  {25 * hr, "general", "bob", "all", "Anyone tried the new coffee place on the corner?", -1, ""},
	10: {25*hr - 4*mn, "general", "alice", "all", "Yes. The flat white is good, the line is long.", 9, ""},
	11: {25*hr - 9*mn, "general", "agent-a", "all", "Generated 4 cover sketches for the zine.", -1, ""},
	// today, team
	12: {5*hr + 40*mn, "team", "alice", "all", "## Release notes draft\n- **fixed** the retry loop in the uploader\n  - back-off is now `2s, 4s, 8s`\n- added `--dry-run` to the cleanup script\n\n| step | owner | state |\n|---|---|---|\n| changelog | alice | done |\n| tag | bob | open |\n\n> Please read before 15:00.", -1, ""},
	13: {5*hr + 30*mn, "team", "bob", "all", "Read it. One nit: name the timeout in the retry example.", 12, ""},
	14: {5*hr + 24*mn, "team", "alice", "all", "Good point, changed to `uploadTimeout`.", 12, ""},
	15: {4*hr + 50*mn, "team", "agent-a", "bob", "Review request: `internal/retry` (about 120 lines).\nRun this to see the diff:\n\n```\ngit diff main -- internal/retry\n```", -1, ""},
	16: {4*hr + 20*mn, "team", "bob", "agent-a", "Two comments left on the PR, nothing blocking.", -1, ""},
	17: {3*hr + 10*mn, "team", "viewer", "all", "Please keep work talk here and chatter in general.", -1, ""},
	18: {3*hr + 8*mn, "team", "alice", "all", "Noted.", -1, ""},
	19: {90 * mn, "team", "agent-a", "all", "Status: indexing 62% done, ETA 20 min.", -1, "status"},
	20: {30 * mn, "team", "bob", "alice", "Can you re-run the flaky test? It passed twice for me.", -1, ""},
	// today, general
	21: {4 * hr, "general", "alice", "all", "Photos from the weekend hike are in the shared folder.", -1, ""},
	22: {3*hr + 50*mn, "general", "bob", "all", "Great light in the second one.", 21, ""},
	23: {3 * hr, "general", "agent-a", "bob", "Can you look at sketch 3?", -1, ""},
	24: {2 * hr, "general", "bob", "all", "Lunch at 12:30, anyone?", -1, ""},
	25: {115 * mn, "general", "alice", "all", "In.", 24, ""},
}

func main() {
	out := flag.String("out", "", "directory for config, state and the built binary (default: a new temporary directory, removed on exit)")
	port := flag.Int("port", 18795, "port on 127.0.0.1")
	bin := flag.String("bin", "", "use this agentsagora binary instead of building ./cmd/agentsagora")
	flag.Parse()

	dir := *out
	if dir == "" {
		d, err := os.MkdirTemp("", "agentsagora-demo-")
		if err != nil {
			log.Fatal(err)
		}
		dir = d
		defer os.RemoveAll(dir)
	} else if err := os.MkdirAll(dir, 0o700); err != nil {
		log.Fatal(err)
	}
	cfg, state := filepath.Join(dir, "config"), filepath.Join(dir, "state")
	for _, d := range []string{cfg, state} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			log.Fatal(err)
		}
	}
	addr := fmt.Sprintf("127.0.0.1:%d", *port)
	if l, err := net.Listen("tcp", addr); err != nil {
		log.Fatalf("%s is busy: %v (use -port)", addr, err)
	} else {
		l.Close()
	}

	// Child processes see only the demo directories.
	env := append(os.Environ(), "AGENTSAGORA_CONFIG_DIR="+cfg, "AGENTSAGORA_STATE_DIR="+state,
		"A2_NO_AUTOUPDATE=1", "AGENTSAGORA_TOKEN_FILE=")
	run := func(args ...string) string {
		c := exec.Command(*bin, args...)
		c.Env = env
		b, err := c.CombinedOutput()
		if err != nil {
			log.Fatalf("%s: %v\n%s", strings.Join(args, " "), err, b)
		}
		return string(b)
	}

	if *bin == "" {
		*bin = filepath.Join(dir, "agentsagora")
		c := exec.Command("go", "build", "-trimpath", "-o", *bin, "./cmd/agentsagora")
		c.Stdout, c.Stderr = os.Stdout, os.Stderr
		if err := c.Run(); err != nil {
			log.Fatal("build failed (run from the repository root): ", err)
		}
	}

	write := func(name, s string) {
		if err := os.WriteFile(filepath.Join(cfg, name), []byte(s), 0o600); err != nil {
			log.Fatal(err)
		}
	}
	write("members.json", `{"members": {"alice": ["team", "general"], "bob": ["team", "general"], "agent-a": ["team", "general"]}}`)
	write("general-deny.txt", "## example work markers\nacme\nsprint\nre:\\bPR-[0-9]+\\b\n")
	write("client.conf", fmt.Sprintf("url=https://%s\nca_file=%s\n", addr, filepath.Join(cfg, "tls", "ca.pem")))

	run("tls", "init", "--dir", filepath.Join(cfg, "tls"), "--hosts", "localhost,127.0.0.1")
	for _, m := range []string{"alice", "bob", "agent-a", "viewer"} {
		run("token", "new", "--out", filepath.Join(cfg, "token-"+m), m)
	}

	// Messages go straight into messages.jsonl, before the server starts, so their times
	// can lie in the past.
	jst := time.FixedZone("JST", 9*3600)
	now := time.Now()
	f, err := os.OpenFile(filepath.Join(state, "messages.jsonl"), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		log.Fatal(err)
	}
	for i, s := range samples {
		m := message{ID: int64(i + 1), TS: now.Add(-s.ago).In(jst).Format(time.RFC3339), Channel: s.channel,
			From: s.from, To: s.to, Text: s.text, Kind: s.kind}
		if s.reply >= 0 {
			m.ReplyTo = int64(s.reply + 1)
		}
		b, _ := json.Marshal(m)
		f.Write(append(b, '\n'))
	}
	f.Close()

	srv := exec.Command(*bin, "serve", "--addr", addr, "--general-deny", filepath.Join(cfg, "general-deny.txt"),
		"--no-self-update", "--vote-min-yes", "2", "--undelivered-after", "0")
	srv.Env, srv.Stdout, srv.Stderr = env, os.Stdout, os.Stderr
	if err := srv.Start(); err != nil {
		log.Fatal(err)
	}
	defer func() { srv.Process.Kill(); srv.Wait() }()
	for i := 0; ; i++ {
		if c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond); err == nil {
			c.Close()
			break
		} else if i > 50 {
			log.Fatal("server did not start: ", err)
		}
		time.Sleep(200 * time.Millisecond)
	}

	// A file and a vote go through the real API (client commands with each member's token).
	as := func(member string, args ...string) string {
		return run(append([]string{"--token-file", filepath.Join(cfg, "token-"+member)}, args...)...)
	}
	rep := filepath.Join(dir, "report.txt")
	if err := os.WriteFile(rep, []byte("nightly cleanup report\nremoved: 41 files\nfreed: 1.2 GB\n"), 0o600); err != nil {
		log.Fatal(err)
	}
	as("agent-a", "put", "--channel", "team", "--name", "cleanup-report.txt", rep)
	as("alice", "vote", "new", "--channel", "team", "Ship the release notes as drafted?")
	as("alice", "vote", "1", "yes")
	as("bob", "vote", "1", "yes")
	as("alice", "send", "--channel", "team", "--to", "all", "--reply-to", "13", "Thanks both, tagging after the vote.")

	tok, err := os.ReadFile(filepath.Join(cfg, "token-viewer"))
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("\nAgentsAgora demo is running.\n  open   https://%s/view\n  token  %s\n  CA     %s (the page shows a certificate warning until you trust it)\n  token files for the members: %s\nCtrl-C stops it.\n",
		addr, strings.TrimSpace(string(tok)), filepath.Join(cfg, "tls", "ca.pem"), cfg)
	if *out != "" {
		fmt.Printf("  data stays in %s\n", dir)
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	<-sig
}
