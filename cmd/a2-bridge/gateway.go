package main

// A one-time Discord gateway identify. Discord asks a bot to connect to the gateway and
// identify at least once before it may create messages over REST; the bridge itself only
// uses REST. This is a minimal WebSocket client (RFC 6455, stdlib only): connect, read
// HELLO, send IDENTIFY with no intents, wait for READY, close.

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"runtime"
	"time"
)

const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

type wsConn struct {
	c  net.Conn
	br *bufio.Reader
}

func wsDial(ctx context.Context, raw string) (*wsConn, error) {
	if err := checkURL(raw, "discord_gateway"); err != nil {
		return nil, err
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	host := u.Host
	if u.Port() == "" {
		if u.Scheme == "wss" {
			host += ":443"
		} else {
			host += ":80"
		}
	}
	d := &net.Dialer{Timeout: 15 * time.Second}
	var c net.Conn
	if u.Scheme == "wss" {
		c, err = (&tls.Dialer{NetDialer: d, Config: &tls.Config{ServerName: u.Hostname(), MinVersion: tls.VersionTLS12}}).DialContext(ctx, "tcp", host)
	} else {
		c, err = d.DialContext(ctx, "tcp", host)
	}
	if err != nil {
		return nil, err
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = c.SetDeadline(dl)
	}
	keyb := make([]byte, 16)
	_, _ = rand.Read(keyb)
	key := base64.StdEncoding.EncodeToString(keyb)
	path := u.RequestURI()
	req := "GET " + path + " HTTP/1.1\r\nHost: " + u.Host + "\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Key: " + key + "\r\nSec-WebSocket-Version: 13\r\nUser-Agent: a2-bridge/" + appVersion + "\r\n\r\n"
	if _, err := io.WriteString(c, req); err != nil {
		c.Close()
		return nil, err
	}
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		c.Close()
		return nil, err
	}
	resp.Body.Close()
	h := sha1.Sum([]byte(key + wsGUID))
	if resp.StatusCode != http.StatusSwitchingProtocols || resp.Header.Get("Sec-WebSocket-Accept") != base64.StdEncoding.EncodeToString(h[:]) {
		c.Close()
		return nil, fmt.Errorf("gateway upgrade refused: HTTP %d", resp.StatusCode)
	}
	return &wsConn{c: c, br: br}, nil
}

func (w *wsConn) writeFrame(op byte, payload []byte) error {
	hdr := []byte{0x80 | op}
	n := len(payload)
	switch {
	case n < 126:
		hdr = append(hdr, 0x80|byte(n))
	case n < 1<<16:
		hdr = append(hdr, 0x80|126, byte(n>>8), byte(n))
	default:
		hdr = append(hdr, 0x80|127)
		hdr = binary.BigEndian.AppendUint64(hdr, uint64(n))
	}
	mask := make([]byte, 4)
	_, _ = rand.Read(mask)
	hdr = append(hdr, mask...)
	buf := make([]byte, n)
	for i := range payload {
		buf[i] = payload[i] ^ mask[i%4]
	}
	_, err := w.c.Write(append(hdr, buf...))
	return err
}

const maxWSMessage = 8 << 20

// readMessage returns the next text/binary message (answers pings, joins fragments).
// A close frame returns an error with the close code.
func (w *wsConn) readMessage() ([]byte, error) {
	var msg []byte
	for {
		var h [2]byte
		if _, err := io.ReadFull(w.br, h[:]); err != nil {
			return nil, err
		}
		fin, op := h[0]&0x80 != 0, h[0]&0x0f
		n := uint64(h[1] & 0x7f)
		switch n {
		case 126:
			var b [2]byte
			if _, err := io.ReadFull(w.br, b[:]); err != nil {
				return nil, err
			}
			n = uint64(binary.BigEndian.Uint16(b[:]))
		case 127:
			var b [8]byte
			if _, err := io.ReadFull(w.br, b[:]); err != nil {
				return nil, err
			}
			n = binary.BigEndian.Uint64(b[:])
		}
		var mask []byte
		if h[1]&0x80 != 0 {
			mask = make([]byte, 4)
			if _, err := io.ReadFull(w.br, mask); err != nil {
				return nil, err
			}
		}
		if n > maxWSMessage || uint64(len(msg))+n > maxWSMessage {
			return nil, errors.New("gateway frame too large")
		}
		p := make([]byte, n)
		if _, err := io.ReadFull(w.br, p); err != nil {
			return nil, err
		}
		if mask != nil {
			for i := range p {
				p[i] ^= mask[i%4]
			}
		}
		switch op {
		case 0x8:
			code := 0
			if len(p) >= 2 {
				code = int(binary.BigEndian.Uint16(p[:2]))
			}
			return nil, fmt.Errorf("gateway closed the connection (code %d)", code)
		case 0x9:
			if err := w.writeFrame(0xA, p); err != nil {
				return nil, err
			}
			continue
		case 0xA:
			continue
		}
		msg = append(msg, p...)
		if fin {
			return msg, nil
		}
	}
}

func (w *wsConn) close() {
	_ = w.writeFrame(0x8, []byte{0x03, 0xE8}) // 1000 normal
	w.c.Close()
}

type gwPayload struct {
	Op int             `json:"op"`
	T  string          `json:"t,omitempty"`
	D  json.RawMessage `json:"d,omitempty"`
}

// gatewayIdentifyOnce connects, identifies and returns after READY (20 s limit).
func gatewayIdentifyOnce(ctx context.Context, gwURL, token string) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	w, err := wsDial(ctx, gwURL)
	if err != nil {
		return err
	}
	defer w.close()
	stop := context.AfterFunc(ctx, func() { w.c.Close() })
	defer stop()
	for {
		data, err := w.readMessage()
		if err != nil {
			return err
		}
		var p gwPayload
		if err := json.Unmarshal(data, &p); err != nil {
			return errors.New("gateway sent non-JSON data")
		}
		switch p.Op {
		case 10: // HELLO
			id := map[string]any{"op": 2, "d": map[string]any{
				"token":   token,
				"intents": 0,
				"properties": map[string]string{
					"os": runtime.GOOS, "browser": "a2-bridge", "device": "a2-bridge",
				},
			}}
			b, _ := json.Marshal(id)
			if err := w.writeFrame(0x1, b); err != nil {
				return err
			}
		case 0:
			if p.T == "READY" {
				return nil
			}
		case 9:
			return errors.New("gateway: invalid session")
		}
	}
}
