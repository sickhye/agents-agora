// Command a2-bridge bridges the AgentsAgora (a2) channel "general" with one Discord
// channel. It is an a2 client member (own token, "discord-bridge") and a Discord bot (REST
// only). Details: README.md in this directory.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
)

const usage = `usage:
  a2-bridge discord [--conf PATH] [--discord-token-file PATH]   run the bridge until stopped
  a2-bridge check   [--conf PATH] [--discord-token-file PATH]   validate config and connectivity
  a2-bridge version
config: ~/.config/agentsagora/bridge.conf (0600); a2 url and ca_file from client.conf
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var err error
	switch os.Args[1] {
	case "discord":
		err = cmdDiscord(ctx, os.Args[2:], os.Stderr)
	case "check":
		err = cmdCheck(ctx, os.Args[2:], os.Stdout)
	case "version":
		fmt.Println("a2-bridge " + appVersion)
	case "-h", "--help", "help":
		fmt.Print(usage)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "a2-bridge: "+err.Error())
		os.Exit(1)
	}
}

func parseFlags(name string, args []string) (config, error) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	conf := fs.String("conf", "", "bridge.conf path")
	tokFile := fs.String("discord-token-file", "", "Discord bot token file (overrides discord_token_file)")
	if err := fs.Parse(args); err != nil {
		return config{}, err
	}
	if fs.NArg() > 0 {
		return config{}, errors.New("unexpected argument " + fs.Arg(0))
	}
	c, err := loadConfig(*conf)
	if err != nil {
		return c, err
	}
	if *tokFile != "" {
		c.DiscordTokenFile = expandHome(*tokFile)
	}
	return c, nil
}

func newBridge(c config, out io.Writer) (*bridge, error) {
	dtok, err := readSecret(c.DiscordTokenFile)
	if err != nil {
		return nil, fmt.Errorf("discord token: %w", err)
	}
	atok, err := readSecret(c.A2TokenFile)
	if err != nil {
		return nil, fmt.Errorf("a2 token: %w", err)
	}
	hc, err := a2HTTPClient(c)
	if err != nil {
		return nil, err
	}
	b := &bridge{cfg: c, out: out, secrets: []string{dtok, atok}, sleep: sleepCtx}
	b.a2 = &a2Client{base: c.A2URL, token: atok, hc: hc}
	b.dc = newDiscordClient(c.DiscordAPI, dtok, discordHTTPClient(), b.logf)
	return b, nil
}

func cmdDiscord(ctx context.Context, args []string, out io.Writer) error {
	c, err := parseFlags("discord", args)
	if err != nil {
		return err
	}
	b, err := newBridge(c, out)
	if err != nil {
		return err
	}
	return b.run(ctx)
}

// cmdCheck prints one ok/FAIL line per check; no token is ever printed.
func cmdCheck(ctx context.Context, args []string, out io.Writer) error {
	c, err := parseFlags("check", args)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "ok   conf %s (a2 %s, discord channel %s)\n", c.ConfPath, c.A2URL, c.DiscordChannelID)
	b, err := newBridge(c, out)
	if err != nil {
		return err
	}
	fails := 0
	fail := func(what string, err error) {
		fails++
		fmt.Fprintf(out, "FAIL %s: %s\n", what, redact(err.Error(), b.secrets...))
	}
	if m, err := b.a2.members(ctx); err != nil {
		fail("a2 members", err)
	} else if err := checkA2Member(m, c.A2Member); err != nil {
		fail("a2 member", err)
	} else {
		fmt.Fprintf(out, "ok   a2 member %s, channels %v\n", m.You, m.YourChannels)
	}
	if u, err := b.dc.me(ctx); err != nil {
		fail("discord users/@me", err)
	} else {
		b.botID = u.ID
		fmt.Fprintf(out, "ok   discord bot %s (id %s)\n", u.Username, u.ID)
	}
	if ch, err := b.dc.channel(ctx, c.DiscordChannelID); err != nil {
		fail("discord channel (View Channel)", err)
	} else {
		fmt.Fprintf(out, "ok   discord channel #%s (id %s)\n", ch.Name, ch.ID)
	}
	if ms, err := b.dc.messagesAfter(ctx, c.DiscordChannelID, "", 5); err != nil {
		fail("discord message read (Read Message History)", err)
	} else {
		fmt.Fprintf(out, "ok   discord message read (%d recent)\n", len(ms))
		for _, m := range ms {
			if m.Author.ID != b.botID && m.Type == 0 && m.Content == "" && len(m.Attachments) == 0 {
				fmt.Fprintln(out, "warn a recent message has empty content: turn on Message Content Intent in the Developer Portal")
				break
			}
		}
	}
	if fails > 0 {
		return fmt.Errorf("%d check(s) failed", fails)
	}
	fmt.Fprintln(out, "ok   all checks passed (sending is not tested)")
	return nil
}
