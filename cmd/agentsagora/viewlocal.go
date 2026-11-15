package main

// Viewer UI on the server machine itself. Chrome's built-in Translator API is not
// offered on the LAN address with the self-signed certificate, but it is on localhost, so
// `serve` also listens on a loopback address (default 127.0.0.1:<port of --addr>) and the
// /view* routes accept https://localhost:<port> and https://<loopback addr> as well.
//
// Rules:
//   - A loopback origin counts only when the TCP peer is loopback. A LAN client that sends
//     Host: localhost still gets 403.
//   - Origin must equal the origin of the Host that was used (no mixing of the LAN origin
//     and a loopback origin).
//   - The loopback listener serves /view* only; every agent route answers 404 there, and the
//     agent API origin rule (server.go authOrigin) is unchanged.
//   - The phone keeps the LAN origin; one-time login links keep using it too (viewonce.go).

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strings"
)

// viewLocalKey marks requests that came in on the loopback view listener (ConnContext).
type viewLocalKey struct{}

// isLoopbackPeer reports whether the TCP peer of r is 127.0.0.0/8 or ::1.
func isLoopbackPeer(r *http.Request) bool {
	ip, err := netip.ParseAddr(remoteIP(r))
	return err == nil && ip.Unmap().IsLoopback()
}

// viewOriginFor is the origin the view accepts for this request: the configured LAN origin
// when the Host matches it, a loopback origin when the Host matches one and the peer is
// loopback, "" otherwise (refused).
func (sv *server) viewOriginFor(r *http.Request) string {
	o := "https://" + strings.ToLower(r.Host)
	if sv.viewOrigin != "" && o == sv.viewOrigin {
		return o
	}
	if slices.Contains(sv.viewLocal, o) && isLoopbackPeer(r) {
		return o
	}
	return ""
}

// viewLocalOrigins lists the origins of a loopback listener at addr (host:port).
func viewLocalOrigins(addr string) []string {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil
	}
	out := []string{"https://localhost:" + port}
	if o := "https://" + addr; o != out[0] {
		out = append(out, o)
	}
	return out
}

// viewLocalAddr turns the --view-local flag into a listen address: "auto" = 127.0.0.1 with
// the port of --addr, "off" or "" = none; anything else must be a loopback IP and a port.
func viewLocalAddr(flag, addr string) (string, error) {
	switch flag {
	case "", "off":
		return "", nil
	case "auto":
		_, port, err := net.SplitHostPort(addr)
		if err != nil {
			return "", err
		}
		return net.JoinHostPort("127.0.0.1", port), nil
	}
	host, _, err := net.SplitHostPort(flag)
	if err != nil {
		return "", fmt.Errorf("--view-local: %v", err)
	}
	ip, err := netip.ParseAddr(host)
	if err != nil || !ip.IsLoopback() {
		return "", fmt.Errorf("--view-local must be a loopback address (127.0.0.1:PORT or [::1]:PORT), \"auto\" or \"off\", got %q", flag)
	}
	return flag, nil
}

// viewLocalOnly answers 404 for every non-/view path that came in on the loopback listener.
func viewLocalOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if on, _ := r.Context().Value(viewLocalKey{}).(bool); on && r.URL.Path != "/view" && !strings.HasPrefix(r.URL.Path, "/view/") {
			http.Error(w, "only /view is served on this address", http.StatusNotFound)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// viewLocalConnContext marks connections accepted on the listener at local.
func viewLocalConnContext(local net.Addr) func(context.Context, net.Conn) context.Context {
	return func(ctx context.Context, c net.Conn) context.Context {
		if local != nil && c.LocalAddr().String() == local.String() {
			return context.WithValue(ctx, viewLocalKey{}, true)
		}
		return ctx
	}
}
