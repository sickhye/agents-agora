package main

// One-time login link for the viewer UI.
//
// Issuing is local only: `agentsagora view-once dm --notify-command CMD` (run on the server
// host, e.g. from a daily timer) makes a random code (32 bytes from crypto/rand,
// base64url) and writes only its sha256 and expiry to stateDir()/view-once.json (mode
// 0600, dir 0700). There is no HTTP route that issues a code, so nobody on the network
// and no member can make one; only a process of the server's own user on the server host
// can write that file. A new code replaces the file, so an older unused code stops
// working. The command then pipes one message with the link to CMD's stdin (CMD delivers
// it to the owner however you like: mail, chat, push notification); the code is never
// printed to stdout or stderr.
//
// GET /view/once?c=<code> does not look at the code's validity and never consumes it
// (chat and phone link previews prefetch links). It only shows one button that POSTs the
// code. POST /view/once consumes the code (single use, 24 h from issue) and starts the same
// view session as a good POST /view/login: same cookie, same 12 h sliding expiry, with one
// difference: it ends at issue + 24 h at the latest, so a leaked link is good until the
// next day at most. A bad, expired or used code takes the failure path of a wrong token
// (per-address lockout) with the same generic message. Logs and the audit log only say
// issued / used / rejected, never the code.

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	viewOnceFile    = "view-once.json"
	viewOnceTTL     = 24 * time.Hour // the link is good for one day after it is issued
	viewOnceVer     = 1
	viewOnceField   = "c"
	viewOnceOrigin  = "https://127.0.0.1:18765" // default for --origin; set it to the address you open in the browser
	viewOnceCodePat = `^[A-Za-z0-9_-]{43}$`     // base64url (no padding) of 32 bytes
)

var viewOnceCodeRE = regexp.MustCompile(viewOnceCodePat)

type viewOnceRec struct {
	Version int       `json:"version"`
	Code    string    `json:"code_sha256"`
	Issued  time.Time `json:"issued"`
	Expires time.Time `json:"expires"`
}

func viewOncePath() string { return filepath.Join(stateDir(), viewOnceFile) }

// issueViewOnce makes a new code, stores its hash (replacing any older code) and returns it.
func issueViewOnce(path string, now time.Time) (string, error) {
	raw := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, raw); err != nil {
		return "", err
	}
	code := base64.RawURLEncoding.EncodeToString(raw)
	rec := viewOnceRec{Version: viewOnceVer, Code: sha256hex(code), Issued: now.UTC(), Expires: now.Add(viewOnceTTL).UTC()}
	b, _ := json.MarshalIndent(rec, "", "  ")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	if err := writeFile0600(path, append(b, '\n')); err != nil {
		return "", err
	}
	return code, nil
}

// viewOnce checks and consumes codes on the server side.
type viewOnce struct {
	mu   sync.Mutex
	path string
	used map[string]bool // hashes consumed by this process (in case the file could not be removed)
}

func newViewOnce(path string) *viewOnce { return &viewOnce{path: path, used: map[string]bool{}} }

// consume reports whether code is the current, unexpired, unused code and, if so, removes
// it so it cannot be used again. limit is the code's end (issue + 24 h): the session it
// starts never lives past it, so a leaked link is good until the next day at most.
func (vo *viewOnce) consume(code string, now time.Time) (limit time.Time, ok bool) {
	if vo == nil || vo.path == "" || !viewOnceCodeRE.MatchString(code) {
		return time.Time{}, false
	}
	vo.mu.Lock()
	defer vo.mu.Unlock()
	b, err := os.ReadFile(vo.path)
	if err != nil {
		return time.Time{}, false
	}
	var rec viewOnceRec
	if json.Unmarshal(b, &rec) != nil || rec.Version != viewOnceVer || len(rec.Code) != 64 {
		return time.Time{}, false
	}
	h := sha256hex(code)
	if subtle.ConstantTimeCompare([]byte(h), []byte(rec.Code)) != 1 || vo.used[h] {
		return time.Time{}, false
	}
	limit = rec.Issued.Add(viewOnceTTL)
	if rec.Expires.Before(limit) {
		limit = rec.Expires
	}
	if !now.Before(limit) || now.Before(rec.Issued.Add(-time.Minute)) {
		return time.Time{}, false
	}
	vo.used[h] = true
	if err := os.Remove(vo.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		fmt.Fprintln(os.Stderr, "a2: view once: remove used code file:", err)
	}
	return limit, true
}

// --- HTTP ---

// viewAlreadyIn redirects a request that already carries a valid view session to /view,
// leaving the code alone (not consumed, no failure counted). Same check as viewSession.
func (sv *server) viewAlreadyIn(w http.ResponseWriter, r *http.Request) bool {
	if !sv.viewLoggedIn(w, r) {
		return false
	}
	fmt.Fprintln(os.Stderr, "a2: view once: already logged in")
	http.Redirect(w, r, "/view", http.StatusSeeOther)
	return true
}

// handleViewOncePage: GET /view/once. Never consumes or checks the code; a code of the
// right shape goes into a hidden field of a one-button form.
func (sv *server) handleViewOncePage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store") // Back must re-request, so the redirect below applies
	if sv.viewAlreadyIn(w, r) {
		return
	}
	code := r.URL.Query().Get(viewOnceField)
	if !viewOnceCodeRE.MatchString(code) {
		writeHTML(w, http.StatusBadRequest, loginPage("Login failed."))
		return
	}
	writeHTML(w, http.StatusOK, oncePage(code))
}

// handleViewOnceLogin: POST /view/once, form field c. Same lockout, rate limit and session
// as handleViewLogin.
func (sv *server) handleViewOnceLogin(w http.ResponseWriter, r *http.Request) {
	if sv.viewAlreadyIn(w, r) {
		return
	}
	now, ip := sv.now(), remoteIP(r)
	if sv.view.locked(ip, now) || !sv.view.limit.allow(ip, now) {
		sv.audit.write(auditEntry{Member: viewerName, Remote: ip, Action: "view-login-locked", Status: 429})
		w.Header().Set("Retry-After", strconv.Itoa(int(viewLockFor.Seconds())))
		writeHTML(w, http.StatusTooManyRequests, loginPage("Too many attempts. Try again later."))
		return
	}
	ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	r.Body = http.MaxBytesReader(w, r.Body, viewMaxLoginBody)
	code := ""
	if ct == "application/x-www-form-urlencoded" && r.ParseForm() == nil {
		code = strings.TrimSpace(r.PostForm.Get(viewOnceField))
	}
	// No viewer token = nothing to bind the session to; the code is left alone then.
	viewer := sv.tokens.viewerHash()
	limit, ok := time.Time{}, false
	if viewer != nil {
		limit, ok = sv.view.once.consume(code, now)
	}
	if !ok {
		sv.view.fail(ip, now)
		sv.audit.write(auditEntry{Member: viewerName, Remote: ip, Action: "view-once-rejected", Status: 401})
		fmt.Fprintln(os.Stderr, "a2: view once: rejected")
		writeHTML(w, http.StatusUnauthorized, loginPage("Login failed."))
		return
	}
	fmt.Fprintln(os.Stderr, "a2: view once: used")
	sv.startViewSession(w, r, ip, now, viewer, limit, "view-once-used")
}

// oncePage: code has passed viewOnceCodeRE, so it holds no HTML special characters.
func oncePage(code string) string {
	return `<!doctype html>
<html lang="ko"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<meta name="referrer" content="no-referrer"><title>AgentsAgora (A2)</title><link rel="stylesheet" href="/view/app.css"></head>
<body class="login"><main><h1>AgentsAgora (A2)</h1>
<form method="post" action="/view/once" autocomplete="off">
<input type="hidden" name="` + viewOnceField + `" value="` + code + `">
<button type="submit">Log in</button>
</form></main></body></html>
`
}

// --- command: agentsagora view-once dm ---

func viewOnceMessage(origin, code string) string {
	return "a2 login link (valid 24 hours, single use): " + strings.TrimRight(origin, "/") + "/view/once?" + viewOnceField + "=" + code
}

// cmdViewOnce: `view-once dm --notify-command CMD [--dry-run] [--origin URL]`. Issues a code and sends the link
// as one message to the notify command. The code goes only into that pipe.
func cmdViewOnce(args []string) error {
	if len(args) == 0 || args[0] != "dm" {
		return usageError("view-once: dm --notify-command CMD [--dry-run] [--origin URL]")
	}
	fs := flag.NewFlagSet("view-once dm", flag.ContinueOnError)
	dry := fs.Bool("dry-run", false, "print the message with <code> in place of the code; issue nothing, send nothing")
	origin := fs.String("origin", viewOnceOrigin, "view origin used in the link")
	notify := fs.String("notify-command", "", "command (split on spaces, no shell) that gets the message on stdin and delivers it to the owner; required unless --dry-run")
	if err := fs.Parse(args[1:]); err != nil {
		return usageError(err.Error())
	}
	if !strings.HasPrefix(*origin, "https://") {
		return usageError("--origin must start with https://")
	}
	if *dry {
		fmt.Println(viewOnceMessage(*origin, "<code>"))
		return nil
	}
	argv := strings.Fields(*notify)
	if len(argv) == 0 {
		return usageError("view-once dm needs --notify-command (or --dry-run)")
	}
	return viewOnceDM(viewOncePath(), *origin, time.Now(), func(msg string) error {
		c := exec.Command(argv[0], argv[1:]...)
		c.Stdin = strings.NewReader(msg + "\n")
		c.Stdout, c.Stderr = os.Stderr, os.Stderr // the command may print status lines; keep stdout clean
		return c.Run()
	})
}

// viewOnceDM issues a code and hands the message to send. When sending fails the code is
// withdrawn (the file removed) so no undelivered code stays valid.
func viewOnceDM(path, origin string, now time.Time, send func(string) error) error {
	code, err := issueViewOnce(path, now)
	if err != nil {
		return fmt.Errorf("view once: issue: %w", err)
	}
	if err := send(viewOnceMessage(origin, code)); err != nil {
		os.Remove(path)
		return fmt.Errorf("view once: notify failed (code withdrawn): %w", err)
	}
	fmt.Fprintf(os.Stderr, "a2: view once: issued (expires %s), notification sent\n", now.Add(viewOnceTTL).In(jst).Format(time.RFC3339))
	return nil
}
