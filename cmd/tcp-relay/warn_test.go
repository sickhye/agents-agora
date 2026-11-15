package main

import (
	"net"
	"testing"
)

func TestStartupWarnings(t *testing.T) {
	cases := []struct {
		listen, allow string
		want          int
	}{
		{"203.0.113.5:18765", "203.0.113.0/24", 0},
		{"203.0.113.5:18765", "203.0.113.7/32", 0},
		{"203.0.113.5:18765", "203.0.0.0/16", 1},
		{"203.0.113.5:18765", "0.0.0.0/0", 1},
		{"0.0.0.0:18765", "203.0.113.0/24", 1},
		{":18765", "203.0.113.0/24", 1},
		{"[::]:18765", "::/0", 2},
		{"[2001:db8::5]:18765", "2001:db8::/64", 0},
		{"[2001:db8::5]:18765", "2001:db8::/32", 1},
	}
	for _, c := range cases {
		_, nw, err := net.ParseCIDR(c.allow)
		if err != nil {
			t.Fatal(err)
		}
		if got := startupWarnings(c.listen, nw); len(got) != c.want {
			t.Errorf("listen %s allow %s: %d warnings %v, want %d", c.listen, c.allow, len(got), got, c.want)
		}
	}
}

func TestAllowed(t *testing.T) {
	_, nw, _ := net.ParseCIDR("192.0.2.0/24")
	if !allowed(&net.TCPAddr{IP: net.ParseIP("192.0.2.9")}, nw) {
		t.Error("inside --allow rejected")
	}
	if allowed(&net.TCPAddr{IP: net.ParseIP("198.51.100.9")}, nw) {
		t.Error("outside --allow accepted")
	}
}
