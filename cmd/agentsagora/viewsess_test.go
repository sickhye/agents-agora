package main

import (
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Every test here uses setup/setupView (temp config and state dirs) or t.TempDir().

func TestViewSessionSurvivesRestart(t *testing.T) {
	v := setupView(t)
	resp, c := v.login(t, v.viewerTok, "")
	if resp.StatusCode != 303 || c == "" {
		t.Fatalf("login: %d", resp.StatusCode)
	}
	sc := resp.Header.Get("Set-Cookie")
	for _, want := range []string{"Max-Age=43200", "HttpOnly", "Secure", "SameSite=Strict"} {
		if !strings.Contains(sc, want) {
			t.Fatalf("cookie %q lacks %s", sc, want)
		}
	}
	csrf := v.csrf(t, c)

	// Restart: a new server and view state from the same dirs.
	v.restart(t)
	if code, _ := v.messages(t, c); code != 200 {
		t.Fatalf("session after restart: %d, want 200", code)
	}
	if got := v.csrf(t, c); got != csrf {
		t.Fatal("CSRF token changed across the restart")
	}
	code, _ := v.viewSend(t, c, v.origin, url.Values{"csrf": {csrf}, "channel": {"team"}, "to": {"all"}, "text": {"after restart"}})
	if code != 200 {
		t.Fatalf("send after restart: %d", code)
	}
	code, _ = v.viewSend(t, c, v.origin, url.Values{"csrf": {strings.Repeat("0", 64)}, "channel": {"team"}, "to": {"all"}, "text": {"x"}})
	if code != 403 {
		t.Fatalf("wrong csrf after restart: %d, want 403", code)
	}

	// File: mode 0600 in a 0700 dir, no raw cookie value and no raw CSRF token.
	p := viewSessionsPath()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), c) || strings.Contains(string(b), csrf) {
		t.Fatal("session file holds the raw cookie value or CSRF token")
	}
	if !strings.Contains(string(b), sessionKey(c)) {
		t.Fatal("session file lacks the session hash")
	}
	if runtime.GOOS != "windows" {
		if st, _ := os.Stat(p); st.Mode().Perm() != 0o600 {
			t.Fatalf("session file mode %v, want 0600", st.Mode().Perm())
		}
		if st, _ := os.Stat(filepath.Dir(p)); st.Mode().Perm() != 0o700 {
			t.Fatalf("state dir mode %v, want 0700", st.Mode().Perm())
		}
	}

	// Logout deletes the entry, also after another restart.
	v.do(t, "POST", "/view/logout", c, map[string]string{"Origin": v.origin}, "")
	if b, _ := os.ReadFile(p); strings.Contains(string(b), sessionKey(c)) {
		t.Fatal("logout left the session in the file")
	}
	v.restart(t)
	if code, _ := v.messages(t, c); code != 401 {
		t.Fatalf("session after logout and restart: %d, want 401", code)
	}
}

func TestViewSessionExpiredAfterReload(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "state", viewSessionsFile)
	viewer := []byte("viewer-token-hash-0123456789abcdef01")
	base := time.Now()
	vs, err := newViewState(p, base)
	if err != nil {
		t.Fatal(err)
	}
	c, err := vs.newSession(viewer, base)
	if err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(filepath.Dir(p)); err != nil || st.Mode().Perm() != 0o700 {
		t.Fatalf("state dir: %v %v", st, err)
	}
	vs2, err := newViewState(p, base.Add(viewSessionTTL-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if !vs2.valid(c, viewer, base.Add(viewSessionTTL-time.Minute)) {
		t.Fatal("live session lost on reload")
	}
	// vs2 renewed the session (less than 6 h left); reload after the renewed expiry.
	late := base.Add(2*viewSessionTTL + time.Minute)
	vs3, err := newViewState(p, late)
	if err != nil {
		t.Fatal(err)
	}
	if vs3.valid(c, viewer, late) {
		t.Fatal("expired session accepted after reload")
	}
	if len(vs3.sessions) != 0 {
		t.Fatal("expired session kept in memory after reload")
	}
	if b, _ := os.ReadFile(p); strings.Contains(string(b), sessionKey(c)) {
		t.Fatal("expired session left in the file after reload")
	}
}

func TestViewSessionSliding(t *testing.T) {
	p := filepath.Join(t.TempDir(), viewSessionsFile)
	viewer := []byte("viewer-token-hash-0123456789abcdef01")
	base := time.Now()
	vs, _ := newViewState(p, base)
	c, _ := vs.newSession(viewer, base)
	// More than 6 h left: no renewal.
	if ok, r := vs.check(c, viewer, base.Add(5*time.Hour)); !ok || !r.IsZero() {
		t.Fatalf("at 5 h: ok=%v renewed=%v, want ok and no renewal", ok, r)
	}
	// In the last 6 h: expiry moves to now+12 h.
	at := base.Add(7 * time.Hour)
	ok, r := vs.check(c, viewer, at)
	if !ok || !r.Equal(at.Add(viewSessionTTL)) {
		t.Fatalf("at 7 h: ok=%v renewed=%v, want %v", ok, r, at.Add(viewSessionTTL))
	}
	// Keep renewing: never past login+7 days.
	now := at
	for i := 0; i < 40; i++ {
		now = now.Add(7 * time.Hour)
		if ok, _ := vs.check(c, viewer, now); !ok && now.Before(base.Add(viewSessionMax)) {
			t.Fatalf("session ended at %v before the 7 day cap", now.Sub(base))
		}
	}
	if vs.valid(c, viewer, base.Add(viewSessionMax)) {
		t.Fatal("session alive at login+7 days")
	}
	// No request for 12 h: expired.
	c2, _ := vs.newSession(viewer, base)
	if vs.valid(c2, viewer, base.Add(viewSessionTTL)) {
		t.Fatal("idle session alive after 12 h")
	}
}

func TestViewSessionsClearedByViewerRotateAndRevoke(t *testing.T) {
	v := setupView(t)
	_, c := v.login(t, v.viewerTok, "")
	p := viewSessionsPath()
	if _, err := os.Stat(p); err != nil {
		t.Fatal(err)
	}
	if _, err := tokenNew(viewerName, "", true); err != nil { // token rotate viewer
		t.Fatal(err)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatalf("session file after viewer rotate: %v, want removed", err)
	}
	if code, _ := v.messages(t, c); code != 401 {
		t.Fatalf("session after viewer rotate: %d", code)
	}
	// A rotated token also ends a session still in a saved file (server was down).
	nt, _ := readToken(filepath.Join(configDir(), "token-viewer"))
	_, c2 := v.login(t, nt, "")
	b, _ := os.ReadFile(p)
	if _, err := tokenNew(viewerName, "", true); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(p, b, 0o600) // as if the file survived
	v.restart(t)
	if code, _ := v.messages(t, c2); code != 401 {
		t.Fatalf("saved session of the old viewer token after restart: %d", code)
	}
	// Revoke viewer.
	nt, _ = readToken(filepath.Join(configDir(), "token-viewer"))
	_, c3 := v.login(t, nt, "")
	if code, _ := v.messages(t, c3); code != 200 {
		t.Fatalf("fresh session: %d", code)
	}
	if err := tokenRevoke(viewerName); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatalf("session file after viewer revoke: %v, want removed", err)
	}
	if code, _ := v.messages(t, c3); code != 401 {
		t.Fatalf("session after viewer revoke: %d", code)
	}
}

func TestViewSessionRenewResendsCookie(t *testing.T) {
	v := setupView(t)
	_, c := v.login(t, v.viewerTok, "")
	base := time.Now()
	v.sv.now = func() time.Time { return base.Add(8 * time.Hour) }
	resp, _ := v.do(t, "GET", "/view/api/messages?since=0", c, nil, "")
	sc := resp.Header.Get("Set-Cookie")
	if resp.StatusCode != 200 || !strings.Contains(sc, "Max-Age=43200") {
		t.Fatalf("renewal: %d cookie %q, want a fresh 12 h Max-Age", resp.StatusCode, sc)
	}
	for _, want := range []string{"HttpOnly", "Secure", "SameSite=Strict", viewCookie + "=" + c} {
		if !strings.Contains(sc, want) {
			t.Fatalf("renewed cookie %q lacks %s", sc, want)
		}
	}
}

// restart replaces the server with a new one built from the same config and state dirs,
// as after a process restart.
func (v *viewFixture) restart(t *testing.T) {
	t.Helper()
	sv, err := newServer(DefaultMembers, nil, denyFile)
	if err != nil {
		t.Fatal(err)
	}
	v.srv.Close()
	srv := httptest.NewServer(sv.handler())
	t.Cleanup(srv.Close)
	v.srv, v.sv = srv, sv
	v.origin = "https://" + srv.Listener.Addr().String()
	sv.viewOrigin = v.origin
}
