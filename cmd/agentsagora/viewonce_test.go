package main

import (
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// captureStderr runs fn with os.Stderr sent to a pipe and returns what was written.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = w
	done := make(chan string)
	go func() { b, _ := io.ReadAll(r); done <- string(b) }()
	defer func() { os.Stderr = old }()
	fn()
	w.Close()
	os.Stderr = old
	return <-done
}

func (v *viewFixture) once(t *testing.T, code, oldCookie string) (*http.Response, string) {
	t.Helper()
	resp, _ := v.do(t, "POST", "/view/once", oldCookie, map[string]string{
		"Origin": v.origin, "Content-Type": "application/x-www-form-urlencoded", "Sec-Fetch-Site": "same-origin",
	}, url.Values{viewOnceField: {code}}.Encode())
	val := ""
	for _, c := range resp.Cookies() {
		if c.Name == viewCookie {
			val = c.Value
		}
	}
	return resp, val
}

func issueTest(t *testing.T, now time.Time) string {
	t.Helper()
	code, err := issueViewOnce(viewOncePath(), now)
	if err != nil {
		t.Fatal(err)
	}
	return code
}

func TestViewOnceIssueStoresOnlyHash(t *testing.T) {
	setupView(t)
	code := issueTest(t, time.Now())
	if !viewOnceCodeRE.MatchString(code) {
		t.Fatalf("code shape %q", code)
	}
	st, err := os.Stat(viewOncePath())
	if err != nil || wrongMode(st, 0o600) {
		t.Fatalf("code file: %v %v, want mode 0600", err, st)
	}
	b, _ := os.ReadFile(viewOncePath())
	if strings.Contains(string(b), code) || !strings.Contains(string(b), sha256hex(code)) {
		t.Fatalf("code file must hold the sha256 only:\n%s", b)
	}
	if c2 := issueTest(t, time.Now()); c2 == code {
		t.Fatal("two issues gave the same code")
	}
}

func TestViewOnceGetDoesNotConsumeThenPostLogsIn(t *testing.T) {
	v := setupView(t)
	code := issueTest(t, time.Now())
	for i := 0; i < 3; i++ { // link previews may fetch it several times
		resp, body := v.do(t, "GET", "/view/once?c="+code, "", nil, "")
		if resp.StatusCode != 200 || !strings.Contains(body, `action="/view/once"`) || !strings.Contains(body, "Log in") {
			t.Fatalf("GET /view/once: %d %s", resp.StatusCode, body)
		}
		if len(resp.Cookies()) != 0 {
			t.Fatal("GET must not start a session")
		}
	}
	if _, err := os.Stat(viewOncePath()); err != nil {
		t.Fatalf("GET consumed the code: %v", err)
	}
	resp, c := v.once(t, code, "")
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/view" || c == "" {
		t.Fatalf("POST /view/once: %d loc=%q cookie=%q", resp.StatusCode, resp.Header.Get("Location"), c)
	}
	// Same session as a token login: same cookie, page and API work, CSRF present.
	if code, _ := v.messages(t, c); code != 200 {
		t.Fatalf("messages with once session: %d", code)
	}
	if _, body := v.do(t, "GET", "/view", c, nil, ""); !strings.Contains(body, "app.js") {
		t.Fatal("once session does not open the view")
	}
	if v.sv.view.csrfOf(c) == "" {
		t.Fatal("once session has no CSRF token")
	}
	for _, ck := range resp.Cookies() {
		if ck.Name == viewCookie && (!ck.HttpOnly || !ck.Secure || ck.SameSite != http.SameSiteStrictMode || ck.MaxAge != int(viewSessionTTL.Seconds())) {
			t.Fatalf("cookie attributes differ from a token login: %+v", ck)
		}
	}
}

func TestViewOnceSingleUse(t *testing.T) {
	v := setupView(t)
	code := issueTest(t, time.Now())
	if resp, c := v.once(t, code, ""); resp.StatusCode != http.StatusSeeOther || c == "" {
		t.Fatalf("first use: %d", resp.StatusCode)
	}
	resp, c := v.once(t, code, "")
	if resp.StatusCode != 401 || c != "" {
		t.Fatalf("second use: %d cookie=%q, want 401 and no cookie", resp.StatusCode, c)
	}
	// Even if the file came back (e.g. could not be removed), this process refuses it.
	if _, ok := v.sv.view.once.consume(code, time.Now()); ok {
		t.Fatal("consume after use")
	}
}

func TestViewOnceNewerReplacesOlder(t *testing.T) {
	v := setupView(t)
	old := issueTest(t, time.Now())
	nw := issueTest(t, time.Now())
	if resp, _ := v.once(t, old, ""); resp.StatusCode != 401 {
		t.Fatalf("older code after a new issue: %d, want 401", resp.StatusCode)
	}
	if resp, c := v.once(t, nw, ""); resp.StatusCode != http.StatusSeeOther || c == "" {
		t.Fatalf("newest code: %d", resp.StatusCode)
	}
}

func TestViewOnceExpiry(t *testing.T) {
	v := setupView(t)
	base := time.Date(2026, 11, 15, 9, 0, 0, 0, jst)
	code := issueTest(t, base)
	v.sv.now = func() time.Time { return base.Add(viewOnceTTL) }
	if resp, c := v.once(t, code, ""); resp.StatusCode != 401 || c != "" {
		t.Fatalf("code at issue+24h: %d, want 401", resp.StatusCode)
	}
	code = issueTest(t, base)
	v.sv.now = func() time.Time { return base.Add(viewOnceTTL - time.Minute) }
	if resp, c := v.once(t, code, ""); resp.StatusCode != http.StatusSeeOther || c == "" {
		t.Fatalf("code just before issue+24h: %d", resp.StatusCode)
	}
}

// A session from a one-time link slides like a token session but never passes issue+24h.
func TestViewOnceSessionCappedAtIssuePlus24h(t *testing.T) {
	v := setupView(t)
	base := time.Date(2026, 11, 15, 9, 0, 0, 0, jst)
	limit := base.Add(viewOnceTTL)
	code := issueTest(t, base)
	at := base.Add(20 * time.Hour)
	v.sv.now = func() time.Time { return at }
	resp, c := v.once(t, code, "")
	if resp.StatusCode != http.StatusSeeOther || c == "" {
		t.Fatalf("once login: %d", resp.StatusCode)
	}
	for _, ck := range resp.Cookies() {
		if ck.Name == viewCookie && ck.MaxAge != int(4*time.Hour/time.Second) {
			t.Fatalf("cookie Max-Age %d, want 4 h (until issue+24h)", ck.MaxAge)
		}
	}
	viewer := v.sv.tokens.viewerHash()
	// Activity just before the limit must not extend it.
	for _, d := range []time.Duration{time.Hour, 3 * time.Hour, 4*time.Hour - time.Second} {
		ok, renewed := v.sv.view.check(c, viewer, at.Add(d))
		if !ok || renewed.After(limit) {
			t.Fatalf("at +%v: ok=%v renewed=%v, limit %v", d, ok, renewed, limit)
		}
	}
	if v.sv.view.valid(c, viewer, limit) {
		t.Fatal("once session alive at issue+24h")
	}
	// The limit survives a restart (session file).
	code = issueTest(t, base)
	at = base.Add(time.Hour)
	v.sv.now = func() time.Time { return at }
	v.sv.view.limit = newLimiter(viewLoginBurst, viewLoginPerMin) // the fake clock went back
	if resp, c = v.once(t, code, ""); resp.StatusCode != http.StatusSeeOther || c == "" {
		t.Fatalf("second once login: %d", resp.StatusCode)
	}
	vs2, err := newViewState(viewSessionsPath(), at)
	if err != nil {
		t.Fatal(err)
	}
	if !vs2.valid(c, viewer, at.Add(11*time.Hour)) {
		t.Fatal("reloaded once session gone too early")
	}
	ok, renewed := vs2.check(c, viewer, limit.Add(-time.Hour))
	if !ok || renewed.After(limit) {
		t.Fatalf("reloaded session renewed past limit: ok=%v %v", ok, renewed)
	}
	if vs2.valid(c, viewer, limit) {
		t.Fatal("reloaded once session alive at issue+24h")
	}
	// A token-login session is unchanged: it slides past issue+24h.
	_, tc := v.login(t, v.viewerTok, "")
	if ok, renewed := v.sv.view.check(tc, viewer, at.Add(7*time.Hour)); !ok || !renewed.Equal(at.Add(19*time.Hour)) {
		t.Fatalf("token session renewal changed: ok=%v %v", ok, renewed)
	}
}

func TestViewOnceBadCodeCountsTowardLockout(t *testing.T) {
	v := setupView(t)
	code := issueTest(t, time.Now())
	bad := strings.Repeat("A", 43)
	for i := 0; i < viewLockFails; i++ {
		resp, c := v.once(t, bad, "")
		if resp.StatusCode != 401 || c != "" {
			t.Fatalf("bad code %d: %d", i, resp.StatusCode)
		}
	}
	if resp, c := v.once(t, code, ""); resp.StatusCode != 429 || c != "" {
		t.Fatalf("good code while locked: %d, want 429", resp.StatusCode)
	}
	if resp, _ := v.login(t, v.viewerTok, ""); resp.StatusCode != 429 {
		t.Fatalf("token login after once failures: %d, want 429 (shared lockout)", resp.StatusCode)
	}
	if _, err := os.Stat(viewOncePath()); err != nil {
		t.Fatal("a locked request consumed the code")
	}
}

func TestViewOnceGetBadShape(t *testing.T) {
	v := setupView(t)
	for _, q := range []string{"", "?c=short", "?c=" + url.QueryEscape(`"><script>`+strings.Repeat("a", 34))} {
		resp, body := v.do(t, "GET", "/view/once"+q, "", nil, "")
		if resp.StatusCode != 400 || strings.Contains(body, "<script>") {
			t.Fatalf("GET /view/once%s: %d", q, resp.StatusCode)
		}
	}
	// POST without Origin is refused by viewGuard like /view/login.
	code := issueTest(t, time.Now())
	resp, _ := v.do(t, "POST", "/view/once", "", map[string]string{"Content-Type": "application/x-www-form-urlencoded"}, "c="+code)
	if resp.StatusCode != 403 {
		t.Fatalf("POST without Origin: %d", resp.StatusCode)
	}
}

func TestViewOnceNoCodeInLogs(t *testing.T) {
	v := setupView(t)
	var sent, code string
	out := captureStderr(t, func() {
		err := viewOnceDM(viewOncePath(), viewOnceOrigin, time.Now(), func(m string) error { sent = m; return nil })
		if err != nil {
			t.Fatal(err)
		}
		code = sent[strings.LastIndex(sent, "=")+1:]
		v.do(t, "GET", "/view/once?c="+code, "", nil, "")
		v.once(t, code, "")
		v.once(t, code, "")
	})
	if !strings.HasPrefix(sent, "a2 login link (valid 24 hours, single use): "+viewOnceOrigin+"/view/once?c=") || !viewOnceCodeRE.MatchString(code) {
		t.Fatalf("DM text %q", sent)
	}
	for _, want := range []string{"view once: issued", "view once: used", "view once: rejected"} {
		if !strings.Contains(out, want) {
			t.Errorf("stderr misses %q:\n%s", want, out)
		}
	}
	audit, _ := os.ReadFile(filepath.Join(stateDir(), "audit.jsonl"))
	for name, s := range map[string]string{"stderr": out, "audit": string(audit)} {
		if strings.Contains(s, code) || strings.Contains(s, sha256hex(code)) {
			t.Errorf("%s holds the code", name)
		}
	}
	for _, want := range []string{`"action":"view-once-used"`, `"action":"view-once-rejected"`} {
		if !strings.Contains(string(audit), want) {
			t.Errorf("audit misses %s", want)
		}
	}
}

func TestViewOnceDMFailureWithdrawsCode(t *testing.T) {
	setupView(t)
	err := viewOnceDM(viewOncePath(), viewOnceOrigin, time.Now(), func(string) error { return errors.New("notify down") })
	if err == nil {
		t.Fatal("want an error")
	}
	if _, err := os.Stat(viewOncePath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("code file left after a failed DM: %v", err)
	}
}

func TestViewOnceDryRun(t *testing.T) {
	setupView(t)
	if err := cmdViewOnce([]string{"dm", "--dry-run"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(viewOncePath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("dry run issued a code")
	}
	if got := viewOnceMessage(viewOnceOrigin, "<code>"); got != "a2 login link (valid 24 hours, single use): https://127.0.0.1:18765/view/once?c=<code>" {
		t.Fatalf("dry-run text %q", got)
	}
}

func TestViewOnceAlreadyLoggedInRedirectsWithoutTouchingCode(t *testing.T) {
	v := setupView(t)
	code := issueTest(t, time.Now())
	_, c := v.login(t, v.viewerTok, "")
	if c == "" {
		t.Fatal("no session")
	}
	resp, _ := v.do(t, "GET", "/view/once?c="+code, c, nil, "")
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/view" {
		t.Fatalf("GET with session: %d loc=%q", resp.StatusCode, resp.Header.Get("Location"))
	}
	// The code is still usable by a fresh client.
	if resp, c2 := v.once(t, code, ""); resp.StatusCode != http.StatusSeeOther || c2 == "" {
		t.Fatalf("code unusable after redirect: %d", resp.StatusCode)
	}
}

func TestViewOnceUsedCodeWithSessionRedirectsNoLockout(t *testing.T) {
	v := setupView(t)
	code := issueTest(t, time.Now())
	resp, c := v.once(t, code, "")
	if resp.StatusCode != http.StatusSeeOther || c == "" {
		t.Fatalf("first login: %d", resp.StatusCode)
	}
	for i := 0; i < viewLockFails+2; i++ {
		resp, _ := v.once(t, code, c)
		if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/view" {
			t.Fatalf("used code with session #%d: %d", i, resp.StatusCode)
		}
	}
	if len(v.sv.view.fails) != 0 {
		t.Fatalf("failures counted: %d", len(v.sv.view.fails))
	}
	// Without a session the same used code fails as before.
	if resp, _ := v.once(t, code, ""); resp.StatusCode != 401 {
		t.Fatalf("used code without session: %d, want 401", resp.StatusCode)
	}
}

func TestViewOnceFormIsNoStore(t *testing.T) {
	v := setupView(t)
	code := issueTest(t, time.Now())
	resp, _ := v.do(t, "GET", "/view/once?c="+code, "", nil, "")
	if resp.StatusCode != 200 || resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("GET form: %d cc=%q", resp.StatusCode, resp.Header.Get("Cache-Control"))
	}
}
