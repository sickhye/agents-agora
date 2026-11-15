package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Message size limits: Discord content is at most 2000 characters; a2 text at most 16 KB.
const (
	maxDiscordRunes = 2000
	maxA2Runes      = 4000
	maxA2Bytes      = 16000
	a2Wait          = 50 // long-poll seconds
	maxBackoff      = 60 * time.Second
)

// state is saved after every handled message so a restart neither repeats nor drops.
type state struct {
	A2Since           int64  `json:"a2_since"`
	DiscordAfter      string `json:"discord_after"`
	GatewayIdentified bool   `json:"gateway_identified,omitempty"`
}

type bridge struct {
	cfg     config
	a2      *a2Client
	dc      *discordClient
	self    string // a2 member name of the bridge
	botID   string // Discord user id of the bridge bot
	out     io.Writer
	secrets []string

	mu    sync.Mutex
	st    state
	sleep func(context.Context, time.Duration) error
}

func redact(s string, secrets ...string) string {
	for _, t := range secrets {
		if t != "" {
			s = strings.ReplaceAll(s, t, "[redacted]")
		}
	}
	return s
}

func scrubErr(err error, secrets ...string) error {
	if err == nil {
		return nil
	}
	return errors.New(redact(err.Error(), secrets...))
}

// logf writes one line; every known secret is replaced before it is written.
func (b *bridge) logf(format string, args ...any) {
	line := time.Now().Format("2006-01-02T15:04:05-07:00") + " a2-bridge: " + fmt.Sprintf(format, args...)
	fmt.Fprintln(b.out, redact(line, b.secrets...))
}

func statePath(dir string) string { return filepath.Join(dir, "state.json") }

func loadState(dir string) (state, bool, error) {
	var st state
	data, err := os.ReadFile(statePath(dir))
	if errors.Is(err, os.ErrNotExist) {
		return st, false, nil
	}
	if err != nil {
		return st, false, err
	}
	if err := json.Unmarshal(data, &st); err != nil {
		return st, false, fmt.Errorf("%s: %w", statePath(dir), err)
	}
	return st, true, nil
}

func saveState(dir string, st state) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, _ := json.Marshal(st)
	tmp, err := os.CreateTemp(dir, ".state-*.tmp")
	if err != nil {
		return err
	}
	_ = tmp.Chmod(0o600)
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), statePath(dir))
}

func (b *bridge) update(f func(*state)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	f(&b.st)
	if err := saveState(b.cfg.StateDir, b.st); err != nil {
		b.logf("state save failed: %v", err)
	}
}

func (b *bridge) snapshot() state {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.st
}

// clip cuts s to at most maxRunes characters and maxBytes bytes, ending with "…".
func clip(s string, maxRunes, maxBytes int) string {
	if utf8.RuneCountInString(s) <= maxRunes && len(s) <= maxBytes {
		return s
	}
	const ell = "…"
	n, cut := 0, 0
	for i := range s {
		_, sz := utf8.DecodeRuneInString(s[i:])
		if n >= maxRunes-1 || i+sz+len(ell) > maxBytes {
			break
		}
		n++
		cut = i + sz
	}
	return s[:cut] + ell
}

// The two formats the bridge writes. A message that already starts with one of them was
// written by a bridge (this one or another), so it is never forwarded again.
var (
	discordFmtRe = regexp.MustCompile(`^(↪ #\d+ )?\*\*[^*\n]{1,80}\*\*: `)
	replyFmtRe   = regexp.MustCompile(`^↪ #\d+`)
)

func hasBridgePrefix(text string) bool {
	return discordFmtRe.MatchString(text) || replyFmtRe.MatchString(text)
}

func toDiscordText(m a2Message) string {
	s := "**" + m.From + "**: " + m.Text
	if m.ReplyTo > 0 {
		s = fmt.Sprintf("↪ #%d %s", m.ReplyTo, s)
	}
	return clip(s, maxDiscordRunes, 4*maxDiscordRunes)
}

func oneLine(s string) string {
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		return r
	}, s)
	return clip(strings.TrimSpace(s), 64, 256)
}

func toA2Text(m discordMessage) string {
	parts := []string{}
	if t := strings.TrimSpace(m.Content); t != "" {
		parts = append(parts, t)
	}
	for _, a := range m.Attachments {
		if a.URL != "" {
			parts = append(parts, a.URL)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return clip(oneLine(m.displayName())+": "+strings.Join(parts, "\n"), maxA2Runes, maxA2Bytes)
}

// forwardA2 reports whether an a2 inbox message goes to Discord.
func (b *bridge) forwardA2(m a2Message) bool {
	return m.Channel == bridgeChannel && m.To == "all" && m.From != b.self &&
		strings.TrimSpace(m.Text) != "" && !hasBridgePrefix(m.Text)
}

// forwardDiscord reports whether a Discord message goes to a2.
func (b *bridge) forwardDiscord(m discordMessage) bool {
	if m.Author.ID == b.botID || (m.Type != 0 && m.Type != 19) {
		return false
	}
	return !hasBridgePrefix(m.Content) && toA2Text(m) != ""
}

// refused: a 4xx about the message itself (400, 413, 422 lint) means it will never be
// accepted; drop it. 401/403/404 are setup problems (token, permission, channel) and 429 is
// a limit: keep the message and retry with back-off.
func refused(err error) bool {
	var he *httpStatusError
	if !errors.As(err, &he) || he.Status < 400 || he.Status >= 500 {
		return false
	}
	switch he.Status {
	case 401, 403, 404, 429:
		return false
	}
	return true
}

type backoff struct{ d time.Duration }

func (k *backoff) next() time.Duration {
	if k.d == 0 {
		k.d = 3 * time.Second
	} else {
		k.d = min(2*k.d, maxBackoff)
	}
	return k.d
}
func (k *backoff) reset() { k.d = 0 }

// init sets ids and the start position. Without saved state it starts at "now" on both
// sides, so old history is not replayed.
func (b *bridge) init(ctx context.Context) error {
	m, err := b.a2.members(ctx)
	if err != nil {
		return fmt.Errorf("a2 members: %w", scrubErr(err, b.secrets...))
	}
	if err := checkA2Member(m, b.cfg.A2Member); err != nil {
		return err
	}
	b.self = m.You
	u, err := b.dc.me(ctx)
	if err != nil {
		return fmt.Errorf("discord users/@me: %w", err)
	}
	b.botID = u.ID
	st, ok, err := loadState(b.cfg.StateDir)
	if err != nil {
		return err
	}
	if !ok {
		msgs, err := b.a2.inbox(ctx, 0, 0)
		if err != nil {
			return fmt.Errorf("a2 inbox: %w", err)
		}
		for _, x := range msgs {
			st.A2Since = max(st.A2Since, x.ID)
		}
		last, err := b.dc.messagesAfter(ctx, b.cfg.DiscordChannelID, "", 1)
		if err != nil {
			return fmt.Errorf("discord channel read: %w", err)
		}
		st.DiscordAfter = "0"
		if len(last) > 0 {
			st.DiscordAfter = last[len(last)-1].ID
		}
		b.logf("no saved state: starting from now (a2 #%d, discord %s)", st.A2Since, st.DiscordAfter)
	}
	b.mu.Lock()
	b.st = st
	b.mu.Unlock()
	b.update(func(*state) {})
	return nil
}

func checkA2Member(m a2Members, want string) error {
	if m.You != want {
		return fmt.Errorf("a2 token belongs to %q, want %q (a2_member in bridge.conf)", m.You, want)
	}
	for _, ch := range m.YourChannels {
		if ch != bridgeChannel {
			return fmt.Errorf("a2 member %q may use channel %q; the bridge member must be %s-only", m.You, ch, bridgeChannel)
		}
	}
	if len(m.YourChannels) == 0 {
		return fmt.Errorf("a2 member %q has no channel", m.You)
	}
	return nil
}

// a2ToDiscord long-polls a2 general and posts each new message to Discord.
func (b *bridge) a2ToDiscord(ctx context.Context) {
	var bo backoff
	for ctx.Err() == nil {
		since := b.snapshot().A2Since
		msgs, err := b.a2.inbox(ctx, since, a2Wait)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			d := bo.next()
			b.logf("a2 inbox failed: %v; retry in %s", scrubErr(err, b.secrets...), d)
			_ = b.sleep(ctx, d)
			continue
		}
		sort.Slice(msgs, func(i, j int) bool { return msgs[i].ID < msgs[j].ID })
		failed := false
		for _, m := range msgs {
			if m.ID <= since {
				continue
			}
			if b.forwardA2(m) {
				if err := b.dc.post(ctx, b.cfg.DiscordChannelID, toDiscordText(m)); err != nil {
					if ctx.Err() != nil {
						return
					}
					if !refused(err) {
						d := bo.next()
						b.logf("discord post of a2 #%d failed: %v; retry in %s", m.ID, err, d)
						_ = b.sleep(ctx, d)
						failed = true
						break
					}
					b.logf("discord refused a2 #%d: %v; skipped", m.ID, err)
				} else {
					b.logf("a2 #%d -> discord", m.ID)
				}
			}
			since = m.ID
			b.update(func(s *state) { s.A2Since = m.ID })
		}
		if !failed {
			bo.reset()
		}
	}
}

// discordToA2 polls the Discord channel and sends each new message to a2 general.
func (b *bridge) discordToA2(ctx context.Context) {
	var bo backoff
	wait := time.Duration(0)
	for {
		if err := b.sleep(ctx, wait); err != nil {
			return
		}
		wait = b.cfg.PollEvery
		after := b.snapshot().DiscordAfter
		ms, err := b.dc.messagesAfter(ctx, b.cfg.DiscordChannelID, after, 100)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			wait = max(bo.next(), b.cfg.PollEvery)
			b.logf("discord read failed: %v; retry in %s", err, wait)
			continue
		}
		failed := false
		for _, m := range ms {
			if !snowLess(after, m.ID) {
				continue
			}
			if b.forwardDiscord(m) {
				if err := b.a2.send(ctx, toA2Text(m)); err != nil {
					if ctx.Err() != nil {
						return
					}
					if !refused(err) {
						wait = max(bo.next(), b.cfg.PollEvery)
						b.logf("a2 send of discord %s failed: %v; retry in %s", m.ID, scrubErr(err, b.secrets...), wait)
						failed = true
						break
					}
					// 422 = general lint; the server error never repeats the text.
					b.logf("a2 refused discord %s: %v; skipped", m.ID, scrubErr(err, b.secrets...))
				} else {
					b.logf("discord %s -> a2", m.ID)
				}
			}
			after = m.ID
			b.update(func(s *state) { s.DiscordAfter = m.ID })
		}
		if !failed {
			bo.reset()
			if len(ms) == 100 {
				wait = 0 // more are waiting
			}
		}
	}
}

func (b *bridge) run(ctx context.Context) error {
	if err := b.init(ctx); err != nil {
		return err
	}
	if !b.snapshot().GatewayIdentified {
		// Discord wants a bot to identify on the gateway once before it can post.
		if err := gatewayIdentifyOnce(ctx, b.cfg.DiscordGateway, b.dc.token); err != nil {
			b.logf("gateway identify failed (posting may fail until it works): %v", scrubErr(err, b.secrets...))
		} else {
			b.logf("gateway identify ok (one time)")
			b.update(func(s *state) { s.GatewayIdentified = true })
		}
	}
	b.logf("running: a2 %s <-> discord channel %s as %s / bot %s", bridgeChannel, b.cfg.DiscordChannelID, b.self, b.botID)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); b.a2ToDiscord(ctx) }()
	go func() { defer wg.Done(); b.discordToA2(ctx) }()
	wg.Wait()
	b.logf("stopped")
	return nil
}
