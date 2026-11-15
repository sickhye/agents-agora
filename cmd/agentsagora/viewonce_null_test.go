package main

import (
	"net/http"
	"net/url"
	"testing"
	"time"
)

// Android Chrome sends Origin "null" on the one-time login form after a certificate warning
// POST /view/once takes it unless the browser marks the request cross-site;
// every other /view route still refuses it.
func TestViewOnceAcceptsNullOriginSameSite(t *testing.T) {
	v := setupView(t)
	code := issueTest(t, time.Now())
	form := url.Values{viewOnceField: {code}}.Encode()
	ct := "application/x-www-form-urlencoded"

	resp, _ := v.do(t, "POST", "/view/once", "", map[string]string{
		"Origin": "null", "Content-Type": ct, "Sec-Fetch-Site": "cross-site",
	}, form)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("null origin marked cross-site: %d, want 403", resp.StatusCode)
	}

	resp, _ = v.do(t, "POST", "/view/login", "", map[string]string{
		"Origin": "null", "Content-Type": ct, "Sec-Fetch-Site": "same-origin",
	}, url.Values{"token": {"x"}}.Encode())
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("null origin on /view/login: %d, want 403", resp.StatusCode)
	}

	resp, _ = v.do(t, "POST", "/view/once", "", map[string]string{
		"Origin": "null", "Content-Type": ct, "Sec-Fetch-Site": "same-origin",
	}, form)
	got := ""
	for _, c := range resp.Cookies() {
		if c.Name == viewCookie {
			got = c.Value
		}
	}
	if got == "" {
		t.Fatalf("null origin same-site once login: %d, no session cookie", resp.StatusCode)
	}
}
