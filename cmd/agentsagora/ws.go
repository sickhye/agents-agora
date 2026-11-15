package main

// Server push over WebSocket (RFC 6455 subset, stdlib only).
//
// GET /ws (agents, Authorization: Bearer, never a token in the URL) and GET /view/ws (the
// viewer's page, session cookie + same Origin + CSRF token in the first frame) upgrade to a
// WebSocket on the same TLS port. The server sends one text frame per message, a JSON
// object of the same shape as an /inbox item (/view/ws: as a /view/api/messages item).
// The client sends {"since": N} (and on /view/ws also "csrf") right after the upgrade;
// the server then sends every readable message with id > N and each new one as it is
// stored. Without a since frame within wsSinceWait the server starts at the newest id.
// A later since frame restarts from that id (the client drops ids it already has).
//
// Subset: no extensions, no subprotocols; text, close, ping and pong frames; fragmented
// client frames are joined up to wsMaxInFrame. Client frames must be masked, server
// frames are not. The server pings every 25 s and closes a socket that has sent nothing
// (no pong) for 60 s. At most wsMaxPerMember sockets per member (the viewer counts as one
// member). On shutdown or self-update the server sends a close frame (1001 going away,
// 1012 service restart) and waits up to shutdownGrace for the sockets to end.

import (
	"bufio"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	wsGUID         = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
	wsMaxPerMember = 8
	wsMaxInFrame   = 4 << 10 // client -> server message limit (since/csrf frames are tiny)
	wsMaxOutFrame  = 1 << 20 // server -> client message limit as read by our client
	wsPingEvery    = 25 * time.Second
	wsPongWait     = 60 * time.Second
	wsSinceWait    = 3 * time.Second
	wsWriteWait    = 10 * time.Second

	wsOpCont   = 0x0
	wsOpText   = 0x1
	wsOpBinary = 0x2
	wsOpClose  = 0x8
	wsOpPing   = 0x9
	wsOpPong   = 0xA

	wsCloseNormal      = 1000
	wsCloseGoingAway   = 1001
	wsCloseProtocol    = 1002
	wsCloseUnsupported = 1003
	wsClosePolicy      = 1008
	wsCloseTooBig      = 1009
	wsCloseRestart     = 1012
)

// wsCloseError is a close frame received from the peer, or a protocol error to report
// with a close code.
type wsCloseError struct {
	code   int
	reason string
}

func (e *wsCloseError) Error() string {
	return fmt.Sprintf("websocket closed (%d %s)", e.code, e.reason)
}

// wsConnFrames reads and writes frames on one connection. Writes are serialised.
type wsConnFrames struct {
	c      net.Conn
	r      *bufio.Reader
	wmu    sync.Mutex
	client bool  // client side: mask outgoing frames, expect unmasked ones
	maxIn  int64 // limit for one (joined) data message
	closed atomic.Bool
}

func wsAcceptKey(key string) string {
	h := sha1.Sum([]byte(key + wsGUID))
	return base64.StdEncoding.EncodeToString(h[:])
}

func (w *wsConnFrames) writeFrame(op byte, p []byte) error {
	w.wmu.Lock()
	defer w.wmu.Unlock()
	hdr := make([]byte, 0, 14)
	hdr = append(hdr, 0x80|op)
	var mbit byte
	if w.client {
		mbit = 0x80
	}
	switch n := len(p); {
	case n <= 125:
		hdr = append(hdr, mbit|byte(n))
	case n <= 0xFFFF:
		hdr = append(hdr, mbit|126)
		hdr = binary.BigEndian.AppendUint16(hdr, uint16(n))
	default:
		hdr = append(hdr, mbit|127)
		hdr = binary.BigEndian.AppendUint64(hdr, uint64(n))
	}
	body := p
	if w.client {
		var key [4]byte
		if _, err := io.ReadFull(rand.Reader, key[:]); err != nil {
			return err
		}
		hdr = append(hdr, key[:]...)
		body = make([]byte, len(p))
		for i := range p {
			body[i] = p[i] ^ key[i&3]
		}
	}
	w.c.SetWriteDeadline(time.Now().Add(wsWriteWait))
	if _, err := w.c.Write(append(hdr, body...)); err != nil {
		return err
	}
	return nil
}

// writeClose sends a close frame (once); the caller closes the connection.
func (w *wsConnFrames) writeClose(code int, reason string) error {
	if w.closed.Swap(true) {
		return nil
	}
	p := binary.BigEndian.AppendUint16(nil, uint16(code))
	if len(reason) > 120 {
		reason = reason[:120]
	}
	return w.writeFrame(wsOpClose, append(p, reason...))
}

// readFrame returns the next control frame, or the next complete data message (fragments
// joined). Protocol errors come back as *wsCloseError with the code to send.
func (w *wsConnFrames) readFrame() (byte, []byte, error) {
	var msgOp byte
	var msg []byte
	for {
		var h [2]byte
		if _, err := io.ReadFull(w.r, h[:]); err != nil {
			return 0, nil, err
		}
		fin, op := h[0]&0x80 != 0, h[0]&0x0F
		if h[0]&0x70 != 0 {
			return 0, nil, &wsCloseError{wsCloseProtocol, "reserved bits set"}
		}
		masked := h[1]&0x80 != 0
		if masked == w.client {
			return 0, nil, &wsCloseError{wsCloseProtocol, "bad masking"}
		}
		n := int64(h[1] & 0x7F)
		switch n {
		case 126:
			var b [2]byte
			if _, err := io.ReadFull(w.r, b[:]); err != nil {
				return 0, nil, err
			}
			n = int64(binary.BigEndian.Uint16(b[:]))
		case 127:
			var b [8]byte
			if _, err := io.ReadFull(w.r, b[:]); err != nil {
				return 0, nil, err
			}
			u := binary.BigEndian.Uint64(b[:])
			if u > 1<<40 {
				return 0, nil, &wsCloseError{wsCloseTooBig, "frame too big"}
			}
			n = int64(u)
		}
		control := op >= 0x8
		if control && (!fin || n > 125) {
			return 0, nil, &wsCloseError{wsCloseProtocol, "bad control frame"}
		}
		if !control && int64(len(msg))+n > w.maxIn {
			return 0, nil, &wsCloseError{wsCloseTooBig, "message too big"}
		}
		var key [4]byte
		if masked {
			if _, err := io.ReadFull(w.r, key[:]); err != nil {
				return 0, nil, err
			}
		}
		p := make([]byte, n)
		if _, err := io.ReadFull(w.r, p); err != nil {
			return 0, nil, err
		}
		if masked {
			for i := range p {
				p[i] ^= key[i&3]
			}
		}
		switch {
		case control:
			if op != wsOpClose && op != wsOpPing && op != wsOpPong {
				return 0, nil, &wsCloseError{wsCloseProtocol, "unknown opcode"}
			}
			return op, p, nil
		case op == wsOpCont:
			if msgOp == 0 {
				return 0, nil, &wsCloseError{wsCloseProtocol, "continuation without start"}
			}
		case op == wsOpText || op == wsOpBinary:
			if msgOp != 0 {
				return 0, nil, &wsCloseError{wsCloseProtocol, "new message inside a fragmented one"}
			}
			msgOp = op
		default:
			return 0, nil, &wsCloseError{wsCloseProtocol, "unknown opcode"}
		}
		msg = append(msg, p...)
		if fin {
			return msgOp, msg, nil
		}
	}
}

// closeInfo decodes a close frame payload.
func closeInfo(p []byte) *wsCloseError {
	if len(p) < 2 {
		return &wsCloseError{1005, ""}
	}
	return &wsCloseError{int(binary.BigEndian.Uint16(p)), string(p[2:])}
}

// headerHas reports whether a comma-separated header contains token (case-insensitive).
func headerHas(h http.Header, name, token string) bool {
	for _, v := range h.Values(name) {
		for _, p := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(p), token) {
				return true
			}
		}
	}
	return false
}

// wsUpgrade checks the handshake and hijacks the connection. On error it has already
// written the HTTP answer.
func wsUpgrade(w http.ResponseWriter, r *http.Request) (*wsConnFrames, error) {
	if !headerHas(r.Header, "Connection", "upgrade") || !headerHas(r.Header, "Upgrade", "websocket") {
		w.Header().Set("Upgrade", "websocket")
		jsonError(w, http.StatusUpgradeRequired, "websocket upgrade required")
		return nil, errors.New("not an upgrade")
	}
	if r.Header.Get("Sec-WebSocket-Version") != "13" {
		w.Header().Set("Sec-WebSocket-Version", "13")
		jsonError(w, http.StatusUpgradeRequired, "websocket version 13 required")
		return nil, errors.New("bad version")
	}
	key := strings.TrimSpace(r.Header.Get("Sec-WebSocket-Key"))
	if b, err := base64.StdEncoding.DecodeString(key); err != nil || len(b) != 16 {
		jsonError(w, http.StatusBadRequest, "bad Sec-WebSocket-Key")
		return nil, errors.New("bad key")
	}
	conn, brw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		jsonError(w, http.StatusHTTPVersionNotSupported, "websocket needs HTTP/1.1")
		return nil, err
	}
	conn.SetDeadline(time.Time{}) // drop the HTTP server's read/write timeouts
	resp := "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: " + wsAcceptKey(key) + "\r\n\r\n"
	conn.SetWriteDeadline(time.Now().Add(wsWriteWait))
	if _, err := brw.WriteString(resp); err == nil {
		err = brw.Flush()
	}
	if err != nil {
		conn.Close()
		return nil, err
	}
	return &wsConnFrames{c: conn, r: brw.Reader, maxIn: wsMaxInFrame}, nil
}

// wsHub counts sockets per member and tracks them for shutdown.
type wsHub struct {
	mu    sync.Mutex
	count map[string]int
	conns map[*wsConnFrames]struct{}
	wg    sync.WaitGroup
}

func newWSHub() *wsHub {
	return &wsHub{count: map[string]int{}, conns: map[*wsConnFrames]struct{}{}}
}

func (h *wsHub) acquire(member string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.count[member] >= wsMaxPerMember {
		return false
	}
	h.count[member]++
	h.wg.Add(1)
	return true
}

func (h *wsHub) release(member string) {
	h.mu.Lock()
	if h.count[member]--; h.count[member] <= 0 {
		delete(h.count, member)
	}
	h.mu.Unlock()
	h.wg.Done()
}

func (h *wsHub) track(c *wsConnFrames, on bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if on {
		h.conns[c] = struct{}{}
	} else {
		delete(h.conns, c)
	}
}

// dropAll closes every socket without a close frame (last step of a shutdown that ran out
// of time; tests use it to simulate a network drop).
func (h *wsHub) dropAll() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.conns {
		c.c.Close()
	}
}

// wait waits for all sockets to end, up to d; then drops the rest.
func (h *wsHub) wait(d time.Duration) {
	done := make(chan struct{})
	go func() { h.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(d):
		h.dropAll()
		<-done
	}
}

// wsItem is one pushed message: its id (for the cursor) and the JSON body.
type wsItem struct {
	id   int64
	body any
}

// wsSession describes one socket for wsServe.
type wsSession struct {
	member string
	feed   func(since int64) ([]wsItem, <-chan struct{}) // items after since, and the next-append channel
	alive  func() bool                                   // re-checked at every ping (token or session still valid)
	csrf   func(tok string) bool                         // nil: no CSRF; else the first frame must carry a valid one
	sent   func(ids []int64)                             // nil, or told the ids of the item frames written (delivery receipts)
	wake   func() <-chan struct{}                        // nil, or a channel closed on a receipt change; then {"receipts":true} is sent
}

type wsClientFrame struct {
	Since *int64 `json:"since"`
	CSRF  string `json:"csrf,omitempty"`
}

// wsServe runs one socket until the client leaves, a check fails or the server stops.
func (sv *server) wsServe(fc *wsConnFrames, s wsSession) {
	sv.ws.track(fc, true)
	defer sv.ws.track(fc, false)
	defer fc.c.Close()
	var lastSeen atomic.Int64
	lastSeen.Store(time.Now().UnixNano())
	sinceCh := make(chan wsClientFrame, 1)
	readDone := make(chan error, 1)
	go func() {
		for {
			op, p, err := fc.readFrame()
			if err != nil {
				readDone <- err
				return
			}
			lastSeen.Store(time.Now().UnixNano())
			switch op {
			case wsOpPing:
				fc.writeFrame(wsOpPong, p)
			case wsOpPong:
			case wsOpClose:
				readDone <- closeInfo(p)
				return
			case wsOpBinary:
				readDone <- &wsCloseError{wsCloseUnsupported, "text frames only"}
				return
			case wsOpText:
				var f wsClientFrame
				if json.Unmarshal(p, &f) != nil || (f.Since != nil && *f.Since < 0) {
					readDone <- &wsCloseError{wsClosePolicy, "bad frame (want {\"since\": N})"}
					return
				}
				select { // keep only the newest
				case <-sinceCh:
				default:
				}
				sinceCh <- f
			}
		}
	}()
	finish := func(code int, reason string) {
		fc.writeClose(code, reason)
		fc.c.SetReadDeadline(time.Now().Add(time.Second))
		select { // let the client answer the close
		case <-readDone:
		case <-time.After(time.Second):
		}
	}
	// handle a read-side end: answer a close, or report a protocol error
	readEnd := func(err error) {
		var ce *wsCloseError
		if errors.As(err, &ce) {
			if ce.code == 1005 {
				ce.code = wsCloseNormal
			}
			fc.writeClose(ce.code, "")
		}
	}

	since := int64(-1)
	t := time.NewTimer(wsSinceWait)
	for since < 0 {
		select {
		case f := <-sinceCh:
			if s.csrf != nil && !s.csrf(f.CSRF) {
				finish(wsClosePolicy, "bad csrf token")
				return
			}
			if f.Since != nil {
				since = *f.Since
			} else {
				since = sv.store.lastID()
			}
		case <-t.C:
			if s.csrf != nil {
				finish(wsClosePolicy, "no csrf frame")
				return
			}
			since = sv.store.lastID()
		case err := <-readDone:
			t.Stop()
			readEnd(err)
			return
		case <-sv.quit:
			finish(sv.closeCode(), "server stopping")
			return
		}
	}
	t.Stop()
	ping := time.NewTicker(sv.wsPing)
	defer ping.Stop()
	for {
		var wake <-chan struct{} // nil blocks forever
		if s.wake != nil {
			wake = s.wake() // taken before the feed, so no change between them is missed
		}
		items, next := s.feed(since)
		var written []int64
		for _, it := range items {
			b, err := json.Marshal(it.body)
			if err != nil {
				continue
			}
			if err := fc.writeFrame(wsOpText, b); err != nil {
				if s.sent != nil {
					s.sent(written)
				}
				return
			}
			written = append(written, it.id)
			since = it.id
		}
		if s.sent != nil {
			s.sent(written)
		}
		select {
		case <-wake:
			if err := fc.writeFrame(wsOpText, []byte(`{"receipts":true}`)); err != nil {
				return
			}
		case <-next:
		case f := <-sinceCh:
			if f.Since != nil {
				since = *f.Since
			}
		case <-ping.C:
			if time.Since(time.Unix(0, lastSeen.Load())) > sv.wsPongWait {
				finish(wsCloseGoingAway, "pong timeout")
				return
			}
			if !s.alive() {
				finish(wsClosePolicy, "token or session no longer valid")
				return
			}
			if err := fc.writeFrame(wsOpPing, nil); err != nil {
				return
			}
		case err := <-readDone:
			readEnd(err)
			return
		case <-sv.quit:
			finish(sv.closeCode(), "server stopping")
			return
		}
	}
}

// closeCode is the close code sent on shutdown: 1012 before a self-update restart.
func (sv *server) closeCode() int {
	if sv.restarting.Load() {
		return wsCloseRestart
	}
	return wsCloseGoingAway
}

// lastID is the newest message id (0 when empty).
func (s *store) lastID() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.msgs) == 0 {
		return 0
	}
	return s.msgs[len(s.msgs)-1].ID
}

// handleWS: GET /ws[?channel=C], agents (bearer token). Pushes what /inbox would return.
func (sv *server) handleWS(w http.ResponseWriter, r *http.Request) {
	member := callerOf(r)
	ip := remoteIP(r)
	tok := bearerToken(r)
	chs, err := sv.readChannels(member, r.URL.Query().Get("channel"))
	if err != nil {
		writeErr(w, err)
		return
	}
	if !sv.ws.acquire(member) {
		sv.audit.write(auditEntry{Member: member, Remote: ip, Action: "ws", Status: 429})
		w.Header().Set("Retry-After", "30")
		jsonError(w, http.StatusTooManyRequests, fmt.Sprintf("too many sockets (max %d per member)", wsMaxPerMember))
		return
	}
	defer sv.ws.release(member)
	fc, err := wsUpgrade(w, r)
	if err != nil {
		return
	}
	sv.audit.write(auditEntry{Member: member, Remote: ip, Action: "ws-open", Status: 101})
	sv.wsServe(fc, wsSession{
		member: member,
		feed: func(since int64) ([]wsItem, <-chan struct{}) {
			ms, next := sv.store.inbox(member, since, chs, currentNames())
			out := make([]wsItem, len(ms))
			for i, m := range ms {
				out[i] = wsItem{m.ID, m}
			}
			return out, next
		},
		alive: func() bool { return sv.tokens.member(tok) == member },
		sent:  func(ids []int64) { sv.deliveredIDs(member, ids) },
	})
	sv.audit.write(auditEntry{Member: member, Remote: ip, Action: "ws-close"})
}

// handleViewWS: GET /view/ws for the viewer's page (session cookie; viewGuard already
// checked source, Host and Origin; here Origin is required, and the first frame must carry
// the session CSRF token). Pushes what /view/api/messages?since= would return.
func (sv *server) handleViewWS(w http.ResponseWriter, r *http.Request) {
	ip := remoteIP(r)
	want := sv.viewOriginFor(r) // the LAN origin, or a loopback origin from a loopback peer (viewlocal.go)
	if want == "" || r.Header.Get("Origin") != want || r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		http.Error(w, "cross-origin request refused", http.StatusForbidden)
		return
	}
	cookie := cookieValue(r)
	if !sv.ws.acquire(viewerName) {
		jsonError(w, http.StatusTooManyRequests, fmt.Sprintf("too many sockets (max %d)", wsMaxPerMember))
		return
	}
	defer sv.ws.release(viewerName)
	fc, err := wsUpgrade(w, r)
	if err != nil {
		return
	}
	sv.audit.write(auditEntry{Member: viewerName, Remote: ip, Action: "view-ws-open", Status: 101})
	sv.wsServe(fc, wsSession{
		member: viewerName,
		feed: func(since int64) ([]wsItem, <-chan struct{}) {
			sv.store.mu.Lock()
			next := sv.store.notify
			sv.store.mu.Unlock()
			all := sv.store.after(since)
			if len(all) > viewMaxMessages {
				all = all[len(all)-viewMaxMessages:]
			}
			nt := currentNames()
			out := make([]wsItem, len(all))
			for i, m := range all {
				vm := viewMsg{Message: nt.showMsg(m)}
				if m.FileID != "" {
					if fm, ok := sv.files.meta(m.FileID); ok {
						vm.File = &fm
					}
				}
				out[i] = wsItem{m.ID, vm}
			}
			return out, next
		},
		alive: func() bool { return sv.view.valid(cookie, sv.tokens.viewerHash(), sv.now()) },
		csrf:  func(tok string) bool { return sv.view.csrfOK(cookie, tok) },
		wake:  sv.deliv.wake,
	})
	sv.audit.write(auditEntry{Member: viewerName, Remote: ip, Action: "view-ws-close"})
}
