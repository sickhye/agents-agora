// Command screenshots retakes the PNGs in docs/images with headless Chrome, from the demo
// server (go run ./demo). Standard library only; it speaks the DevTools protocol itself.
//
//	go run ./demo -port 18795                  # prints the viewer token; leave it running
//	go run ./demo/screenshots -url https://127.0.0.1:18795 -token TOKEN -out docs/images
//
// Name shots after the flags to retake only those (go run ./demo/screenshots ... health keys).
// It talks only to a server on 127.0.0.1 / localhost (the demo's CA is private, so the
// certificate check is off for that one connection) and to a Chrome it starts with a
// temporary profile. Set CHROME to the browser binary if it is not "google-chrome".
package main

import (
	"bufio"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// ---- minimal WebSocket client (enough for the DevTools protocol) ----

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

// ---- DevTools calls ----

type cdp struct {
	ws *wsConn
	id int
}

func (c *cdp) call(method string, params any) map[string]any {
	c.id++
	b, _ := json.Marshal(map[string]any{"id": c.id, "method": method, "params": params})
	if err := c.ws.send(b); err != nil {
		log.Fatal(err)
	}
	for {
		raw, err := c.ws.recv()
		if err != nil {
			log.Fatal(err)
		}
		var m map[string]any
		json.Unmarshal(raw, &m)
		if id, ok := m["id"].(float64); ok && int(id) == c.id {
			if m["error"] != nil {
				log.Fatalf("%s: %v", method, m["error"])
			}
			r, _ := m["result"].(map[string]any)
			return r
		}
	}
}

func (c *cdp) eval(js string) {
	r := c.call("Runtime.evaluate", map[string]any{"expression": js, "returnByValue": true})
	if r["exceptionDetails"] != nil {
		log.Fatalf("eval %q: %v", js, r["exceptionDetails"])
	}
}

// wait keeps talking to the page (so events are drained) for d.
func (c *cdp) wait(d time.Duration) {
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		c.eval("1")
		time.Sleep(100 * time.Millisecond)
	}
}

// ---- shots ----

func click(sel string) string {
	b, _ := json.Marshal(sel)
	return `document.querySelector(` + string(b) + `).click()`
}
func key(k string) string {
	b, _ := json.Marshal(k)
	return `document.body.dispatchEvent(new KeyboardEvent("keydown", {key: ` + string(b) + `, bubbles: true}))`
}
func search(q string) string {
	b, _ := json.Marshal(q)
	return `(function(){var q=document.getElementById("q");q.value=` + string(b) + `;q.dispatchEvent(new KeyboardEvent("keydown",{key:"Enter",bubbles:true}));})()`
}

type shot struct {
	name          string
	steps         []string // JS run in order after the page loaded
	width, height int      // 0 = desktop 1280x800
	mobile        bool
	themes        []string // nil = light and dark
}

var shots = []shot{
	{name: "team", steps: []string{`document.querySelector("#list li.msg[data-id=\"13\"]").scrollIntoView({block: "start"})`}},
	{name: "thread", steps: []string{click("#list .replies")}},
	{name: "dm", steps: []string{click("#dms .side-item")}},
	{name: "general", steps: []string{`document.querySelectorAll("#chans .side-item")[1].click()`}},
	{name: "search", steps: []string{search("from:alice is:thread")}},
	{name: "health", steps: []string{key("h")}},
	{name: "replay", steps: []string{click("#rp-btn"), `(function(){var s=document.getElementById("rp-pos");s.value="1000";s.dispatchEvent(new Event("input",{bubbles:true}));})()`}},
	{name: "keys", steps: []string{`document.querySelector("#list li.msg[data-id=\"13\"]").scrollIntoView({block: "start"})`, key("?")}},
	{name: "mobile-list", width: 390, height: 844, mobile: true, themes: []string{"light"}, steps: []string{`var x = document.getElementById("tl-x"); if (x) x.click()`}},
	{name: "mobile-chat", width: 390, height: 844, mobile: true, themes: []string{"light"}, steps: []string{click("#chans .side-item")}},
	{name: "mobile-thread", width: 390, height: 844, mobile: true, themes: []string{"light"}, steps: []string{click("#chans .side-item"), click("#list .replies")}},
	{name: "mobile-dark", width: 390, height: 844, mobile: true, themes: []string{"dark"}, steps: []string{click("#chans .side-item")}},
}

func main() {
	base := flag.String("url", "https://127.0.0.1:18795", "demo server (loopback only)")
	token := flag.String("token", "", "viewer token printed by go run ./demo")
	out := flag.String("out", "docs/images", "output directory")
	scale := flag.Float64("scale", 2, "device scale factor")
	flag.Parse()
	only := flag.Args()
	u, err := url.Parse(*base)
	if err != nil || u.Scheme != "https" || (u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost") {
		log.Fatal("-url must be https://127.0.0.1:PORT (this tool only talks to the local demo server)")
	}
	if *token == "" {
		log.Fatal("-token is required")
	}
	origin := u.Scheme + "://" + u.Host
	os.MkdirAll(*out, 0o755)

	// Log in once with the viewer token and keep the session cookie.
	cl := &http.Client{
		Transport:     &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}, // loopback demo CA
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	req, _ := http.NewRequest("POST", origin+"/view/login", strings.NewReader(url.Values{"token": {*token}}.Encode()))
	req.Header.Set("Origin", origin)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := cl.Do(req)
	if err != nil {
		log.Fatal(err)
	}
	resp.Body.Close()
	var cookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Value != "" {
			cookie = c
		}
	}
	if cookie == nil {
		log.Fatalf("login failed (HTTP %d)", resp.StatusCode)
	}

	chrome := os.Getenv("CHROME")
	if chrome == "" {
		chrome = "google-chrome"
	}
	prof, err := os.MkdirTemp("", "a2-shots-")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(prof)
	cmd := exec.Command(chrome, "--headless=new", "--remote-debugging-port=0", "--user-data-dir="+prof,
		"--ignore-certificate-errors", "--no-first-run", "--no-default-browser-check", "--disable-gpu",
		"--hide-scrollbars", "--window-size=1280,820", "--lang=en-US", "about:blank")
	stderr, _ := cmd.StderrPipe()
	if err := cmd.Start(); err != nil {
		log.Fatal("cannot start Chrome (set CHROME): ", err)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()
	var wsURL string
	sc := bufio.NewScanner(stderr)
	re := regexp.MustCompile(`DevTools listening on (ws://\S+)`)
	for sc.Scan() {
		if m := re.FindStringSubmatch(sc.Text()); m != nil {
			wsURL = m[1]
			break
		}
	}
	go io.Copy(io.Discard, stderr)
	du, err := url.Parse(wsURL)
	if err != nil || wsURL == "" {
		log.Fatal("no DevTools address from Chrome")
	}
	var page string
	for i := 0; i < 50 && page == ""; i++ {
		if r, err := http.Get("http://" + du.Host + "/json/list"); err == nil {
			var ts []map[string]any
			json.NewDecoder(r.Body).Decode(&ts)
			r.Body.Close()
			for _, t := range ts {
				if t["type"] == "page" {
					page, _ = t["webSocketDebuggerUrl"].(string)
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	ws, err := wsDial(page)
	if err != nil {
		log.Fatal(err)
	}
	c := &cdp{ws: ws}
	c.call("Page.enable", map[string]any{})
	c.call("Network.enable", map[string]any{})
	c.call("Network.setCookie", map[string]any{"name": cookie.Name, "value": cookie.Value, "url": origin + "/",
		"secure": true, "httpOnly": true, "sameSite": "Strict", "path": "/"})

	for _, s := range shots {
		if len(only) > 0 && !slices.Contains(only, s.name) {
			continue
		}
		themes := s.themes
		if themes == nil {
			themes = []string{"light", "dark"}
		}
		for _, theme := range themes {
			w, h := s.width, s.height
			if w == 0 {
				w, h = 1280, 800
			}
			c.call("Emulation.setDeviceMetricsOverride", map[string]any{"width": w, "height": h, "deviceScaleFactor": *scale, "mobile": s.mobile})
			c.call("Emulation.setEmulatedMedia", map[string]any{"features": []map[string]string{{"name": "prefers-color-scheme", "value": theme}}})
			c.call("Page.navigate", map[string]any{"url": origin + "/view"})
			c.wait(800 * time.Millisecond)
			c.eval(`localStorage.clear(); localStorage.setItem("a2-view-theme", "` + theme + `"); 1`)
			c.call("Page.reload", map[string]any{})
			c.wait(2200 * time.Millisecond)
			for _, step := range s.steps {
				c.eval(step)
				c.wait(500 * time.Millisecond)
			}
			c.wait(400 * time.Millisecond)
			r := c.call("Page.captureScreenshot", map[string]any{"format": "png"})
			b, _ := base64.StdEncoding.DecodeString(r["data"].(string))
			name := s.name + "-" + theme + ".png"
			if s.themes != nil && len(s.themes) == 1 && theme == "light" {
				name = s.name + ".png"
			}
			file := filepath.Join(*out, name)
			if err := os.WriteFile(file, b, 0o644); err != nil {
				log.Fatal(err)
			}
			fmt.Println(file)
		}
	}
}
