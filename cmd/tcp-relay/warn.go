package main

import (
	"fmt"
	"net"
)

// startupWarnings lists the risky settings of this relay. They are only warnings: the relay
// still starts. The relay is a hop: the server sees the relay's address, not the client's,
// so --allow here is the only source filter that applies to relayed clients.
func startupWarnings(listen string, nw *net.IPNet) []string {
	var out []string
	if host, _, err := net.SplitHostPort(listen); err == nil {
		if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
			out = append(out, fmt.Sprintf("--listen %s is a wildcard (every interface); give the address of one interface", listen))
		}
	}
	ones, bits := nw.Mask.Size()
	limit := 24
	if bits == 128 {
		limit = 64
	}
	if ones < limit {
		out = append(out, fmt.Sprintf("--allow %s is wider than /%d; use the narrowest CIDR that covers your clients", nw, limit))
	}
	return out
}
