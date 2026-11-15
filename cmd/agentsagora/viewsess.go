package main

// Viewer UI sessions, kept in memory and in a state file so a server restart does not log
// the owner out.
//
// The file (stateDir()/view-sessions.json, mode 0600, dir 0700) holds per session only
// sha256(cookie value), sha256(CSRF token), a fingerprint of the viewer token hash at login,
// the login time and the expiry. The raw cookie value and the raw CSRF token are never
// written. The CSRF token is derived from the cookie value (HMAC), so after a restart the
// server can still hand it to the page without storing it.
//
// Lifetime: 12 h from login. A request in the last 6 h moves the expiry to now+12 h, but
// never past login+7 days. Logout deletes the entry; `token rotate viewer` / `token revoke viewer`
// delete the file, and a session whose viewer fingerprint no longer matches is dropped.

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

const (
	viewSessionsFile = "view-sessions.json"
	viewRenewWithin  = 6 * time.Hour      // renew when less than this is left
	viewSessionMax   = 7 * 24 * time.Hour // hard cap from login
	viewSessionsVer  = 1
)

type viewSession struct {
	login    time.Time
	expires  time.Time
	viewerFP string    // viewerFingerprint(viewer token hash at login); a rotated or revoked token ends the session
	csrfHash string    // sha256 hex of the CSRF token
	limit    time.Time // absolute end (zero = none); set for one-time-link sessions (viewonce.go)
}

type viewSessionRec struct {
	ID      string    `json:"id_sha256"`
	CSRF    string    `json:"csrf_sha256"`
	Viewer  string    `json:"viewer_fp"`
	Login   time.Time `json:"login"`
	Expires time.Time `json:"expires"`
	Limit   time.Time `json:"limit,omitzero"`
}

type viewSessionFile struct {
	Version  int              `json:"version"`
	Sessions []viewSessionRec `json:"sessions"`
}

func viewSessionsPath() string { return filepath.Join(stateDir(), viewSessionsFile) }

func sha256hex(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }

// viewerFingerprint binds a session to the viewer token hash without storing that hash.
func viewerFingerprint(viewer []byte) string {
	if viewer == nil {
		return ""
	}
	return sha256hex("a2-view-viewer\x00" + string(viewer))
}

// csrfFor derives the CSRF token of a session from its cookie value.
func csrfFor(v string) string {
	m := hmac.New(sha256.New, []byte(v))
	m.Write([]byte("a2-view-csrf"))
	return hex.EncodeToString(m.Sum(nil))
}

// loadViewSessions reads the session file; a missing file is empty. Expired and malformed
// entries are skipped.
func loadViewSessions(path string, now time.Time) (map[string]viewSession, error) {
	out := map[string]viewSession{}
	if path == "" {
		return out, nil
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	var f viewSessionFile
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	for _, r := range f.Sessions {
		if len(r.ID) != 64 || len(r.CSRF) != 64 || r.Viewer == "" || !now.Before(r.Expires) {
			continue
		}
		if r.Expires.After(r.Login.Add(viewSessionMax)) {
			r.Expires = r.Login.Add(viewSessionMax)
		}
		if !r.Limit.IsZero() && r.Expires.After(r.Limit) {
			r.Expires = r.Limit
		}
		if !now.Before(r.Expires) {
			continue
		}
		out[r.ID] = viewSession{login: r.Login, expires: r.Expires, viewerFP: r.Viewer, csrfHash: r.CSRF, limit: r.Limit}
	}
	return out, nil
}

// saveLocked writes the sessions atomically (tmp + rename, 0600). Caller holds vs.mu.
// A write error is logged; the sessions stay valid in memory.
func (vs *viewState) saveLocked() {
	if vs.path == "" {
		return
	}
	f := viewSessionFile{Version: viewSessionsVer, Sessions: []viewSessionRec{}}
	for k, s := range vs.sessions {
		f.Sessions = append(f.Sessions, viewSessionRec{ID: k, CSRF: s.csrfHash, Viewer: s.viewerFP, Login: s.login.UTC(), Expires: s.expires.UTC(), Limit: utcOrZero(s.limit)})
	}
	b, _ := json.MarshalIndent(f, "", "  ")
	err := os.MkdirAll(filepath.Dir(vs.path), 0o700)
	if err == nil {
		err = writeFile0600(vs.path, append(b, '\n'))
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "a2: save view sessions:", err)
	}
}

// newSession makes a random session, drops expired ones, ones of another viewer token and the
// oldest above the cap, and saves.
func (vs *viewState) newSession(viewer []byte, now time.Time) (string, error) {
	return vs.newSessionUntil(viewer, now, time.Time{})
}

// newSessionUntil is newSession with an absolute end: the session never lives past limit
// (zero = no extra limit). A limit not after now makes no session.
func (vs *viewState) newSessionUntil(viewer []byte, now, limit time.Time) (string, error) {
	if !limit.IsZero() && !now.Before(limit) {
		return "", errors.New("view session limit already passed")
	}
	raw := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, raw); err != nil {
		return "", err
	}
	v := hex.EncodeToString(raw)
	fp := viewerFingerprint(viewer)
	vs.mu.Lock()
	defer vs.mu.Unlock()
	for k, s := range vs.sessions {
		if !now.Before(s.expires) || s.viewerFP != fp {
			delete(vs.sessions, k)
		}
	}
	for len(vs.sessions) >= viewMaxSessions {
		oldest := ""
		for k, s := range vs.sessions {
			if oldest == "" || s.expires.Before(vs.sessions[oldest].expires) {
				oldest = k
			}
		}
		delete(vs.sessions, oldest)
	}
	exp := now.Add(viewSessionTTL)
	if !limit.IsZero() && exp.After(limit) {
		exp = limit
	}
	vs.sessions[sessionKey(v)] = viewSession{login: now, expires: exp, viewerFP: fp, csrfHash: sha256hex(csrfFor(v)), limit: limit}
	vs.saveLocked()
	return v, nil
}

// check reports whether v is a live session of the current viewer token. When less than
// viewRenewWithin is left it moves the expiry (capped at login+viewSessionMax), saves, and
// returns the new expiry as renewed (zero otherwise).
func (vs *viewState) check(v string, viewer []byte, now time.Time) (ok bool, renewed time.Time) {
	if v == "" || viewer == nil {
		return false, time.Time{}
	}
	fp := viewerFingerprint(viewer)
	k := sessionKey(v)
	vs.mu.Lock()
	defer vs.mu.Unlock()
	s, found := vs.sessions[k]
	if !found {
		return false, time.Time{}
	}
	if subtle.ConstantTimeCompare([]byte(s.viewerFP), []byte(fp)) != 1 {
		// The viewer token changed: every session of the old token ends.
		for k2, s2 := range vs.sessions {
			if s2.viewerFP != fp {
				delete(vs.sessions, k2)
			}
		}
		vs.saveLocked()
		return false, time.Time{}
	}
	if !now.Before(s.expires) {
		delete(vs.sessions, k)
		vs.saveLocked()
		return false, time.Time{}
	}
	if s.expires.Sub(now) < viewRenewWithin {
		exp := now.Add(viewSessionTTL)
		if limit := s.login.Add(viewSessionMax); exp.After(limit) {
			exp = limit
		}
		if !s.limit.IsZero() && exp.After(s.limit) {
			exp = s.limit
		}
		if exp.After(s.expires) {
			s.expires = exp
			vs.sessions[k] = s
			vs.saveLocked()
			return true, exp
		}
	}
	return true, time.Time{}
}

func utcOrZero(t time.Time) time.Time {
	if t.IsZero() {
		return t
	}
	return t.UTC()
}

func (vs *viewState) valid(v string, viewer []byte, now time.Time) bool {
	ok, _ := vs.check(v, viewer, now)
	return ok
}

// csrfOf returns the CSRF token of a live session, or "".
func (vs *viewState) csrfOf(v string) string {
	if v == "" {
		return ""
	}
	vs.mu.Lock()
	defer vs.mu.Unlock()
	if _, ok := vs.sessions[sessionKey(v)]; !ok {
		return ""
	}
	return csrfFor(v)
}

// csrfOK reports (in constant time) whether tok is the CSRF token of session v.
func (vs *viewState) csrfOK(v, tok string) bool {
	if v == "" || tok == "" {
		return false
	}
	vs.mu.Lock()
	s, ok := vs.sessions[sessionKey(v)]
	vs.mu.Unlock()
	return ok && subtle.ConstantTimeCompare([]byte(sha256hex(tok)), []byte(s.csrfHash)) == 1
}

func (vs *viewState) drop(v string) {
	if v == "" {
		return
	}
	vs.mu.Lock()
	defer vs.mu.Unlock()
	k := sessionKey(v)
	if _, ok := vs.sessions[k]; ok {
		delete(vs.sessions, k)
		vs.saveLocked()
	}
}

// clearViewSessions deletes the session file (used by `token rotate viewer` / `token revoke viewer`).
// A running server also drops those sessions on their next request (fingerprint mismatch).
func clearViewSessions() error {
	err := os.Remove(viewSessionsPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
