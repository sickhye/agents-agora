package main

import (
	"crypto/x509"
	"encoding/json"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

type viewFixture struct {
	*fixture
	viewerTok string
	origin    string
}

func setupView(t *testing.T) *viewFixture {
	t.Helper()
	f := setup(t)
	p, err := tokenNew(viewerName, "", false)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := readToken(p)
	if err != nil {
		t.Fatal(err)
	}
	origin := "https://" + f.srv.Listener.Addr().String()
	f.sv.viewOrigin = origin
	return &viewFixture{fixture: f, viewerTok: tok, origin: origin}
}

// do sends a request; hdr values "" are skipped. The Secure cookie is set by hand because
// the test server is plain HTTP.
func (v *viewFixture) do(t *testing.T, method, path, cookie string, hdr map[string]string, body string) (*http.Response, string) {
	t.Helper()
	r, _ := http.NewRequest(method, v.srv.URL+path, strings.NewReader(body))
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

func (v *viewFixture) login(t *testing.T, tok, oldCookie string) (*http.Response, string) {
	t.Helper()
	resp, _ := v.do(t, "POST", "/view/login", oldCookie, map[string]string{
		"Origin": v.origin, "Content-Type": "application/x-www-form-urlencoded", "Sec-Fetch-Site": "same-origin",
	}, url.Values{"token": {tok}}.Encode())
	val := ""
	for _, c := range resp.Cookies() {
		if c.Name == viewCookie {
			val = c.Value
		}
	}
	return resp, val
}

func (v *viewFixture) messages(t *testing.T, cookie string) (int, []map[string]any) {
	t.Helper()
	resp, body := v.do(t, "GET", "/view/api/messages?since=0", cookie, nil, "")
	var out struct {
		Messages []map[string]any `json:"messages"`
	}
	json.Unmarshal([]byte(body), &out)
	return resp.StatusCode, out.Messages
}

func TestViewAgentTokenRejected(t *testing.T) {
	v := setupView(t)
	for _, m := range DefaultMembers {
		if resp, c := v.login(t, v.tokens[m], ""); resp.StatusCode != 401 || c != "" {
			t.Fatalf("login with %s token: %d cookie=%q, want 401 and no cookie", m, resp.StatusCode, c)
		}
		v.sv.view.ok("127.0.0.1") // keep the lockout out of this test
	}
	resp, _ := v.do(t, "GET", "/view/api/messages", "", map[string]string{"Authorization": "Bearer " + v.tokens["alice"]}, "")
	if resp.StatusCode != 401 {
		t.Fatalf("agent bearer on view API: %d, want 401", resp.StatusCode)
	}
	resp, body := v.do(t, "GET", "/view", "", map[string]string{"Authorization": "Bearer " + v.tokens["alice"]}, "")
	if resp.StatusCode != 200 || !strings.Contains(body, `action="/view/login"`) || strings.Contains(body, "app.js") {
		t.Fatalf("agent bearer on /view must get the login page: %d", resp.StatusCode)
	}
	if code, _ := v.messages(t, "forged-cookie"); code != 401 {
		t.Fatalf("forged cookie: %d", code)
	}
}

func TestViewerIsNotAMember(t *testing.T) {
	v := setupView(t)
	// viewer token cannot use any agent route.
	for _, p := range []struct{ m, path string }{{"POST", "/send"}, {"GET", "/inbox"}, {"GET", "/members"}, {"POST", "/mcp"}, {"GET", "/files"}} {
		code, _ := v.req(t, p.m, p.path, v.viewerTok, map[string]string{"channel": "team", "to": "all", "text": "x"})
		if code != 401 {
			t.Fatalf("viewer token on %s %s: %d, want 401", p.m, p.path, code)
		}
	}
	code, out := v.req(t, "GET", "/members", v.tokens["alice"], nil)
	b, _ := json.Marshal(out)
	if code != 200 || strings.Contains(string(b), `"viewer"`) {
		t.Fatalf("/members shows viewer: %s", b)
	}
	if code, _ := send(t, v.fixture, "alice", "team", viewerName, "hi"); code != 400 {
		t.Fatalf("DM to viewer: %d, want 400", code)
	}
	if res, isErr := toolCall(t, v.fixture, "alice", "send", map[string]any{"channel": "team", "to": viewerName, "text": "hi"}); !isErr {
		t.Fatalf("MCP send to viewer not refused: %v", res)
	}
	if res, _ := toolCall(t, v.fixture, "alice", "members", map[string]any{}); strings.Contains(func() string { b, _ := json.Marshal(res); return string(b) }(), `"viewer"`) {
		t.Fatalf("MCP members shows viewer")
	}
	if _, err := newServer([]string{"alice", viewerName}, nil); err == nil {
		t.Fatal("newServer accepted viewer as a member")
	}
	if slices.Contains(DefaultMembers, viewerName) {
		t.Fatal("viewer in DefaultMembers")
	}
	if _, ok := memberChannels[viewerName]; ok {
		t.Fatal("viewer in memberChannels")
	}
	// token new viewer works through the CLI and writes a 0600 file; revoke works too.
	if err := tokenRevoke(viewerName); err != nil {
		t.Fatal(err)
	}
	out2 := captureStdout(t, func() {
		if err := cmdToken([]string{"new", viewerName}); err != nil {
			t.Fatal(err)
		}
	})
	p := strings.TrimSpace(out2)
	if p != filepath.Join(configDir(), "token-viewer") {
		t.Fatalf("token new viewer printed %q", out2)
	}
	if st, _ := os.Stat(p); wrongMode(st, 0o600) {
		t.Fatalf("token-viewer mode %v", st.Mode().Perm())
	}
}

func TestViewLoginCookieAndAllTraffic(t *testing.T) {
	v := setupView(t)
	send(t, v.fixture, "alice", "team", "bob", "dm between agents")
	send(t, v.fixture, "erin", "general", "all", "hello general")
	putFile(t, v.fixture, "frank", "general", "cat.txt", "text/plain", []byte("meow"))

	resp, c := v.login(t, v.viewerTok, "")
	if resp.StatusCode != 303 || resp.Header.Get("Location") != "/view" || c == "" {
		t.Fatalf("login: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	raw := resp.Header.Get("Set-Cookie")
	for _, want := range []string{viewCookie + "=", "Path=/", "Max-Age=43200", "HttpOnly", "Secure", "SameSite=Strict"} {
		if !strings.Contains(raw, want) {
			t.Fatalf("Set-Cookie %q lacks %q", raw, want)
		}
	}
	if strings.Contains(raw, "Domain=") {
		t.Fatalf("Set-Cookie has a Domain: %q", raw)
	}
	code, msgs := v.messages(t, c)
	if code != 200 || len(msgs) != 3 {
		t.Fatalf("view messages: %d %v", code, msgs)
	}
	if msgs[0]["to"] != "bob" || msgs[1]["channel"] != "general" || msgs[2]["file"] == nil {
		t.Fatalf("view does not show all traffic: %v", msgs)
	}
	fid := msgs[2]["file_id"].(string)
	resp, body := v.do(t, "GET", "/view/files/"+fid, c, nil, "")
	if resp.StatusCode != 200 || body != "meow" || !strings.HasPrefix(resp.Header.Get("Content-Disposition"), "attachment") {
		t.Fatalf("view file: %d %q %q", resp.StatusCode, body, resp.Header.Get("Content-Disposition"))
	}
	if resp, _ := v.do(t, "GET", "/view/files/"+fid, "", nil, ""); resp.StatusCode != 401 {
		t.Fatalf("view file without session: %d", resp.StatusCode)
	}
	resp, body = v.do(t, "GET", "/view", c, nil, "")
	if resp.StatusCode != 200 || !strings.Contains(body, `src="/view/app.js"`) {
		t.Fatalf("app page: %d", resp.StatusCode)
	}

	// A second login rotates the session; the old cookie stops working.
	_, c2 := v.login(t, v.viewerTok, c)
	if c2 == "" || c2 == c {
		t.Fatal("login did not rotate the session")
	}
	if code, _ := v.messages(t, c); code != 401 {
		t.Fatalf("old session after rotate: %d", code)
	}
	// Logout ends the session and clears the cookie.
	resp, _ = v.do(t, "POST", "/view/logout", c2, map[string]string{"Origin": v.origin}, "")
	if resp.StatusCode != 303 || !strings.Contains(resp.Header.Get("Set-Cookie"), "Max-Age=0") {
		t.Fatalf("logout: %d %q", resp.StatusCode, resp.Header.Get("Set-Cookie"))
	}
	if code, _ := v.messages(t, c2); code != 401 {
		t.Fatalf("session after logout: %d", code)
	}
	// Rotating the viewer token ends live sessions.
	_, c3 := v.login(t, v.viewerTok, "")
	if _, err := tokenNew(viewerName, "", true); err != nil {
		t.Fatal(err)
	}
	if code, _ := v.messages(t, c3); code != 401 {
		t.Fatalf("session after viewer token rotate: %d", code)
	}
	// Expiry after 12 h.
	nt, _ := readToken(filepath.Join(configDir(), "token-viewer"))
	_, c4 := v.login(t, nt, "")
	base := time.Now()
	v.sv.now = func() time.Time { return base.Add(viewSessionTTL + time.Second) }
	if code, _ := v.messages(t, c4); code != 401 {
		t.Fatalf("session after 12 h: %d", code)
	}

	// Audit: login, logout and fail, without token or text.
	v.sv.now = time.Now
	v.login(t, "sc_wrong", "")
	ab, _ := os.ReadFile(filepath.Join(stateDir(), "audit.jsonl"))
	a := string(ab)
	for _, want := range []string{`"view-login"`, `"view-logout"`, `"view-login-fail"`, `"view-get_file"`} {
		if !strings.Contains(a, want) {
			t.Fatalf("audit lacks %s", want)
		}
	}
	for _, bad := range []string{v.viewerTok, nt, "sc_wrong", c, c2, "dm between agents", "meow"} {
		if strings.Contains(a, bad) {
			t.Fatalf("audit holds a secret or text: %q", bad)
		}
	}
}

func TestViewCSRFAndOrigin(t *testing.T) {
	v := setupView(t)
	form := url.Values{"token": {v.viewerTok}}.Encode()
	ct := "application/x-www-form-urlencoded"
	cases := []map[string]string{
		{"Content-Type": ct}, // no Origin
		{"Content-Type": ct, "Origin": "https://evil.example"}, // other origin
		{"Content-Type": ct, "Origin": "null"},
		{"Content-Type": ct, "Origin": v.origin, "Sec-Fetch-Site": "cross-site"},
	}
	for i, h := range cases {
		if resp, _ := v.do(t, "POST", "/view/login", "", h, form); resp.StatusCode != 403 || len(resp.Cookies()) != 0 {
			t.Fatalf("case %d: login %d, want 403", i, resp.StatusCode)
		}
	}
	_, c := v.login(t, v.viewerTok, "")
	if resp, _ := v.do(t, "POST", "/view/logout", c, map[string]string{"Origin": "https://evil.example"}, ""); resp.StatusCode != 403 {
		t.Fatalf("cross-site logout: %d", resp.StatusCode)
	}
	if resp, _ := v.do(t, "GET", "/view/api/messages", c, map[string]string{"Origin": "https://evil.example"}, ""); resp.StatusCode != 403 {
		t.Fatalf("cross-origin GET: %d", resp.StatusCode)
	}
	// Wrong Host (DNS rebinding) is refused.
	r, _ := http.NewRequest("GET", v.srv.URL+"/view", nil)
	r.Host = "evil.example:18765"
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("wrong Host: %d", resp.StatusCode)
	}
	// Agent API routes still refuse every Origin, even the view origin.
	r, _ = http.NewRequest("GET", v.srv.URL+"/members", nil)
	r.Header.Set("Origin", v.origin)
	r.Header.Set("Authorization", "Bearer "+v.tokens["alice"])
	resp, _ = http.DefaultClient.Do(r)
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("agent route with Origin: %d", resp.StatusCode)
	}
	// The session still works after the refused requests.
	if code, _ := v.messages(t, c); code != 200 {
		t.Fatalf("session: %d", code)
	}
}

func TestViewLockout(t *testing.T) {
	v := setupView(t)
	base := time.Now()
	v.sv.now = func() time.Time { return base }
	for i := 0; i < viewLockFails; i++ {
		if resp, _ := v.login(t, "sc_bad", ""); resp.StatusCode != 401 {
			t.Fatalf("bad try %d: %d", i, resp.StatusCode)
		}
	}
	if resp, c := v.login(t, v.viewerTok, ""); resp.StatusCode != 429 || c != "" {
		t.Fatalf("right token while locked: %d, want 429", resp.StatusCode)
	}
	v.sv.now = func() time.Time { return base.Add(14 * time.Minute) }
	if resp, _ := v.login(t, v.viewerTok, ""); resp.StatusCode != 429 {
		t.Fatalf("still locked at 14 min: %d", resp.StatusCode)
	}
	v.sv.now = func() time.Time { return base.Add(viewLockFor + time.Second) }
	if resp, c := v.login(t, v.viewerTok, ""); resp.StatusCode != 303 || c == "" {
		t.Fatalf("after lockout: %d", resp.StatusCode)
	}
	ab, _ := os.ReadFile(filepath.Join(stateDir(), "audit.jsonl"))
	if !strings.Contains(string(ab), `"view-login-locked"`) {
		t.Fatal("audit lacks view-login-locked")
	}
}

func TestViewXSSAndHeaders(t *testing.T) {
	v := setupView(t)
	payload := `<script>alert(1)</script><img src=x onerror=alert(2)>"'&`
	if code, _ := send(t, v.fixture, "alice", "team", "all", payload); code != 200 {
		t.Fatal("send")
	}
	putFile(t, v.fixture, "alice", "team", `<img src=x onerror=1>.txt`, "text/plain", []byte("x"))
	_, c := v.login(t, v.viewerTok, "")
	resp, body := v.do(t, "GET", "/view/api/messages", c, nil, "")
	if resp.Header.Get("Content-Type") != "application/json" || strings.ContainsAny(body, "<>") {
		t.Fatalf("view API must be JSON with < and > escaped: %q %s", resp.Header.Get("Content-Type"), body)
	}
	var out struct {
		Messages []Message `json:"messages"`
	}
	json.Unmarshal([]byte(body), &out)
	if len(out.Messages) == 0 || out.Messages[0].Text != payload {
		t.Fatal("payload not round-tripped as data")
	}
	// The script builds the DOM with textContent only.
	for _, bad := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "eval(", "new Function"} {
		if strings.Contains(viewJS, bad) {
			t.Fatalf("app.js uses %s", bad)
		}
	}
	if !strings.Contains(viewJS, "textContent") {
		t.Fatal("app.js does not use textContent")
	}
	// No inline script or style, no external resource.
	for _, page := range []string{viewAppHTML, loginPage("x")} {
		if strings.Contains(page, "<script>") || strings.Contains(page, "<style") || strings.Contains(page, " style=") ||
			regexp.MustCompile(` on[a-z]+=`).MatchString(page) || strings.Contains(page, "http") {
			t.Fatalf("page has inline code or an external URL")
		}
	}
	for _, p := range []string{"/view", "/view/app.js", "/view/app.css", "/view/api/messages", "/view/nope"} {
		resp, _ := v.do(t, "GET", p, c, nil, "")
		h := resp.Header
		csp := h.Get("Content-Security-Policy")
		if !strings.HasPrefix(csp, "default-src 'self'") || strings.Contains(csp, "unsafe-inline") || strings.Contains(csp, "unsafe-eval") ||
			h.Get("X-Frame-Options") != "DENY" || h.Get("Referrer-Policy") != "same-origin" || h.Get("Cache-Control") != "no-store" {
			t.Fatalf("%s headers: %v", p, h)
		}
	}
}

func TestViewAllowCIDR(t *testing.T) {
	v := setupView(t)
	v.sv.viewAllow = []netip.Prefix{netip.MustParsePrefix("198.51.100.0/24")}
	if resp, _ := v.do(t, "GET", "/view", "", nil, ""); resp.StatusCode != 403 {
		t.Fatalf("view from 127.0.0.1 outside --view-allow: %d", resp.StatusCode)
	}
	if resp, _ := v.login(t, v.viewerTok, ""); resp.StatusCode != 403 {
		t.Fatalf("login outside --view-allow: %d", resp.StatusCode)
	}
	// Agent routes are not affected by --view-allow.
	if code, _ := v.req(t, "GET", "/members", v.tokens["alice"], nil); code != 200 {
		t.Fatalf("agent route: %d", code)
	}
	v.sv.viewAllow = []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}
	if resp, _ := v.do(t, "GET", "/view", "", nil, ""); resp.StatusCode != 200 {
		t.Fatalf("view inside --view-allow: %d", resp.StatusCode)
	}
}

func TestTLSRenewServer(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := tlsInit(dir, "127.0.0.1,localhost", false, time.Now()); err != nil {
		t.Fatal(err)
	}
	caBefore, _ := os.ReadFile(filepath.Join(dir, "ca.pem"))
	oldSrv, _ := readCertPEM(filepath.Join(dir, "server.pem"))
	out := captureStdout(t, func() {
		if rc := run([]string{"tls", "renew-server", "--dir", dir}); rc != 0 {
			t.Fatalf("renew-server rc %d", rc)
		}
	})
	if strings.Contains(out, "PRIVATE KEY") {
		t.Fatal("renew-server printed key material")
	}
	caAfter, _ := os.ReadFile(filepath.Join(dir, "ca.pem"))
	if string(caBefore) != string(caAfter) {
		t.Fatal("renew-server changed ca.pem")
	}
	srv, err := readCertPEM(filepath.Join(dir, "server.pem"))
	if err != nil {
		t.Fatal(err)
	}
	if srv.SerialNumber.Cmp(oldSrv.SerialNumber) == 0 {
		t.Fatal("server certificate not replaced")
	}
	if d := srv.NotAfter.Sub(srv.NotBefore); d > 398*24*time.Hour {
		t.Fatalf("server certificate valid %v, over 398 days", d)
	}
	if !slices.Equal(srv.DNSNames, oldSrv.DNSNames) || len(srv.IPAddresses) != len(oldSrv.IPAddresses) {
		t.Fatal("renew-server lost the host names")
	}
	if st, _ := os.Stat(filepath.Join(dir, "server-key.pem")); wrongMode(st, 0o600) {
		t.Fatal("server key mode")
	}
	ca, _ := readCertPEM(filepath.Join(dir, "ca.pem"))
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	if _, err := srv.Verify(x509.VerifyOptions{Roots: pool, DNSName: "127.0.0.1"}); err != nil {
		t.Fatalf("new server cert does not verify against the old CA: %v", err)
	}
	if _, err := loadServerCert(filepath.Join(dir, "server.pem"), filepath.Join(dir, "server-key.pem")); err != nil {
		t.Fatal(err)
	}
}

// csrf reads the session CSRF token from the messages response.
func (v *viewFixture) csrf(t *testing.T, cookie string) string {
	t.Helper()
	_, body := v.do(t, "GET", "/view/api/messages?since=0", cookie, nil, "")
	var out struct {
		CSRF string `json:"csrf"`
	}
	json.Unmarshal([]byte(body), &out)
	if out.CSRF == "" {
		t.Fatal("no csrf in the messages response")
	}
	return out.CSRF
}

func (v *viewFixture) viewSend(t *testing.T, cookie, origin string, form url.Values) (int, map[string]any) {
	t.Helper()
	resp, body := v.do(t, "POST", "/view/send", cookie, map[string]string{
		"Origin": origin, "Content-Type": "application/x-www-form-urlencoded", "Sec-Fetch-Site": "same-origin",
	}, form.Encode())
	var out map[string]any
	json.Unmarshal([]byte(body), &out)
	return resp.StatusCode, out
}

func fromViewer(t *testing.T, ms []map[string]any, text string) bool {
	t.Helper()
	for _, m := range ms {
		if m["from"] == viewerName && m["text"] == text {
			return true
		}
	}
	return false
}

func TestViewSendLandsInInbox(t *testing.T) {
	v := setupView(t)
	_, c := v.login(t, v.viewerTok, "")
	tok := v.csrf(t, c)
	code, m := v.viewSend(t, c, v.origin, url.Values{"csrf": {tok}, "channel": {"team"}, "to": {"all"}, "text": {"hello from the owner"}})
	if code != 200 || m["from"] != viewerName || m["to"] != "all" || m["channel"] != "team" {
		t.Fatalf("view send work/all: %d %v", code, m)
	}
	for _, mem := range []string{"alice", "bob", "dave"} {
		_, in := v.req(t, "GET", "/inbox", v.tokens[mem], nil)
		if !fromViewer(t, msgs(t, in), "hello from the owner") {
			t.Fatalf("%s inbox lacks the viewer message: %v", mem, in)
		}
	}
	_, in := v.req(t, "GET", "/inbox", v.tokens["erin"], nil)
	if fromViewer(t, msgs(t, in), "hello from the owner") {
		t.Fatal("general-only member sees a work message")
	}
	// MCP inbox shows from=viewer too.
	res, isErr := toolCall(t, v.fixture, "carol", "inbox", map[string]any{})
	if b, _ := json.Marshal(res); isErr || !strings.Contains(string(b), `"from":"viewer"`) {
		t.Fatalf("MCP inbox lacks from=viewer: %s", b)
	}
	// general, to all: every general member gets it.
	code, _ = v.viewSend(t, c, v.origin, url.Values{"csrf": {tok}, "channel": {"general"}, "to": {"all"}, "text": {"good night all"}})
	if code != 200 {
		t.Fatalf("view send general: %d", code)
	}
	for _, mem := range []string{"erin", "frank"} {
		_, in := v.req(t, "GET", "/inbox?channel=general", v.tokens[mem], nil)
		if !fromViewer(t, msgs(t, in), "good night all") {
			t.Fatalf("%s general inbox lacks the viewer message", mem)
		}
	}
	// DM to one member: only that member gets it.
	code, _ = v.viewSend(t, c, v.origin, url.Values{"csrf": {tok}, "channel": {"team"}, "to": {"dave"}, "text": {"only for dave"}})
	if code != 200 {
		t.Fatalf("view DM: %d", code)
	}
	_, in = v.req(t, "GET", "/inbox", v.tokens["dave"], nil)
	if !fromViewer(t, msgs(t, in), "only for dave") {
		t.Fatal("dave lacks the DM")
	}
	_, in = v.req(t, "GET", "/inbox", v.tokens["alice"], nil)
	if fromViewer(t, msgs(t, in), "only for dave") {
		t.Fatal("alice sees a DM to dave")
	}
	// The view itself shows the sent messages.
	if _, all := v.messages(t, c); !fromViewer(t, all, "only for dave") {
		t.Fatal("view does not show its own message")
	}
	// Bad requests: unknown channel, empty to, viewer as recipient, member not in channel, empty text.
	for name, f := range map[string]url.Values{
		"channel":     {"channel": {"secret"}, "to": {"all"}, "text": {"x"}},
		"empty to":    {"channel": {"team"}, "to": {""}, "text": {"x"}},
		"to viewer":   {"channel": {"team"}, "to": {viewerName}, "text": {"x"}},
		"not in chan": {"channel": {"team"}, "to": {"erin"}, "text": {"x"}},
		"empty text":  {"channel": {"team"}, "to": {"all"}, "text": {"  "}},
	} {
		f.Set("csrf", tok)
		if code, _ := v.viewSend(t, c, v.origin, f); code != 400 {
			t.Fatalf("%s: %d, want 400", name, code)
		}
	}
	// Agents still cannot DM viewer, and /members still lacks viewer.
	if code, _ := send(t, v.fixture, "alice", "team", viewerName, "reply"); code != 400 {
		t.Fatalf("agent DM to viewer: %d, want 400", code)
	}
	_, out := v.req(t, "GET", "/members", v.tokens["alice"], nil)
	if b, _ := json.Marshal(out); strings.Contains(string(b), `"viewer"`) {
		t.Fatalf("/members shows viewer: %s", b)
	}
	// Audit records view-send without the text.
	raw, _ := os.ReadFile(filepath.Join(stateDir(), "audit.jsonl"))
	if !strings.Contains(string(raw), `"view-send"`) || strings.Contains(string(raw), "only for dave") {
		t.Fatalf("audit view-send wrong: %s", raw)
	}
}

func TestViewSendGeneralLint(t *testing.T) {
	v := setupView(t)
	_, c := v.login(t, v.viewerTok, "")
	tok := v.csrf(t, c)
	code, out := v.viewSend(t, c, v.origin, url.Values{"csrf": {tok}, "channel": {"general"}, "to": {"all"}, "text": {"did you see the Acme build"}})
	if code != 422 {
		t.Fatalf("general lint for viewer: %d %v, want 422", code, out)
	}
	_, in := v.req(t, "GET", "/inbox?channel=general", v.tokens["erin"], nil)
	if len(msgs(t, in)) != 0 {
		t.Fatalf("rejected message stored: %v", in)
	}
	if code, _ := v.viewSend(t, c, v.origin, url.Values{"csrf": {tok}, "channel": {"team"}, "to": {"all"}, "text": {"the Acme build is green"}}); code != 200 {
		t.Fatalf("work is not linted: %d", code)
	}
}

func TestViewSendCSRF(t *testing.T) {
	v := setupView(t)
	_, c := v.login(t, v.viewerTok, "")
	tok := v.csrf(t, c)
	ok := url.Values{"csrf": {tok}, "channel": {"team"}, "to": {"all"}, "text": {"x"}}
	cases := []struct {
		name, origin, site string
		form               url.Values
	}{
		{"no csrf", v.origin, "same-origin", url.Values{"channel": {"team"}, "to": {"all"}, "text": {"x"}}},
		{"wrong csrf", v.origin, "same-origin", url.Values{"csrf": {strings.Repeat("0", 64)}, "channel": {"team"}, "to": {"all"}, "text": {"x"}}},
		{"no origin", "", "same-origin", ok},
		{"other origin", "https://evil.example", "same-origin", ok},
		{"cross-site", v.origin, "cross-site", ok},
	}
	for _, tc := range cases {
		resp, _ := v.do(t, "POST", "/view/send", c, map[string]string{
			"Origin": tc.origin, "Content-Type": "application/x-www-form-urlencoded", "Sec-Fetch-Site": tc.site,
		}, tc.form.Encode())
		if resp.StatusCode != 403 {
			t.Fatalf("%s: %d, want 403", tc.name, resp.StatusCode)
		}
	}
	// A token from another session does not work for this one.
	_, c2 := v.login(t, v.viewerTok, "")
	if code, _ := v.viewSend(t, c2, v.origin, ok); code != 403 {
		t.Fatalf("csrf of another session: %d, want 403", code)
	}
	// JSON body is refused (form only).
	resp, _ := v.do(t, "POST", "/view/send", c2, map[string]string{"Origin": v.origin, "Content-Type": "application/json"}, `{"channel":"team","to":"all","text":"x"}`)
	if resp.StatusCode != 400 {
		t.Fatalf("json body: %d, want 400", resp.StatusCode)
	}
	_, in := v.req(t, "GET", "/inbox", v.tokens["alice"], nil)
	if len(msgs(t, in)) != 0 {
		t.Fatalf("refused sends were stored: %v", in)
	}
}

func TestViewSendAgentTokenRefused(t *testing.T) {
	v := setupView(t)
	form := url.Values{"channel": {"team"}, "to": {"all"}, "text": {"x"}}.Encode()
	hdr := map[string]string{"Origin": v.origin, "Content-Type": "application/x-www-form-urlencoded", "Authorization": "Bearer " + v.tokens["alice"]}
	if resp, _ := v.do(t, "POST", "/view/send", "", hdr, form); resp.StatusCode != 401 {
		t.Fatalf("agent bearer on /view/send: %d, want 401", resp.StatusCode)
	}
	// An agent token used as the session cookie does not work either.
	if resp, _ := v.do(t, "POST", "/view/send", v.tokens["alice"], hdr, form); resp.StatusCode != 401 {
		t.Fatalf("agent token as cookie: %d, want 401", resp.StatusCode)
	}
	// Without a session cookie, even the viewer token as bearer is refused.
	hdr["Authorization"] = "Bearer " + v.viewerTok
	if resp, _ := v.do(t, "POST", "/view/send", "", hdr, form); resp.StatusCode != 401 {
		t.Fatalf("viewer bearer without session: %d, want 401", resp.StatusCode)
	}
	_, in := v.req(t, "GET", "/inbox", v.tokens["dave"], nil)
	if len(msgs(t, in)) != 0 {
		t.Fatalf("refused send stored: %v", in)
	}
}

func TestViewSendRateLimit(t *testing.T) {
	v := setupView(t)
	_, c := v.login(t, v.viewerTok, "")
	tok := v.csrf(t, c)
	got429 := false
	for i := 0; i < rateBurst+2; i++ {
		code, _ := v.viewSend(t, c, v.origin, url.Values{"csrf": {tok}, "channel": {"team"}, "to": {"all"}, "text": {"n"}})
		if code == 429 {
			got429 = true
			break
		}
		if code != 200 {
			t.Fatalf("send %d: %d", i, code)
		}
	}
	if !got429 {
		t.Fatal("no rate limit for view send")
	}
}

func TestViewPageHasSendForm(t *testing.T) {
	v := setupView(t)
	_, c := v.login(t, v.viewerTok, "")
	_, body := v.do(t, "GET", "/view", c, nil, "")
	for _, want := range []string{`id="send"`, `action="/view/send"`, `name="channel"`, `name="to"`, `name="text"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("page lacks %s", want)
		}
	}
	if strings.Contains(viewJS, "innerHTML") {
		t.Fatal("script uses innerHTML")
	}
}
