package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
)

const (
	lanOrigin   = "https://192.0.2.10:18765"
	localOrigin = "https://localhost:18765"
)

// setupLocal: the LAN origin of the server host plus the loopback origins of 127.0.0.1:18765. The
// test server itself is on 127.0.0.1, so its TCP peer is loopback; the Host header is set
// per request.
func setupLocal(t *testing.T) *viewFixture {
	t.Helper()
	v := setupView(t)
	v.sv.viewOrigin = lanOrigin
	v.sv.viewLocal = viewLocalOrigins("127.0.0.1:18765")
	return v
}

func (v *viewFixture) doHost(t *testing.T, method, path, host, cookie string, hdr map[string]string, body string) (*http.Response, string) {
	t.Helper()
	r, _ := http.NewRequest(method, v.srv.URL+path, strings.NewReader(body))
	r.Host = host
	for k, val := range hdr {
		if val != "" {
			r.Header.Set(k, val)
		}
	}
	if cookie != "" {
		r.AddCookie(&http.Cookie{Name: viewCookie, Value: cookie})
	}
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func (v *viewFixture) loginHost(t *testing.T, host, origin string) (int, string) {
	t.Helper()
	resp, _ := v.doHost(t, "POST", "/view/login", host, "", map[string]string{
		"Origin": origin, "Content-Type": "application/x-www-form-urlencoded", "Sec-Fetch-Site": "same-origin",
	}, url.Values{"token": {v.viewerTok}}.Encode())
	for _, c := range resp.Cookies() {
		if c.Name == viewCookie {
			return resp.StatusCode, c.Value
		}
	}
	return resp.StatusCode, ""
}

// direct runs the handler with a chosen TCP peer (a LAN client cannot be made over the
// loopback test server).
func direct(sv *server, method, path, peer, host string, hdr map[string]string) int {
	r := httptest.NewRequest(method, "https://"+host+path, nil)
	r.RemoteAddr = peer
	r.Host = host
	for k, val := range hdr {
		r.Header.Set(k, val)
	}
	w := httptest.NewRecorder()
	sv.handler().ServeHTTP(w, r)
	return w.Code
}

func TestViewLocalLoopbackOK(t *testing.T) {
	v := setupLocal(t)
	code, c := v.loginHost(t, "localhost:18765", localOrigin)
	if code != http.StatusSeeOther || c == "" {
		t.Fatalf("login on localhost: %d cookie=%q", code, c)
	}
	resp, page := v.doHost(t, "GET", "/view", "localhost:18765", c, nil, "")
	if resp.StatusCode != 200 || !strings.Contains(page, "/view/app.js") {
		t.Fatalf("page on localhost: %d", resp.StatusCode)
	}
	resp, body := v.doHost(t, "GET", "/view/api/messages?since=0", "localhost:18765", c, nil, "")
	if resp.StatusCode != 200 {
		t.Fatalf("messages on localhost: %d %s", resp.StatusCode, body)
	}
	var mr struct {
		CSRF string `json:"csrf"`
	}
	if json.Unmarshal([]byte(body), &mr); mr.CSRF == "" {
		t.Fatal("no csrf in the messages response on localhost")
	}
	csrf := mr.CSRF
	resp, body = v.doHost(t, "POST", "/view/send", "localhost:18765", c, map[string]string{
		"Origin": localOrigin, "Content-Type": "application/x-www-form-urlencoded", "Sec-Fetch-Site": "same-origin",
	}, url.Values{"csrf": {csrf}, "channel": {"team"}, "to": {"all"}, "text": {"from localhost"}}.Encode())
	if resp.StatusCode != 200 {
		t.Fatalf("send on localhost: %d %s", resp.StatusCode, body)
	}
	if resp, _ := v.doHost(t, "GET", "/view", "127.0.0.1:18765", "", nil, ""); resp.StatusCode != 200 {
		t.Fatalf("page on 127.0.0.1: %d", resp.StatusCode)
	}
	// the LAN origin works as before
	if code, c := v.loginHost(t, "192.0.2.10:18765", lanOrigin); code != http.StatusSeeOther || c == "" {
		t.Fatalf("login on the LAN origin: %d", code)
	}
}

func TestViewLocalRefused(t *testing.T) {
	v := setupLocal(t)
	form := url.Values{"token": {v.viewerTok}}.Encode()
	post := func(host, origin string) int {
		resp, _ := v.doHost(t, "POST", "/view/login", host, "", map[string]string{
			"Origin": origin, "Content-Type": "application/x-www-form-urlencoded", "Sec-Fetch-Site": "same-origin"}, form)
		return resp.StatusCode
	}
	for _, c := range []struct{ name, host, origin string }{
		{"localhost page, LAN origin", "localhost:18765", lanOrigin},
		{"LAN page, localhost origin", "192.0.2.10:18765", localOrigin},
		{"127.0.0.1 page, localhost origin", "127.0.0.1:18765", localOrigin},
		{"localhost, other port", "localhost:9999", "https://localhost:9999"},
		{"localhost, foreign origin", "localhost:18765", "https://evil.example"},
	} {
		if got := post(c.host, c.origin); got != http.StatusForbidden {
			t.Errorf("%s: %d, want 403", c.name, got)
		}
	}
	// a LAN peer that sends Host: localhost
	sv := v.sv
	for _, p := range []string{"/view", "/view/app.js"} {
		if got := direct(sv, "GET", p, "198.51.100.50:40000", "localhost:18765", nil); got != http.StatusForbidden {
			t.Errorf("LAN peer, Host localhost, %s: %d, want 403", p, got)
		}
		if got := direct(sv, "GET", p, "198.51.100.50:40000", "127.0.0.1:18765", nil); got != http.StatusForbidden {
			t.Errorf("LAN peer, Host 127.0.0.1, %s: %d, want 403", p, got)
		}
	}
	if got := direct(sv, "GET", "/view/ws", "198.51.100.50:40000", "localhost:18765", map[string]string{"Origin": localOrigin}); got != http.StatusForbidden {
		t.Errorf("LAN peer, ws on localhost: %d, want 403", got)
	}
	if got := direct(sv, "GET", "/view", "198.51.100.50:40000", "192.0.2.10:18765", nil); got != 200 {
		t.Errorf("LAN peer on the LAN origin: %d, want 200", got)
	}
	// loopback view off: localhost is refused as before
	sv.viewLocal = nil
	if resp, _ := v.doHost(t, "GET", "/view", "localhost:18765", "", nil, ""); resp.StatusCode != http.StatusForbidden {
		t.Errorf("localhost with the loopback view off: %d, want 403", resp.StatusCode)
	}
}

// --view-allow admits loopback peers only while the loopback view is on.
func TestViewLocalAllowList(t *testing.T) {
	v := setupLocal(t)
	v.sv.viewAllow = []netip.Prefix{netip.MustParsePrefix("198.51.100.0/24")}
	if resp, _ := v.doHost(t, "GET", "/view", "localhost:18765", "", nil, ""); resp.StatusCode != 200 {
		t.Fatalf("loopback peer with --view-allow %s: %d", "198.51.100.0/24", resp.StatusCode)
	}
	if got := direct(v.sv, "GET", "/view", "10.0.0.5:4000", "192.0.2.10:18765", nil); got != http.StatusForbidden {
		t.Fatalf("peer outside --view-allow: %d", got)
	}
	v.sv.viewLocal = nil
	if resp, _ := v.doHost(t, "GET", "/view", "192.0.2.10:18765", "", nil, ""); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("loopback peer, loopback view off, outside --view-allow: %d", resp.StatusCode)
	}
}

func TestViewLocalWS(t *testing.T) {
	v := setupLocal(t)
	_, c := v.loginHost(t, "localhost:18765", localOrigin)
	ws := func(origin string) int {
		u, _ := url.Parse(v.srv.URL)
		conn, err := net.Dial("tcp", u.Host)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		req, _ := http.NewRequest("GET", v.srv.URL+"/view/ws", nil)
		req.Host = "localhost:18765"
		for k, val := range map[string]string{"Upgrade": "websocket", "Connection": "Upgrade", "Sec-WebSocket-Key": "dGhlIHNhbXBsZSBub25jZQ==",
			"Sec-WebSocket-Version": "13", "Cookie": viewCookie + "=" + c, "Origin": origin, "Sec-Fetch-Site": "same-origin"} {
			req.Header.Set(k, val)
		}
		req.Write(conn)
		resp, err := http.ReadResponse(bufio.NewReader(conn), req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if got := ws(localOrigin); got != http.StatusSwitchingProtocols {
		t.Fatalf("ws on localhost from loopback: %d, want 101", got)
	}
	if got := ws(lanOrigin); got != http.StatusForbidden {
		t.Fatalf("ws on localhost with the LAN origin: %d, want 403", got)
	}
}

// Agent routes keep refusing a browser Origin, the loopback one included, and the loopback
// listener serves /view only.
func TestViewLocalAgentAPI(t *testing.T) {
	v := setupLocal(t)
	r, _ := http.NewRequest("GET", v.srv.URL+"/members", nil)
	r.Host = "localhost:18765"
	r.Header.Set("Authorization", "Bearer "+v.tokens["alice"])
	r.Header.Set("Origin", localOrigin)
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("agent route with the localhost Origin: %d, want 403", resp.StatusCode)
	}

	ts := httptest.NewUnstartedServer(v.sv.handler())
	ts.Config.ConnContext = viewLocalConnContext(ts.Listener.Addr())
	ts.Start()
	defer ts.Close()
	for p, want := range map[string]int{"/members": 404, "/healthz": 404, "/inbox": 404, "/view": 403, "/view/app.css": 403} {
		// 403 on /view*: the Host is the test address, not a view origin; the route itself is served
		resp, err := http.Get(ts.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("loopback listener %s: %d, want %d", p, resp.StatusCode, want)
		}
	}
	r = httptest.NewRequest("GET", "https://localhost:18765/view", nil)
	r.RemoteAddr = "127.0.0.1:5000"
	r = r.WithContext(context.WithValue(r.Context(), viewLocalKey{}, true))
	w := httptest.NewRecorder()
	v.sv.handler().ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("/view on the loopback listener: %d", w.Code)
	}
}

func TestViewLocalAddr(t *testing.T) {
	for _, c := range []struct{ flag, want string }{
		{"auto", "127.0.0.1:18765"}, {"off", ""}, {"", ""}, {"127.0.0.1:20000", "127.0.0.1:20000"}, {"[::1]:18765", "[::1]:18765"},
	} {
		got, err := viewLocalAddr(c.flag, "192.0.2.10:18765")
		if err != nil || got != c.want {
			t.Errorf("%q: %q %v, want %q", c.flag, got, err, c.want)
		}
	}
	for _, bad := range []string{"192.0.2.10:18765", "0.0.0.0:18765", "localhost:18765", "nonsense"} {
		if _, err := viewLocalAddr(bad, "192.0.2.10:18765"); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if got := viewLocalOrigins("127.0.0.1:18765"); len(got) != 2 || got[0] != localOrigin || got[1] != "https://127.0.0.1:18765" {
		t.Errorf("origins %v", got)
	}
	if got := viewLocalOrigins("[::1]:18765"); len(got) != 2 || got[1] != "https://[::1]:18765" {
		t.Errorf("origins %v", got)
	}
}
