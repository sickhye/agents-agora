package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"sync"
	"time"
)

type discordUser struct {
	ID         string `json:"id"`
	Username   string `json:"username"`
	GlobalName string `json:"global_name"`
	Bot        bool   `json:"bot"`
}

type discordMessage struct {
	ID          string      `json:"id"`
	Type        int         `json:"type"`
	Content     string      `json:"content"`
	Author      discordUser `json:"author"`
	WebhookID   string      `json:"webhook_id"`
	Attachments []struct {
		URL      string `json:"url"`
		Filename string `json:"filename"`
	} `json:"attachments"`
	Member *struct {
		Nick string `json:"nick"`
	} `json:"member"`
}

// displayName: server nickname, then global display name, then user name.
func (m discordMessage) displayName() string {
	if m.Member != nil && m.Member.Nick != "" {
		return m.Member.Nick
	}
	if m.Author.GlobalName != "" {
		return m.Author.GlobalName
	}
	return m.Author.Username
}

// discordClient is a small REST client that honours 429 retry_after and the X-RateLimit
// headers per route. The token only goes into the Authorization header.
type discordClient struct {
	base  string
	token string
	hc    *http.Client
	sleep func(context.Context, time.Duration) error
	logf  func(string, ...any)

	mu        sync.Mutex
	notBefore map[string]time.Time // route -> earliest next request
}

func newDiscordClient(base, token string, hc *http.Client, logf func(string, ...any)) *discordClient {
	return &discordClient{base: base, token: token, hc: hc, sleep: sleepCtx, logf: logf, notBefore: map[string]time.Time{}}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

const maxRateWait = 10 * time.Minute

func secs(s string) (time.Duration, bool) {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f < 0 {
		return 0, false
	}
	d := time.Duration(f * float64(time.Second))
	return min(d, maxRateWait), true
}

// do sends one request on route (a fixed label, never containing the token), retrying a
// 429 up to 5 times after retry_after.
func (d *discordClient) do(ctx context.Context, route, method, path string, body any, out any) error {
	var payload []byte
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		payload = b
	}
	for attempt := 0; ; attempt++ {
		d.mu.Lock()
		wait := time.Until(d.notBefore[route])
		if g := time.Until(d.notBefore["global"]); g > wait {
			wait = g
		}
		d.mu.Unlock()
		if err := d.sleep(ctx, wait); err != nil {
			return err
		}
		var rd io.Reader
		if payload != nil {
			rd = bytes.NewReader(payload)
		}
		req, err := http.NewRequestWithContext(ctx, method, d.base+path, rd)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bot "+d.token)
		req.Header.Set("User-Agent", "DiscordBot (https://localhost/a2-bridge, "+appVersion+")")
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := d.hc.Do(req)
		if err != nil {
			return fmt.Errorf("discord %s: %w", route, scrubErr(err, d.token))
		}
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		resp.Body.Close()
		d.noteLimits(route, resp.Header)
		if resp.StatusCode == http.StatusTooManyRequests {
			var rl struct {
				RetryAfter float64 `json:"retry_after"`
				Global     bool    `json:"global"`
			}
			_ = json.Unmarshal(data, &rl)
			ra := time.Duration(rl.RetryAfter * float64(time.Second))
			if ra <= 0 {
				ra, _ = secs(resp.Header.Get("Retry-After"))
			}
			ra = min(max(ra, 100*time.Millisecond), maxRateWait)
			key := route
			if rl.Global || resp.Header.Get("X-RateLimit-Global") == "true" {
				key = "global"
			}
			d.mu.Lock()
			d.notBefore[key] = time.Now().Add(ra)
			d.mu.Unlock()
			d.logf("discord %s: rate limited, retry after %s", route, ra.Round(time.Millisecond))
			if attempt >= 4 {
				return &httpStatusError{Status: 429, Msg: "rate limited"}
			}
			continue
		}
		if resp.StatusCode/100 != 2 {
			var e struct {
				Message string `json:"message"`
				Code    int    `json:"code"`
			}
			_ = json.Unmarshal(data, &e)
			msg := e.Message
			if msg == "" {
				msg = http.StatusText(resp.StatusCode)
			}
			if e.Code != 0 {
				msg = fmt.Sprintf("%s (code %d)", msg, e.Code)
			}
			return &httpStatusError{Status: resp.StatusCode, Msg: "discord " + route + ": " + redact(msg, d.token)}
		}
		if out != nil {
			return json.Unmarshal(data, out)
		}
		return nil
	}
}

// noteLimits: when X-RateLimit-Remaining is 0, wait X-RateLimit-Reset-After before the
// next request on this route.
func (d *discordClient) noteLimits(route string, h http.Header) {
	if h.Get("X-RateLimit-Remaining") != "0" {
		return
	}
	ra, ok := secs(h.Get("X-RateLimit-Reset-After"))
	if !ok {
		return
	}
	d.mu.Lock()
	if t := time.Now().Add(ra); t.After(d.notBefore[route]) {
		d.notBefore[route] = t
	}
	d.mu.Unlock()
}

func (d *discordClient) me(ctx context.Context) (discordUser, error) {
	var u discordUser
	err := d.do(ctx, "me", http.MethodGet, "/users/@me", nil, &u)
	return u, err
}

type discordChannel struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	GuildID string `json:"guild_id"`
}

func (d *discordClient) channel(ctx context.Context, id string) (discordChannel, error) {
	var c discordChannel
	err := d.do(ctx, "channel", http.MethodGet, "/channels/"+url.PathEscape(id), nil, &c)
	return c, err
}

// messagesAfter returns up to 100 messages after id ("" = the latest), oldest first.
func (d *discordClient) messagesAfter(ctx context.Context, channelID, after string, limit int) ([]discordMessage, error) {
	q := url.Values{"limit": {strconv.Itoa(limit)}}
	if after != "" {
		q.Set("after", after)
	}
	var ms []discordMessage
	if err := d.do(ctx, "get-messages", http.MethodGet, "/channels/"+url.PathEscape(channelID)+"/messages?"+q.Encode(), nil, &ms); err != nil {
		return nil, err
	}
	sort.Slice(ms, func(i, j int) bool { return snowLess(ms[i].ID, ms[j].ID) })
	return ms, nil
}

func (d *discordClient) post(ctx context.Context, channelID, content string) error {
	body := map[string]any{
		"content": content,
		// a2 text never pings anyone on Discord (@everyone, roles, users).
		"allowed_mentions": map[string]any{"parse": []string{}},
	}
	return d.do(ctx, "post-message", http.MethodPost, "/channels/"+url.PathEscape(channelID)+"/messages", body, nil)
}

// snowLess compares Discord ids (decimal uint64 strings).
func snowLess(a, b string) bool {
	x, _ := strconv.ParseUint(a, 10, 64)
	y, _ := strconv.ParseUint(b, 10, 64)
	return x < y
}
