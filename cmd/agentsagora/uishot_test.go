//go:build uishot

package main

// Screenshot check of the viewer UI in a real browser (local headless Chrome over the
// DevTools protocol). Not part of the normal test run:
//
//	UISHOT_DIR=/tmp/shots go test -tags uishot -run TestUIShot ./cmd/agentsagora
//
// It uses only a temporary state dir (TestMain), a test TLS server on 127.0.0.1 and a
// temporary Chrome profile; it fails on any uncaught script exception.

import (
	"bufio"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

type wsConn struct {
	c  net.Conn
	br *bufio.Reader
}

func wsDial(raw string) (*wsConn, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	c, err := net.Dial("tcp", u.Host)
	if err != nil {
		return nil, err
	}
	k := make([]byte, 16)
	rand.Read(k)
	key := base64.StdEncoding.EncodeToString(k)
	fmt.Fprintf(c, "GET %s HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n\r\n", u.RequestURI(), u.Host, key)
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		return nil, err
	}
	sum := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	if resp.StatusCode != 101 || resp.Header.Get("Sec-WebSocket-Accept") != base64.StdEncoding.EncodeToString(sum[:]) {
		return nil, fmt.Errorf("websocket handshake: %s", resp.Status)
	}
	return &wsConn{c: c, br: br}, nil
}

func (w *wsConn) send(p []byte) error {
	h := []byte{0x81}
	switch n := len(p); {
	case n < 126:
		h = append(h, 0x80|byte(n))
	case n < 1<<16:
		h = append(h, 0x80|126, byte(n>>8), byte(n))
	default:
		h = append(h, 0x80|127)
		h = binary.BigEndian.AppendUint64(h, uint64(n))
	}
	m := make([]byte, 4)
	rand.Read(m)
	h = append(h, m...)
	b := make([]byte, len(p))
	for i := range p {
		b[i] = p[i] ^ m[i%4]
	}
	_, err := w.c.Write(append(h, b...))
	return err
}

func (w *wsConn) recv() ([]byte, error) {
	var out []byte
	for {
		h := make([]byte, 2)
		if _, err := io.ReadFull(w.br, h); err != nil {
			return nil, err
		}
		n := uint64(h[1] & 0x7f)
		switch n {
		case 126:
			b := make([]byte, 2)
			io.ReadFull(w.br, b)
			n = uint64(binary.BigEndian.Uint16(b))
		case 127:
			b := make([]byte, 8)
			io.ReadFull(w.br, b)
			n = binary.BigEndian.Uint64(b)
		}
		p := make([]byte, n)
		if _, err := io.ReadFull(w.br, p); err != nil {
			return nil, err
		}
		if op := h[0] & 0x0f; op == 0x9 || op == 0xA {
			continue
		}
		out = append(out, p...)
		if h[0]&0x80 != 0 {
			return out, nil
		}
	}
}

type cdp struct {
	t      *testing.T
	ws     *wsConn
	id     int
	errors []string
}

func (c *cdp) call(method string, params any) map[string]any {
	c.t.Helper()
	c.id++
	b, _ := json.Marshal(map[string]any{"id": c.id, "method": method, "params": params})
	if err := c.ws.send(b); err != nil {
		c.t.Fatal(err)
	}
	for {
		raw, err := c.ws.recv()
		if err != nil {
			c.t.Fatal(err)
		}
		var m map[string]any
		json.Unmarshal(raw, &m)
		if m["method"] == "Runtime.exceptionThrown" {
			c.errors = append(c.errors, string(raw))
		}
		if m["method"] == "Runtime.consoleAPICalled" && strings.Contains(string(raw), `"type":"error"`) {
			c.errors = append(c.errors, string(raw))
		}
		if id, ok := m["id"].(float64); ok && int(id) == c.id {
			if m["error"] != nil {
				c.t.Fatalf("%s: %v", method, m["error"])
			}
			r, _ := m["result"].(map[string]any)
			return r
		}
	}
}

func (c *cdp) eval(js string) any {
	c.t.Helper()
	r := c.call("Runtime.evaluate", map[string]any{"expression": js, "returnByValue": true})
	if r["exceptionDetails"] != nil {
		c.t.Fatalf("eval %q: %v", js, r["exceptionDetails"])
	}
	res, _ := r["result"].(map[string]any)
	return res["value"]
}

func (c *cdp) wait(d time.Duration) {
	c.t.Helper()
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		c.eval("1")
		time.Sleep(100 * time.Millisecond)
	}
}

func (c *cdp) shot(dir, name string) {
	c.t.Helper()
	r := c.call("Page.captureScreenshot", map[string]any{"format": "png"})
	b, _ := base64.StdEncoding.DecodeString(r["data"].(string))
	if err := os.WriteFile(filepath.Join(dir, name+".png"), b, 0o600); err != nil {
		c.t.Fatal(err)
	}
}

func TestUIShot(t *testing.T) {
	dir := os.Getenv("UISHOT_DIR")
	if dir == "" {
		t.Skip("UISHOT_DIR not set")
	}
	os.MkdirAll(dir, 0o700)
	v := setupView(t)
	ts := httptest.NewUnstartedServer(v.sv.handler())
	ts.EnableHTTP2 = true // as ServeTLS does in production
	ts.StartTLS()
	defer ts.Close()
	origin := "https://" + ts.Listener.Addr().String()
	v.sv.viewOrigin = origin

	// Traffic over two days: channel talk, DMs, a thread, a file, the viewer's own message.
	base := time.Now().Add(-26 * time.Hour)
	at := func(min int) time.Time { return base.Add(time.Duration(min) * time.Minute) }
	st := v.sv.store
	add := func(min int, ch, from, to, text string, re int64) int64 {
		m, err := st.appendReply(ch, from, to, text, "", re, at(min))
		if err != nil {
			t.Fatal(err)
		}
		return m.ID
	}
	add(0, "team", "alice", "all", "Morning. Build 4412 is green on the runner.", 0)
	add(1, "team", "alice", "all", "Starting the nightly cleanup now.", 0)
	root := add(3, "team", "bob", "all", "Can someone check why the disk alert fired at 03:10?", 0)
	add(5, "team", "alice", "all", "Looking. It was the log rotation, 91% for two minutes.", root)
	add(7, "team", "bob", "all", "Thanks, closing it.", root)
	add(9, "team", "dave", "all", "Windows test pass finished: 212 passed, 0 failed.", 0)
	add(12, "team", "carol", "bob", "Your review queue has 3 items.", 0)
	add(14, "team", "bob", "carol", "On it after lunch.", 0)
	add(30, "general", "erin", "all", "Second opinion: the loop in the sample is fine.\nOne nit: name the timeout.", 0)
	add(31, "general", "frank", "all", "Generated 4 cover sketches.", 0)
	g := add(33, "general", "frank", "all", "Photos moved, 1.2 GB.", 0)
	add(35, "general", "erin", "all", "Nice.", g)
	// Markdown in a body
	add(36, "general", "erin", "all", "## Review\n- **one** item\n  - nested `code`\n- two\n\n| case | result |\n|---|---|\n| a | ok |\n\n```\n<b>raw</b>\n```\n> quoted\nsee [docs](https://example.com/x)", 0)
	add(26*60-20, "team", viewerName, "all", "Please keep work talk here.", 0)
	add(26*60-10, "team", "alice", "all", "Noted.", 0)
	add(26*60-5, "general", "frank", "erin", "Can you look at sketch 3?", 0)
	if _, _, err := v.sv.putFile("dave", "team", "all", "result.txt", "text/plain", []byte("ok\n")); err != nil {
		t.Fatal(err)
	}
	// a vote with counts under its message
	if _, _, err := v.sv.voteNew("alice", "team", "Build the inbox search next?", nil, 0); err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"alice", "dave"} {
		if _, _, err := v.sv.voteCast(w, 1, "yes"); err != nil {
			t.Fatal(err)
		}
	}

	cl := ts.Client()
	cl.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	req, _ := http.NewRequest("POST", ts.URL+"/view/login", strings.NewReader(url.Values{"token": {v.viewerTok}}.Encode()))
	req.Header.Set("Origin", origin)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := cl.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	cookie := ""
	for _, c := range resp.Cookies() {
		if c.Name == viewCookie {
			cookie = c.Value
		}
	}
	if cookie == "" {
		t.Fatal("no session")
	}

	prof, err := os.MkdirTemp("", "uishot-chrome-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(prof)
	cmd := exec.Command("google-chrome", "--headless=new", "--remote-debugging-port=0", "--user-data-dir="+prof,
		"--ignore-certificate-errors", "--no-first-run", "--no-default-browser-check", "--disable-gpu",
		"--window-size=1280,820", "--lang=en-US", "about:blank")
	stderr, _ := cmd.StderrPipe()
	if err := cmd.Start(); err != nil {
		t.Skip("no chrome: ", err)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait(); os.RemoveAll(prof) }()
	sc := bufio.NewScanner(stderr)
	re := regexp.MustCompile(`DevTools listening on (ws://\S+)`)
	var wsURL string
	for sc.Scan() {
		if m := re.FindStringSubmatch(sc.Text()); m != nil {
			wsURL = m[1]
			break
		}
	}
	go io.Copy(io.Discard, stderr)
	u, _ := url.Parse(wsURL)
	var targets []map[string]any
	for i := 0; i < 50 && len(targets) == 0; i++ {
		r, err := http.Get("http://" + u.Host + "/json/list")
		if err == nil {
			json.NewDecoder(r.Body).Decode(&targets)
			r.Body.Close()
		}
		var pages []map[string]any
		for _, tg := range targets {
			if tg["type"] == "page" {
				pages = append(pages, tg)
			}
		}
		targets = pages
		time.Sleep(100 * time.Millisecond)
	}
	if len(targets) == 0 {
		t.Fatal("no page target")
	}
	ws, err := wsDial(targets[0]["webSocketDebuggerUrl"].(string))
	if err != nil {
		t.Fatal(err)
	}
	c := &cdp{t: t, ws: ws}
	c.call("Runtime.enable", map[string]any{})
	c.call("Page.enable", map[string]any{})
	c.call("Network.setCookie", map[string]any{"name": viewCookie, "value": cookie, "url": ts.URL + "/",
		"secure": true, "httpOnly": true, "sameSite": "Strict", "path": "/"})
	c.call("Emulation.setDeviceMetricsOverride", map[string]any{"width": 1280, "height": 820, "deviceScaleFactor": 1, "mobile": false})
	c.call("Emulation.setEmulatedMedia", map[string]any{"features": []map[string]string{{"name": "prefers-color-scheme", "value": "light"}}})
	c.call("Page.navigate", map[string]any{"url": ts.URL + "/view"})
	c.wait(1500 * time.Millisecond)
	if n, _ := c.eval(`document.querySelectorAll("#list .msg").length`).(float64); n < 5 {
		t.Fatalf("work pane shows %v messages", n)
	}
	// live updates over /view/ws: the status says live, and a new message shows up well
	// before the once-a-minute poll would fetch it
	if s, _ := c.eval(`document.getElementById("status").textContent`).(string); !strings.Contains(s, "live") {
		t.Fatalf("status %q, want live (WebSocket not up)", s)
	}
	if _, err := v.sv.sendOn("alice", chTeam, allTarget, "Pushed over the socket", 0); err != nil {
		t.Fatal(err)
	}
	c.wait(1000 * time.Millisecond)
	if s, _ := c.eval(`document.getElementById("list").textContent`).(string); !strings.Contains(s, "Pushed over the socket") {
		t.Fatal("pushed message not shown within 1 s")
	}
	c.shot(dir, "1-desktop-team")
	c.eval(`document.querySelector("#list .replies").click()`)
	c.wait(400 * time.Millisecond)
	if n, _ := c.eval(`document.querySelectorAll("#th-list .msg").length`).(float64); n != 3 {
		t.Fatalf("thread shows %v messages, want 3", n)
	}
	c.shot(dir, "2-desktop-thread")
	c.eval(`document.querySelectorAll("#dms .side-item")[1].click()`)
	c.wait(400 * time.Millisecond)
	c.shot(dir, "3-desktop-dm")
	c.call("Emulation.setEmulatedMedia", map[string]any{"features": []map[string]string{{"name": "prefers-color-scheme", "value": "dark"}}})
	c.eval(`document.querySelectorAll("#chans .side-item")[1].click()`)
	c.wait(400 * time.Millisecond)
	c.shot(dir, "4-desktop-general-dark")
	if n, _ := c.eval(`document.querySelectorAll("#list .md-table, #list .md-code, #list .md-li, #list .md-h, #list .md-q").length`).(float64); n < 6 {
		t.Fatalf("markdown blocks: %v", n)
	}
	if r, _ := c.eval(`(document.querySelector("#list .md a")||{}).rel`).(string); r != "noopener noreferrer" {
		t.Fatalf("markdown link rel %q", r)
	}
	if n, _ := c.eval(`document.querySelectorAll("#list .md b").length`).(float64); n != 0 {
		t.Fatal("raw <b> in a code block became markup")
	}
	c.eval(`document.getElementById("rp-btn").click()`)
	c.eval(`(function(){var s=document.getElementById("rp-speed");s.value="600";s.dispatchEvent(new Event("change"));document.getElementById("rp-play").click();})()`)
	c.wait(2500 * time.Millisecond)
	c.shot(dir, "5-replay")
	if txt, _ := c.eval(`document.getElementById("rp-clock").textContent`).(string); !strings.Contains(txt, "JST") {
		t.Fatalf("replay clock %q", txt)
	}
	c.eval(`document.getElementById("rp-exit").click()`)
	c.call("Emulation.setEmulatedMedia", map[string]any{"features": []map[string]string{{"name": "prefers-color-scheme", "value": "light"}}})
	// observer UI: search, health, a link to one message, keys
	key := func(k string) {
		c.eval(`document.body.dispatchEvent(new KeyboardEvent("keydown", {key: "` + k + `", bubbles: true}))`)
	}
	key("/")
	if a, _ := c.eval(`document.activeElement.id`).(string); a != "q" {
		t.Fatalf("/ focused %q", a)
	}
	c.eval(`(function(){var q=document.getElementById("q");q.value="from:alice noted";q.dispatchEvent(new KeyboardEvent("keydown",{key:"Enter",bubbles:true}));})()`)
	c.wait(300 * time.Millisecond)
	if n, _ := c.eval(`document.querySelectorAll("#list .msg").length`).(float64); n != 1 {
		t.Fatalf("search from:alice noted shows %v", n)
	}
	c.eval(`(function(){var q=document.getElementById("q");q.value="in:general is:thread";q.dispatchEvent(new KeyboardEvent("keydown",{key:"Enter",bubbles:true}));})()`)
	c.wait(300 * time.Millisecond)
	c.shot(dir, "9-search")
	if n, _ := c.eval(`document.querySelectorAll("#list .msg").length`).(float64); n != 1 {
		t.Fatalf("search in:general is:thread shows %v", n)
	}
	key("Escape")
	c.wait(200 * time.Millisecond)
	if q, _ := c.eval(`document.getElementById("q").value`).(string); q != "" {
		t.Fatalf("Escape left the search %q", q)
	}
	key("h")
	c.wait(300 * time.Millisecond)
	c.shot(dir, "10-health")
	if n, _ := c.eval(`document.querySelectorAll("#h-members tbody tr").length`).(float64); n < 3 {
		t.Fatalf("health members rows %v", n)
	}
	if n, _ := c.eval(`document.querySelectorAll("#h-matrix td.cell:not(.zero)").length`).(float64); n < 3 {
		t.Fatalf("health matrix cells %v", n)
	}
	key("h")
	c.eval(`location.hash = "#m=` + fmt.Sprint(root) + `"`)
	c.wait(500 * time.Millisecond)
	c.shot(dir, "11-link")
	if h, _ := c.eval(`(document.querySelector("#list li.msg.hl")||{}).getAttribute ? document.querySelector("#list li.msg.hl").getAttribute("data-id") : ""`).(string); h != fmt.Sprint(root) {
		t.Fatalf("link #m=%d highlighted %q", root, h)
	}
	key("j")
	if n, _ := c.eval(`document.querySelectorAll("#list li.msg.sel").length`).(float64); n != 1 {
		t.Fatalf("j selected %v", n)
	}
	key("?")
	if h, _ := c.eval(`document.getElementById("help").hidden`).(bool); h {
		t.Fatal("? did not open the help")
	}
	c.eval(`document.getElementById("help-close").dispatchEvent(new KeyboardEvent("keydown", {key: "Escape", bubbles: true}))`)
	if h, _ := c.eval(`document.getElementById("help").hidden`).(bool); !h {
		t.Fatal("Escape did not close the help")
	}
	c.eval(`(function(){var t=document.getElementById("s-text");t.focus();t.dispatchEvent(new KeyboardEvent("keydown",{key:"j",bubbles:true}));})()`)
	if n, _ := c.eval(`document.querySelectorAll("#list li.msg.sel").length`).(float64); n != 1 {
		t.Fatal("j in the composer moved the selection")
	}
	c.eval(`document.getElementById("s-text").blur(); location.hash = ""`)
	c.call("Emulation.setDeviceMetricsOverride", map[string]any{"width": 390, "height": 844, "deviceScaleFactor": 2, "mobile": true})
	c.call("Page.reload", map[string]any{})
	c.wait(1500 * time.Millisecond)
	c.shot(dir, "6-phone-list")
	c.eval(`document.querySelectorAll("#chans .side-item")[0].click()`)
	c.wait(400 * time.Millisecond)
	c.shot(dir, "7-phone-chat")
	c.eval(`document.getElementById("h-btn").click()`)
	c.wait(300 * time.Millisecond)
	c.shot(dir, "7b-phone-health")
	if w, _ := c.eval(`document.documentElement.scrollWidth`).(float64); w > 390 {
		t.Fatalf("phone page is %v px wide", w)
	}
	c.eval(`document.getElementById("h-btn").click()`)
	c.wait(300 * time.Millisecond)
	// Write from the phone into the thread and check it lands as a reply.
	c.eval(`document.querySelector("#list .replies").click()`)
	c.wait(300 * time.Millisecond)
	c.eval(`(function(){var t=document.getElementById("t-text");t.value="Reply from the viewer";document.getElementById("th-send").requestSubmit();})()`)
	c.wait(1500 * time.Millisecond)
	c.shot(dir, "8-phone-thread")
	found := false
	for _, m := range st.after(0) {
		if m.From == viewerName && m.Text == "Reply from the viewer" && m.ReplyTo == root {
			found = true
		}
	}
	if !found {
		t.Fatalf("thread reply from the page not stored (status %v)", c.eval(`document.getElementById("t-status").textContent`))
	}
	if len(c.errors) > 0 {
		t.Fatalf("script errors: %v", c.errors)
	}
}
