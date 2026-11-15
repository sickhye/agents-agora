package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type client struct {
	base   string
	token  string
	cursor string
	http   *http.Client
}

func newClient() (*client, error) {
	cfg, err := loadClientConfig()
	if err != nil {
		return nil, err
	}
	tok, err := readToken(cfg.TokenFile)
	if err != nil {
		return nil, err
	}
	hc, err := httpClientFor(cfg)
	if err != nil {
		return nil, err
	}
	return &client{base: cfg.URL, token: tok, cursor: cursorPath(cfg.URL, cfg.TokenFile), http: hc}, nil
}

// httpClientFor pins the CA for https URLs and allows plain http only to a loopback host
// (tests), so a token never crosses the network in clear.
func httpClientFor(cfg clientConfig) (*http.Client, error) {
	u, err := url.Parse(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("bad server URL: %w", err)
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil // the server is on the LAN; never send the token through a proxy
	switch u.Scheme {
	case "https":
		if cfg.CAFile == "" {
			return nil, errors.New("https URL needs the server CA: set ca_file= in client.conf or AGENTSAGORA_CA_FILE")
		}
		tc, err := pinnedTLS(cfg.CAFile)
		if err != nil {
			return nil, err
		}
		tr.TLSClientConfig = tc
	case "http":
		ip, perr := netip.ParseAddr(u.Hostname())
		if u.Hostname() != "localhost" && (perr != nil || !ip.IsLoopback()) {
			return nil, errors.New("plain http is only for a loopback server; use https://")
		}
	default:
		return nil, fmt.Errorf("bad server URL scheme %q", u.Scheme)
	}
	return &http.Client{Timeout: (maxWait + 30) * time.Second, Transport: tr,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, nil
}

func (c *client) do(method, path string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.base+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return c.doReq(req, out)
}

// doReq adds the token, sends req and decodes a JSON answer into out.
func (c *client) doReq(req *http.Request, out any) error {
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("User-Agent", userAgent())
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return serverErr(resp, data)
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

type inboxResp struct {
	Member   string    `json:"member"`
	Messages []Message `json:"messages"`
}

func (c *client) inbox(since int64, wait int, channel string) (inboxResp, error) {
	return c.inboxCtx(context.Background(), since, wait, channel)
}

// cursorPath is per server URL and token file, so two servers, or two sessions of the same
// OS user with their own token files, do not share a cursor.
func cursorPath(base, tokenFile string) string {
	name := "cursor"
	if u, err := url.Parse(base); err == nil && u.Host != "" {
		name += "-" + strings.NewReplacer(":", "_", "/", "_").Replace(u.Host)
	}
	if b := filepath.Base(tokenFile); tokenFile != "" && b != "." && b != "/" {
		name += "-" + b
	}
	return filepath.Join(stateDir(), name)
}

func readCursor(p string) int64 {
	b, err := os.ReadFile(p)
	if err != nil {
		return 0
	}
	n, _ := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	return n
}

func writeCursor(p string, n int64) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	return writeFile0600(p, []byte(strconv.FormatInt(n, 10)+"\n"))
}

// formatLine is one message on one line; newlines in the text become "\n".
func formatLine(m Message) string {
	t := strings.NewReplacer("\r", `\r`, "\n", `\n`).Replace(m.Text)
	re := ""
	if m.ReplyTo != 0 {
		re = fmt.Sprintf(" (re #%d)", m.ReplyTo)
	}
	if m.Kind != "" { // [log] [status] [digest] [reminder]; plain lines unchanged
		re += " [" + m.Kind + "]"
	}
	return fmt.Sprintf("[a2] #%d [%s] %s %s -> %s%s%s: %s", m.ID, channelLabel(m.Channel), m.TS, m.From, m.To, tagSuffix(m), re, t)
}

// logLineMax is the text length (runes) of a "[log]" line in a2 watch.
const logLineMax = 80

// watchLine is formatLine, except a log record (send --log-only) is one short line with
// a "[log]" prefix so a receiver can skip it (a2 inbox shows the full text).
func watchLine(m Message) string {
	if m.Kind != kindLog {
		return formatLine(m)
	}
	t := strings.Join(strings.Fields(m.Text), " ")
	if r := []rune(t); len(r) > logLineMax {
		t = string(r[:logLineMax-1]) + "…"
	}
	return fmt.Sprintf("[a2] [log] #%d %s%s: %s", m.ID, m.From, tagSuffix(m), t)
}

// yourChannels asks the server for the caller's channels (GET /members, 3 s); the client
// does not know its own member name or rights, the server does (renames included).
func yourChannels() string {
	c, err := newClient()
	if err != nil {
		return "your channels: unknown (" + err.Error() + "); a2 members shows them"
	}
	c.http.Timeout = 3 * time.Second
	var r struct {
		You      string   `json:"you"`
		Channels []string `json:"your_channels"`
	}
	if err := c.do("GET", "/members", nil, &r); err != nil {
		return "your channels: unknown (server: " + err.Error() + ")"
	}
	return fmt.Sprintf("you are %s; your channels: %s (only these are accepted)", r.You, strings.Join(channelLabels(r.Channels), ", "))
}

func cmdSend(args []string) error {
	fs := flag.NewFlagSet("send", flag.ContinueOnError)
	to := fs.String("to", "", "recipient member or all")
	channel := fs.String("channel", "", "team (🔒 private, work only) or general (# public, no work talk); default: your first channel")
	replyTo := fs.Int64("reply-to", 0, "reply in the thread of this message id (same channel)")
	logOnly := fs.Bool("log-only", false, "a record (hand-off add/done and the like): a2 watch shows it as one short [log] line")
	status := fs.Bool("status", false, "a status line: the server may batch it with your other status lines into one digest (OUTCOME: problem / BLOCK / blocked go out at once)")
	tag := fs.String("tag", "", "comma separated labels for the message (up to 5; lowercase a-z 0-9 and -, 1 to 24 characters), e.g. approval,blog")
	needsReply := fs.Bool("needs-reply", false, "one recipient: if no reply in this thread comes back --within, the server sends one reminder to both of you")
	within := fs.Duration("within", 30*time.Minute, "with --needs-reply: time to wait for the reply (1m to 168h)")
	fs.Usage = func() {
		out := fs.Output()
		fmt.Fprintln(out, "usage: a2 send --to <member|all> [--channel C] [--reply-to ID] [--tag t1,t2] [--log-only | --status | --needs-reply [--within 30m]] <text|->")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(fs.Output(), yourChannels())
			return nil
		}
		return usageError(err.Error())
	}
	if *to == "" || fs.NArg() != 1 {
		return usageError(`send needs --to <member|all> and one text argument ("-" reads stdin)`)
	}
	req := sendReq{Channel: *channel, To: *to, ReplyTo: *replyTo, Tags: splitTags(*tag)}
	if _, err := normalizeTags(req.Tags); err != nil {
		return usageError(err.Error())
	}
	if err := sendKindFlags(&req, *logOnly, *status, *needsReply, *within, withinSet(fs)); err != nil {
		return err
	}
	text := fs.Arg(0)
	if text == "-" {
		b, err := io.ReadAll(io.LimitReader(os.Stdin, maxTextBytes+1))
		if err != nil {
			return err
		}
		text = strings.TrimRight(string(b), "\n")
	}
	if len(text) > maxTextBytes {
		return fmt.Errorf("text is %d bytes, limit %d", len(text), maxTextBytes)
	}
	req.Text = text
	c, err := newClient()
	if err != nil {
		return err
	}
	var m Message
	if err := c.do("POST", "/send", req, &m); err != nil {
		return err
	}
	fmt.Print(sendResult(req, m))
	return nil
}

func withinSet(fs *flag.FlagSet) bool {
	set := false
	fs.Visit(func(f *flag.Flag) { set = set || f.Name == "within" })
	return set
}

// sendKindFlags puts --log-only, --status and --needs-reply/--within into req.
func sendKindFlags(req *sendReq, logOnly, status, needsReply bool, within time.Duration, withinGiven bool) error {
	n := 0
	for _, b := range []bool{logOnly, status, needsReply} {
		if b {
			n++
		}
	}
	switch {
	case n > 1:
		return usageError("use only one of --log-only, --status, --needs-reply")
	case withinGiven && !needsReply:
		return usageError("--within goes with --needs-reply")
	case logOnly:
		req.Kind = kindLog
	case status:
		req.Kind = kindStatus
	case needsReply:
		if within < minNeedsReply || within > maxNeedsReply {
			return usageError("--within must be 1m to 168h")
		}
		if req.To == allTarget {
			return usageError("--needs-reply needs one recipient, not all")
		}
		req.NeedsReplySec = int64(within / time.Second)
	}
	return nil
}

// sendResult is what a2 send prints for the answer m to req (stdout lines; a notice from
// the server, or about an older server that ignored a send-option option, goes to stderr).
func sendResult(req sendReq, m Message) string {
	var b strings.Builder
	if m.Queued != "" {
		due := m.Queued
		if t, err := time.Parse(time.RFC3339, m.Queued); err == nil {
			due = t.In(jst).Format("15:04")
		}
		fmt.Fprintf(&b, "queued status line for %s in %s (goes out in the digest by about %s)\n", m.To, channelLabel(m.Channel), due)
	} else {
		fmt.Fprintf(&b, "sent #%d to %s in %s\n", m.ID, m.To, channelLabel(m.Channel))
	}
	if m.ReplyBy != "" {
		by := m.ReplyBy
		if t, err := time.Parse(time.RFC3339, m.ReplyBy); err == nil {
			by = t.In(jst).Format("15:04")
		}
		fmt.Fprintf(&b, "needs reply by %s (one reminder to both sides if none)\n", by)
	}
	if m.Away != "" { // a recipient is not alive (the message is still stored)
		fmt.Fprintln(&b, m.Away)
	}
	if m.Notice != "" {
		fmt.Fprintln(os.Stderr, "a2: notice:", m.Notice)
	}
	if req.Kind != "" && m.Kind == "" && m.Queued == "" {
		fmt.Fprintln(os.Stderr, "a2: notice: the server is older and ignored --"+map[string]string{kindLog: "log-only", kindStatus: "status"}[req.Kind]+" (sent as a plain message)")
	}
	if len(req.Tags) > 0 && len(m.Tags) == 0 && m.Queued == "" {
		fmt.Fprintln(os.Stderr, "a2: notice: the server is older and ignored --tag (stored without tags)")
	}
	if req.NeedsReplySec > 0 && m.ReplyBy == "" {
		fmt.Fprintln(os.Stderr, "a2: notice: the server is older and ignored --needs-reply (no reminder)")
	}
	return b.String()
}

func cmdInbox(args []string) error {
	fs := flag.NewFlagSet("inbox", flag.ContinueOnError)
	peek := fs.Bool("peek", false, "do not move the read cursor")
	all := fs.Bool("all", false, "show from the first message (with --peek: cursor kept)")
	channel := fs.String("channel", "", "only this channel (own cursor); default: all your channels")
	sinceS := fs.String("since", "", "filter: after message id N, or from a time (RFC3339, or HH:MM today in JST)")
	from := fs.String("from", "", "filter: only messages from this member (or a2-system)")
	thread := fs.Int64("thread", 0, "filter: this message and every reply in its thread")
	sent := fs.Bool("sent", false, "read-only: list the messages you sent yourself (cursor kept)")
	tag := fs.String("tag", "", "filter: only messages with any of these comma separated tags")
	if err := fs.Parse(args); err != nil {
		return usageError(err.Error())
	}
	// filters search the whole inbox and leave the cursor alone (sendcheck.go)
	filtered := *sinceS != "" || *from != "" || *thread != 0 || *sent || *tag != ""
	var sinceID int64
	var sinceTime string
	if *sinceS != "" {
		if n, err := strconv.ParseInt(strings.TrimPrefix(*sinceS, "#"), 10, 64); err == nil && n >= 0 {
			sinceID = n
		} else if t, err := parseSinceTime(*sinceS, time.Now()); err == nil {
			sinceTime = t.Format(time.RFC3339)
		} else {
			return usageError("--since: give a message id, RFC3339 or HH:MM")
		}
	}
	if *thread < 0 {
		return usageError("bad --thread")
	}
	if _, err := parseTagFilter(splitTags(*tag)); err != nil {
		return usageError(err.Error())
	}
	c, err := newClient()
	if err != nil {
		return err
	}
	cp := channelCursor(c.cursor, *channel)
	since := readCursor(cp)
	if *all || filtered {
		since = sinceID
	}
	var r inboxResp
	if filtered {
		q := url.Values{"since": {strconv.FormatInt(since, 10)}}
		for k, v := range map[string]string{"channel": *channel, "since_time": sinceTime, "from": *from} {
			if v != "" {
				q.Set(k, v)
			}
		}
		if *sent {
			q.Set("sent", "1")
		}
		if *tag != "" {
			q.Set("tag", *tag)
		}
		if *thread != 0 {
			q.Set("thread", strconv.FormatInt(*thread, 10))
		}
		var fr struct {
			inboxResp
			Filtered bool `json:"filtered"`
		}
		if err := c.do("GET", "/inbox?"+q.Encode(), nil, &fr); err != nil {
			return err
		}
		// since=<id> is an old parameter; the others need a server that applies them
		if !fr.Filtered && (sinceTime != "" || *from != "" || *thread != 0 || *sent || *tag != "") {
			return errors.New("the server does not support inbox filters yet (needs a newer server)")
		}
		r = fr.inboxResp
	} else if r, err = c.inbox(since, 0, *channel); err != nil {
		return err
	}
	if filtered {
		*peek = true
	}
	if len(r.Messages) == 0 {
		fmt.Println("no new messages")
		return nil
	}
	for _, m := range r.Messages {
		fmt.Println(formatLine(m))
	}
	if !*peek {
		last := r.Messages[len(r.Messages)-1].ID
		if last > readCursor(cp) {
			return writeCursor(cp, last)
		}
	}
	return nil
}

func cmdMembers(args []string) error {
	if len(args) != 0 {
		return usageError("members takes no arguments")
	}
	c, err := newClient()
	if err != nil {
		return err
	}
	var r struct {
		You     string `json:"you"`
		Members []struct {
			Name     string   `json:"name"`
			Channels []string `json:"channels"`
			HasToken bool     `json:"has_token"`
			Alive    *bool    `json:"alive"`     // older servers; absent = never seen
			LastSeen string   `json:"last_seen"` // RFC3339
		} `json:"members"`
	}
	if err := c.do("GET", "/members", nil, &r); err != nil {
		return err
	}
	for _, m := range r.Members {
		mark := ""
		if m.Name == r.You {
			mark = " (you)"
		}
		state := "no token yet"
		if m.HasToken {
			state = "token set"
		}
		if m.Alive != nil {
			live := "not alive"
			if *m.Alive {
				live = "alive"
			}
			if t, err := time.Parse(time.RFC3339, m.LastSeen); err == nil {
				live += " | last " + t.In(jst).Format("15:04")
			}
			state += " | " + live
		}
		chs := ""
		if len(m.Channels) > 0 {
			chs = " | " + strings.Join(channelLabels(m.Channels), " ")
		}
		fmt.Printf("%s%s: %s%s\n", m.Name, mark, state, chs)
	}
	return nil
}

// channelCursor keeps a separate cursor for a one-channel view, so it does not skip
// unread messages of the other channel.
func channelCursor(base, channel string) string {
	if channel == "" {
		return base
	}
	return base + "-" + strings.NewReplacer("/", "_", `\`, "_", ".", "_").Replace(channel)
}

// mimeByExt guesses the MIME of an upload from its extension (the server checks the content).
var mimeByExt = map[string]string{".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".webp": "image/webp",
	".gif": "image/gif", ".pdf": "application/pdf", ".txt": "text/plain", ".log": "text/plain", ".md": "text/markdown"}

func init() {
	for _, e := range []string{".ts", ".tsx", ".js", ".jsx", ".go", ".py", ".sh", ".yaml", ".yml", ".toml", ".csv", ".diff", ".patch"} {
		mimeByExt[e] = "text/plain"
	}
	mimeByExt[".json"] = "application/json"
}

// uploadMIME picks the MIME for a file without --mime: by extension, or text/plain for an
// extension-less file that is valid UTF-8 text. Other unknown extensions need --mime.
func uploadMIME(p string) (string, error) {
	ext := strings.ToLower(filepath.Ext(p))
	if m := mimeByExt[ext]; m != "" {
		return m, nil
	}
	if ext == "" {
		if data, err := os.ReadFile(p); err == nil && len(data) <= maxFileBytes && isText(data) {
			return "text/plain", nil
		}
	}
	return "", errors.New("unknown extension; give --mime")
}

func cmdPut(args []string) error {
	fs := flag.NewFlagSet("put", flag.ContinueOnError)
	channel := fs.String("channel", "", "team or general (required)")
	to := fs.String("to", "all", "member or all, for the announcement message")
	name := fs.String("name", "", "display name (default: the file's base name)")
	mimeT := fs.String("mime", "", "MIME type (default: from the extension)")
	if err := fs.Parse(args); err != nil {
		return usageError(err.Error())
	}
	if *channel == "" || fs.NArg() != 1 {
		return usageError("put needs --channel <team|general> and one file")
	}
	p := fs.Arg(0)
	if *name == "" {
		*name = filepath.Base(p)
	}
	if *mimeT == "" {
		m, err := uploadMIME(p)
		if err != nil {
			return err
		}
		*mimeT = m
	}
	st, err := os.Stat(p)
	if err != nil {
		return err
	}
	if st.Size() > maxFileBytes {
		return fmt.Errorf("file is %d bytes, limit %d", st.Size(), maxFileBytes)
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return err
	}
	c, err := newClient()
	if err != nil {
		return err
	}
	q := url.Values{"channel": {*channel}, "name": {*name}, "to": {*to}}
	req, err := http.NewRequest("POST", c.base+"/files?"+q.Encode(), bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", *mimeT)
	var out struct {
		File FileMeta `json:"file"`
	}
	if err := c.doReq(req, &out); err != nil {
		return err
	}
	fmt.Printf("shared %s id=%s in %s (%d bytes)\n", out.File.Name, out.File.ID, channelLabel(out.File.Channel), out.File.Size)
	return nil
}

func cmdGet(args []string) error {
	fs := flag.NewFlagSet("get", flag.ContinueOnError)
	out := fs.String("out", "", "output path (default: ./<id>-<name>; never overwrites)")
	if err := fs.Parse(args); err != nil {
		return usageError(err.Error())
	}
	if fs.NArg() != 1 || !fileIDRe.MatchString(fs.Arg(0)) {
		return usageError("get needs one file id (24 hex characters)")
	}
	id := fs.Arg(0)
	c, err := newClient()
	if err != nil {
		return err
	}
	req, err := http.NewRequest("GET", c.base+"/files/"+id, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("User-Agent", userAgent())
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxFileBytes+1))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return serverErr(resp, data)
	}
	if *out == "" {
		name := "file"
		if _, params, err := mime.ParseMediaType(resp.Header.Get("Content-Disposition")); err == nil && checkName(params["filename"]) == nil {
			name = params["filename"]
		}
		*out = id + "-" + name
	}
	f, err := os.OpenFile(*out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	fmt.Printf("saved %s (%d bytes, %s)\n", *out, len(data), resp.Header.Get("Content-Type"))
	return nil
}

func cmdFiles(args []string) error {
	fs := flag.NewFlagSet("files", flag.ContinueOnError)
	channel := fs.String("channel", "", "only this channel")
	if err := fs.Parse(args); err != nil {
		return usageError(err.Error())
	}
	c, err := newClient()
	if err != nil {
		return err
	}
	var r struct {
		Files []FileMeta `json:"files"`
	}
	path := "/files"
	if *channel != "" {
		path += "?" + url.Values{"channel": {*channel}}.Encode()
	}
	if err := c.do("GET", path, nil, &r); err != nil {
		return err
	}
	if len(r.Files) == 0 {
		fmt.Println("no files")
	}
	for _, f := range r.Files {
		fmt.Printf("%s [%s] %s %s %s %d bytes %s\n", f.ID, channelLabel(f.Channel), f.TS, f.From, f.MIME, f.Size, f.Name)
	}
	return nil
}

func serverErr(resp *http.Response, data []byte) error {
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(data, &e) == nil && e.Error != "" {
		return fmt.Errorf("server: %d %s", resp.StatusCode, e.Error)
	}
	return fmt.Errorf("server: %s", resp.Status)
}
