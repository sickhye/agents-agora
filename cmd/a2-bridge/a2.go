package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
)

// a2Message is the stored message shape of the a2 server (cmd/agentsagora/server.go Message).
type a2Message struct {
	ID      int64  `json:"id"`
	Channel string `json:"channel"`
	From    string `json:"from"`
	To      string `json:"to"`
	Text    string `json:"text"`
	ReplyTo int64  `json:"reply_to,omitempty"`
}

type a2Client struct {
	base  string
	token string
	hc    *http.Client
}

// httpStatusError keeps the status so callers can tell a refusal (4xx) from a failure.
type httpStatusError struct {
	Status int
	Msg    string
}

func (e *httpStatusError) Error() string { return fmt.Sprintf("HTTP %d: %s", e.Status, e.Msg) }

func (a *a2Client) do(ctx context.Context, method, path string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.token)
	req.Header.Set("User-Agent", "a2-bridge/"+appVersion)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(data, &e)
		if e.Error == "" {
			e.Error = http.StatusText(resp.StatusCode)
		}
		return &httpStatusError{Status: resp.StatusCode, Msg: e.Error}
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

type a2Members struct {
	You          string   `json:"you"`
	YourChannels []string `json:"your_channels"`
}

func (a *a2Client) members(ctx context.Context) (a2Members, error) {
	var m a2Members
	err := a.do(ctx, http.MethodGet, "/members", nil, &m)
	return m, err
}

func (a *a2Client) inbox(ctx context.Context, since int64, wait int) ([]a2Message, error) {
	var r struct {
		Messages []a2Message `json:"messages"`
	}
	q := url.Values{"since": {strconv.FormatInt(since, 10)}, "wait": {strconv.Itoa(wait)}, "channel": {bridgeChannel}}
	err := a.do(ctx, http.MethodGet, "/inbox?"+q.Encode(), nil, &r)
	return r.Messages, err
}

func (a *a2Client) send(ctx context.Context, text string) error {
	return a.do(ctx, http.MethodPost, "/send", map[string]any{"channel": bridgeChannel, "to": "all", "text": text}, nil)
}
