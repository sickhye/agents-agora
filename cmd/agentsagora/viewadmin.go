package main

// Viewer UI additions: server version / last update, and member rename.

import (
	"mime"
	"net/http"
)

type serverStatus struct {
	buildInfo
	LastUpdate *lastUpdate `json:"last_update,omitempty"`
}

func (sv *server) serverStatus() serverStatus {
	st := serverStatus{buildInfo: sv.build}
	if lu, ok := readLastUpdate(); ok {
		st.LastUpdate = &lu
	}
	return st
}

// handleViewRename: POST /view/rename, form fields csrf, old, new. Same rules as
// `a2 token rename` (names.go); only the logged-in viewer reaches it.
func (sv *server) handleViewRename(w http.ResponseWriter, r *http.Request) {
	ip := remoteIP(r)
	ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	r.Body = http.MaxBytesReader(w, r.Body, viewMaxLoginBody)
	if ct != "application/x-www-form-urlencoded" || r.ParseForm() != nil {
		jsonError(w, http.StatusBadRequest, "form body required")
		return
	}
	f := r.PostForm
	old, nw := f.Get("old"), f.Get("new")
	target := old + "->" + nw
	if len(target) > 2*maxMemberName+2 {
		target = "(too long)"
	}
	if !sv.view.csrfOK(cookieValue(r), f.Get("csrf")) {
		sv.audit.write(auditEntry{Member: viewerName, Remote: ip, Action: "member-rename", Status: 403, Target: target})
		jsonError(w, http.StatusForbidden, "bad csrf token (reload the page)")
		return
	}
	var err error
	switch {
	case old == viewerName:
		err = errCode(http.StatusBadRequest, "viewer cannot be renamed")
	case !sv.isMember(currentNames(), old):
		err = errCode(http.StatusBadRequest, "unknown member %s", old)
	default:
		err = renameMember(old, nw, viewerName, sv.now())
	}
	sv.audit.write(auditEntry{Member: viewerName, Remote: ip, Action: "member-rename", Status: statusOf(err), Target: target})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"old": old, "new": nw})
}
