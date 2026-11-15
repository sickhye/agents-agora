package main

// Browser view for the owner (viewer identity "viewer").
//
// viewer is not a member: it is not in the member table, /members, recipient checks or agent
// auth (tokenStore.member never returns it). Its token only works on POST /view/login, which
// starts a server-side session (random cookie, 12 h, sliding, kept across restarts: viewsess.go). The view shows every message in both
// channels, DMs and files included. POST /view/send sends a text message as viewer (to all or one
// member, either channel; CSRF token per session, general lint and rate limit as for members).
// Agents see from=viewer; viewer is never a recipient.
//
// Browser rules: agent API routes still refuse any Origin (server.go auth). /view* routes
// accept only the configured same origin (or https://localhost:<port> from a loopback peer,
// viewlocal.go), check the Host header, require the Origin of that Host on POST (CSRF,
// together with SameSite=Strict), and send a strict CSP with no inline script.

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log"
	"mime"
	"net/http"
	"net/netip"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	viewerName        = "viewer"
	viewCookie        = "__Host-a2_view"
	viewSessionTTL    = 12 * time.Hour
	viewMaxSessions   = 32
	viewLockFails     = 5                // bad logins per source address before the lockout
	viewLockFor       = 15 * time.Minute // lockout (and failure window) length
	viewLoginBurst    = 10               // login attempts per source address at once
	viewLoginPerMin   = 6                // then per minute
	viewMaxLoginBody  = 4 << 10
	viewMaxMessages   = 2000 // messages per /view/api/messages response (newest kept)
	viewCSP           = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self'; connect-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'; object-src 'none'"
	viewDownloadCSP   = "default-src 'none'; sandbox"
	viewDefaultAllow  = "127.0.0.0/8"
	viewLoginFormName = "token"
)

// viewState holds the view sessions and the login lockout table.
type viewState struct {
	mu       sync.Mutex
	sessions map[string]viewSession // sha256(cookie value) hex -> session (viewsess.go)
	path     string                 // session file; "" keeps sessions in memory only
	fails    map[string]*loginFail  // source address -> failures
	limit    *limiter               // login attempts per source address
	once     *viewOnce              // one-time login codes (viewonce.go)
}

type loginFail struct {
	n     int
	first time.Time
	until time.Time
}

// newViewState loads the sessions saved in path ("" = memory only), drops expired ones and
// rewrites the file.
func newViewState(path string, now time.Time) (*viewState, error) {
	ss, err := loadViewSessions(path, now)
	if err != nil {
		return nil, err
	}
	vs := &viewState{sessions: ss, path: path, fails: map[string]*loginFail{}, limit: newLimiter(viewLoginBurst, viewLoginPerMin)}
	if path != "" {
		vs.once = newViewOnce(filepath.Join(filepath.Dir(path), viewOnceFile))
	}
	if path != "" {
		vs.mu.Lock()
		vs.saveLocked()
		vs.mu.Unlock()
	}
	return vs, nil
}

func sessionKey(v string) string { s := sha256.Sum256([]byte(v)); return hex.EncodeToString(s[:]) }

func (vs *viewState) locked(ip string, now time.Time) bool {
	vs.mu.Lock()
	defer vs.mu.Unlock()
	f, ok := vs.fails[ip]
	return ok && now.Before(f.until)
}

// fail records a bad login; the 5th within 15 minutes locks the address for 15 minutes.
func (vs *viewState) fail(ip string, now time.Time) {
	vs.mu.Lock()
	defer vs.mu.Unlock()
	f, ok := vs.fails[ip]
	if !ok || now.Sub(f.first) >= viewLockFor {
		f = &loginFail{first: now}
		vs.fails[ip] = f
	}
	f.n++
	if f.n >= viewLockFails {
		f.until = now.Add(viewLockFor)
	}
}

func (vs *viewState) ok(ip string) {
	vs.mu.Lock()
	delete(vs.fails, ip)
	vs.mu.Unlock()
}

// --- routing and checks ---

func (sv *server) viewRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /view", sv.viewGuard(sv.handleViewPage))
	mux.HandleFunc("GET /view/app.js", sv.viewGuard(staticHandler("text/javascript; charset=utf-8", viewJS)))
	mux.HandleFunc("GET /view/app.css", sv.viewGuard(staticHandler("text/css; charset=utf-8", viewCSS)))
	mux.HandleFunc("POST /view/login", sv.viewGuard(sv.handleViewLogin))
	mux.HandleFunc("GET /view/once", sv.viewGuard(sv.handleViewOncePage))
	mux.HandleFunc("POST /view/once", sv.viewGuard(sv.handleViewOnceLogin))
	mux.HandleFunc("POST /view/logout", sv.viewGuard(sv.handleViewLogout))
	mux.HandleFunc("GET /view/api/messages", sv.viewGuard(sv.viewSession(sv.handleViewMessages)))
	mux.HandleFunc("POST /view/send", sv.viewGuard(sv.viewSession(sv.handleViewSend)))
	mux.HandleFunc("POST /view/rename", sv.viewGuard(sv.viewSession(sv.handleViewRename)))
	mux.HandleFunc("GET /view/files/{id}", sv.viewGuard(sv.viewSession(sv.handleViewFile)))
	mux.HandleFunc("GET /view/ws", sv.viewGuard(sv.viewSession(sv.handleViewWS)))
	mux.HandleFunc("/view/", sv.viewGuard(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
}

// viewGuard sets the view headers and applies the source, Host and Origin rules.
func (sv *server) viewGuard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", viewCSP)
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Cache-Control", "no-store")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		// With the loopback view on (viewlocal.go), a loopback peer is admitted besides --view-allow.
		if len(sv.viewAllow) > 0 && !(len(sv.viewLocal) > 0 && isLoopbackPeer(r)) {
			ip, err := netip.ParseAddr(remoteIP(r))
			if err != nil || !slices.ContainsFunc(sv.viewAllow, func(p netip.Prefix) bool { return p.Contains(ip.Unmap()) }) {
				http.Error(w, "source address not allowed for the view", http.StatusForbidden)
				return
			}
		}
		// want: the origin of the Host used (LAN, or loopback from a loopback peer only);
		// Origin must equal it, so a page on one host never posts to the other.
		want := sv.viewOriginFor(r)
		if sv.viewOrigin != "" && want == "" {
			http.Error(w, "open the view at "+sv.viewOrigin+"/view", http.StatusForbidden)
			return
		}
		origin := r.Header.Get("Origin")
		// Android Chrome sends Origin "null" on the one-time login form after a certificate
		// warning was accepted. Only POST /view/once takes it: the one-time
		// code is the credential, and a request the browser marks cross-site is still refused.
		nullOnce := origin == "null" && r.Method == http.MethodPost && r.URL.Path == "/view/once" &&
			sv.viewOrigin != "" && r.Header.Get("Sec-Fetch-Site") != "cross-site"
		if origin != "" && !nullOnce && (want == "" || origin != want) {
			log.Printf("view refused: %s %s from %s origin=%q", r.Method, r.URL.Path, remoteIP(r), origin)
			http.Error(w, "cross-origin request refused", http.StatusForbidden)
			return
		}
		if r.Method == http.MethodPost && (origin == "" || r.Header.Get("Sec-Fetch-Site") == "cross-site") {
			log.Printf("view refused: %s %s from %s origin=%q sec-fetch-site=%q", r.Method, r.URL.Path, remoteIP(r), origin, r.Header.Get("Sec-Fetch-Site"))
			http.Error(w, "cross-origin request refused", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

func cookieValue(r *http.Request) string {
	c, err := r.Cookie(viewCookie)
	if err != nil {
		return ""
	}
	return c.Value
}

// viewLoggedIn checks the session and, when it was renewed, resends the cookie with the
// new Max-Age so the browser keeps it as long as the server does.
func (sv *server) viewLoggedIn(w http.ResponseWriter, r *http.Request) bool {
	now := sv.now()
	v := cookieValue(r)
	ok, renewed := sv.view.check(v, sv.tokens.viewerHash(), now)
	if ok && !renewed.IsZero() {
		sv.setViewCookie(w, v, int(renewed.Sub(now).Seconds()))
	}
	return ok
}

func (sv *server) viewSession(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !sv.viewLoggedIn(w, r) {
			jsonError(w, http.StatusUnauthorized, "log in at /view")
			return
		}
		next(w, r)
	}
}

func staticHandler(ctype, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", ctype)
		io.WriteString(w, body)
	}
}

func writeHTML(w http.ResponseWriter, code int, body string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(code)
	io.WriteString(w, body)
}

// --- handlers ---

func (sv *server) handleViewPage(w http.ResponseWriter, r *http.Request) {
	if sv.viewLoggedIn(w, r) {
		writeHTML(w, http.StatusOK, viewAppHTML)
		return
	}
	writeHTML(w, http.StatusOK, loginPage(""))
}

func (sv *server) setViewCookie(w http.ResponseWriter, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{Name: viewCookie, Value: value, Path: "/", MaxAge: maxAge,
		HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode})
}

func (sv *server) handleViewLogin(w http.ResponseWriter, r *http.Request) {
	now, ip := sv.now(), remoteIP(r)
	if sv.view.locked(ip, now) || !sv.view.limit.allow(ip, now) {
		sv.audit.write(auditEntry{Member: viewerName, Remote: ip, Action: "view-login-locked", Status: 429})
		w.Header().Set("Retry-After", strconv.Itoa(int(viewLockFor.Seconds())))
		writeHTML(w, http.StatusTooManyRequests, loginPage("Too many attempts. Try again later."))
		return
	}
	ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	r.Body = http.MaxBytesReader(w, r.Body, viewMaxLoginBody)
	tok := ""
	if ct == "application/x-www-form-urlencoded" && r.ParseForm() == nil {
		tok = strings.TrimSpace(r.PostForm.Get(viewLoginFormName))
	}
	viewer := sv.tokens.viewerHash()
	if !sv.tokens.isViewer(tok) {
		sv.view.fail(ip, now)
		sv.audit.write(auditEntry{Member: viewerName, Remote: ip, Action: "view-login-fail", Status: 401})
		writeHTML(w, http.StatusUnauthorized, loginPage("Login failed."))
		return
	}
	sv.startViewSession(w, r, ip, now, viewer, time.Time{}, "view-login")
}

// startViewSession ends the failure count of ip, rotates the session, sets the cookie and
// redirects to /view. Shared by the token login and the one-time link (viewonce.go); limit
// is an absolute end (zero = the usual sliding session only).
func (sv *server) startViewSession(w http.ResponseWriter, r *http.Request, ip string, now time.Time, viewer []byte, limit time.Time, action string) {
	sv.view.ok(ip)
	sv.view.drop(cookieValue(r)) // rotate: the old session (if any) ends here
	v, err := sv.view.newSessionUntil(viewer, now, limit)
	if err != nil {
		writeErr(w, err)
		return
	}
	age := viewSessionTTL
	if !limit.IsZero() && limit.Sub(now) < age {
		age = limit.Sub(now)
	}
	sv.setViewCookie(w, v, int(age.Seconds()))
	sv.audit.write(auditEntry{Member: viewerName, Remote: ip, Action: action, Status: 303})
	http.Redirect(w, r, "/view", http.StatusSeeOther)
}

func (sv *server) handleViewLogout(w http.ResponseWriter, r *http.Request) {
	sv.view.drop(cookieValue(r))
	sv.setViewCookie(w, "", -1)
	sv.audit.write(auditEntry{Member: viewerName, Remote: remoteIP(r), Action: "view-logout", Status: 303})
	http.Redirect(w, r, "/view", http.StatusSeeOther)
}

type viewMsg struct {
	Message
	File *FileMeta `json:"file,omitempty"`
}

// handleViewMessages: every message (both channels, DMs and files) with id > since; the
// newest viewMaxMessages when there are more ("truncated"). With before=N instead: the newest
// viewMaxMessages with id < N, for loading older history ("truncated" = still older ones).
func (sv *server) handleViewMessages(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var since, before int64
	for _, p := range []struct {
		name string
		dst  *int64
	}{{"since", &since}, {"before", &before}} {
		if v := q.Get(p.name); v != "" {
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil || n < 0 {
				jsonError(w, http.StatusBadRequest, "bad "+p.name)
				return
			}
			*p.dst = n
		}
	}
	if since > 0 && before > 0 {
		jsonError(w, http.StatusBadRequest, "give since or before, not both")
		return
	}
	rseq := -1 // with rseq=N, receipts changed after sequence N are added too (receipts.go viewReceipts)
	if v := q.Get("rseq"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			jsonError(w, http.StatusBadRequest, "bad rseq")
			return
		}
		rseq = n
	}
	var all []Message
	if before > 0 {
		all = sv.store.before(before)
	} else {
		all = sv.store.after(since)
	}
	if tv := q.Get("tag"); tv != "" { // only messages with any of these tags
		tl, err := parseTagFilter(splitTags(tv))
		if err != nil {
			writeErr(w, err)
			return
		}
		kept := make([]Message, 0, len(all))
		for _, m := range all {
			if hasAnyTag(m, tl) {
				kept = append(kept, m)
			}
		}
		all = kept
	}
	truncated := len(all) > viewMaxMessages
	if truncated {
		all = all[len(all)-viewMaxMessages:]
	}
	nt := currentNames()
	out := make([]viewMsg, 0, len(all))
	for _, m := range all {
		vm := viewMsg{Message: nt.showMsg(m)}
		if m.FileID != "" {
			if fm, ok := sv.files.meta(m.FileID); ok {
				vm.File = &fm
			}
		}
		out = append(out, vm)
	}
	mc := map[string][]string{}
	names := sv.memberNames(nt)
	for _, m := range names {
		mc[m] = nt.channels(m)
	}
	alive := map[string]bool{} // liveness dot; members never seen are left out
	now := sv.now()
	for _, b := range sv.members {
		if lv := sv.liveOf(nt, b, now); lv.seen {
			alive[nt.current(b)] = lv.alive
		}
	}
	rcpts, badge, rseqNow := sv.viewReceipts(all, rseq, nt)
	writeJSON(w, http.StatusOK, map[string]any{"messages": out, "truncated": truncated,
		"receipts": rcpts, "undelivered": badge, "rseq": rseqNow,
		"votes":   sv.viewVotes(), // every vote by message id, counts so far (vote.go)
		"members": names, "member_channels": mc, "alive": alive, "channels": allChannels, "channel_info": channelDefs, "server": sv.serverStatus(),
		"csrf": sv.view.csrfOf(cookieValue(r))})
}

// handleViewSend: POST /view/send, form fields csrf, channel, to, text, optional reply_to. The message is sent
// as viewer on either channel, to all or to one member; the general lint and the send rate
// limit apply as for members. viewer itself stays out of /members and cannot be a recipient.
func (sv *server) handleViewSend(w http.ResponseWriter, r *http.Request) {
	ip := remoteIP(r)
	ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	if ct != "application/x-www-form-urlencoded" || r.ParseForm() != nil {
		jsonError(w, http.StatusBadRequest, "form body required")
		return
	}
	f := r.PostForm
	if !sv.view.csrfOK(cookieValue(r), f.Get("csrf")) {
		sv.audit.write(auditEntry{Member: viewerName, Remote: ip, Action: "view-send", Channel: f.Get("channel"), Status: 403, Target: f.Get("to")})
		jsonError(w, http.StatusForbidden, "bad csrf token (reload the page)")
		return
	}
	ch, to, text := f.Get("channel"), f.Get("to"), f.Get("text")
	var replyTo int64
	if v := f.Get("reply_to"); v != "" {
		n, perr := strconv.ParseInt(v, 10, 64)
		if perr != nil || n < 0 {
			jsonError(w, http.StatusBadRequest, "bad reply_to")
			return
		}
		replyTo = n
	}
	var m Message
	var err error
	switch {
	case !slices.Contains(allChannels, ch):
		err = errCode(http.StatusBadRequest, "unknown channel %q (channels: %s)", ch, strings.Join(allChannels, ", "))
	case to == "":
		err = errCode(http.StatusBadRequest, "unknown recipient \"\"")
	default:
		m, err = sv.sendOn(viewerName, ch, to, text, replyTo)
	}
	sv.audit.write(auditEntry{Member: viewerName, Remote: ip, Action: "view-send", Channel: ch, Size: int64(len(text)), Status: statusOf(err), Target: to})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

func (sv *server) handleViewFile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	target := id
	if !fileIDRe.MatchString(id) {
		target = "(bad id)"
	}
	fm, ok := sv.files.meta(id)
	var data []byte
	var err error
	if ok {
		data, err = sv.files.read(id)
	}
	status := 200
	if !ok || err != nil {
		status = 404
	}
	sv.audit.write(auditEntry{Member: viewerName, Remote: remoteIP(r), Action: "view-get_file", Channel: fm.Channel, Size: int64(len(data)), Status: status, Target: target})
	if status != 200 {
		http.Error(w, "no such file", http.StatusNotFound)
		return
	}
	h := w.Header()
	h.Set("Content-Security-Policy", viewDownloadCSP)
	h.Set("Content-Type", fm.MIME)
	h.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": fm.Name}))
	h.Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(http.StatusOK)
	w.Write(data)
}

// before returns a copy of every message with id < id.
func (s *store) before(id int64) []Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Message{}
	for _, m := range s.msgs {
		if m.ID >= id {
			break
		}
		out = append(out, m)
	}
	return out
}

// after returns a copy of every message with id > since.
func (s *store) after(since int64) []Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Message{}
	for _, m := range s.msgs {
		if m.ID > since {
			out = append(out, m)
		}
	}
	return out
}

// --- embedded page, style and script (no external resources, no inline script) ---

// loginPage takes only fixed strings from this file; it never echoes request data.
func loginPage(msg string) string {
	note := ""
	if msg != "" {
		note = `<p class="err">` + msg + `</p>`
	}
	return `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<meta name="referrer" content="same-origin"><title>AgentsAgora (A2)</title><link rel="stylesheet" href="/view/app.css"></head>
<body class="login"><main><h1>AgentsAgora (A2)</h1>` + note + `
<form method="post" action="/view/login" autocomplete="off">
<label for="token">Viewer token</label>
<input id="token" name="token" type="password" required autofocus>
<button type="submit">Log in</button>
</form></main></body></html>
`
}
