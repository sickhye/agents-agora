// tcp-relay forwards TCP connections from one address to another, byte for byte.
// It does not look at the data, so TLS stays end to end between client and server.
//
// Use: tcp-relay --listen 203.0.113.5:18765 --to 192.0.2.10:18765 --allow 203.0.113.0/24
//
// Keep --allow narrow (the CIDR of your clients, not 0.0.0.0/0) and listen on one interface
// address, not a wildcard; the relay warns at startup when it is not. The server sees the
// relay's address as the source of relayed clients, so the server's own --allow, lockout and
// audit log apply to the relay, not to the client. Only this --allow filters them.
//
// Only clients whose source address is inside --allow (required) are accepted. If the
// listen address is not up yet (for example a VPN interface starting), it
// retries every 5 s instead of exiting.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"time"
)

func main() {
	listen := flag.String("listen", "", "address to listen on (host:port)")
	to := flag.String("to", "", "address to forward to (host:port)")
	allow := flag.String("allow", "", "accept only clients from this CIDR (required)")
	flag.Parse()
	if *listen == "" || *to == "" || *allow == "" {
		fmt.Fprintln(os.Stderr, "usage: tcp-relay --listen HOST:PORT --to HOST:PORT --allow CIDR")
		os.Exit(2)
	}
	_, nw, err := net.ParseCIDR(*allow)
	if err != nil {
		log.Fatalf("tcp-relay: bad --allow: %v", err)
	}
	for _, w := range startupWarnings(*listen, nw) {
		log.Printf("tcp-relay: WARNING: %s", w)
	}
	var ln net.Listener
	for {
		ln, err = net.Listen("tcp", *listen)
		if err == nil {
			break
		}
		log.Printf("tcp-relay: listen %s: %v (retry in 5s)", *listen, err)
		time.Sleep(5 * time.Second)
	}
	log.Printf("tcp-relay: %s -> %s (allow %s)", *listen, *to, nw)
	serve(ln, *to, nw)
}

func serve(ln net.Listener, to string, nw *net.IPNet) {
	for {
		c, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			log.Printf("tcp-relay: accept: %v", err)
			time.Sleep(time.Second)
			continue
		}
		if !allowed(c.RemoteAddr(), nw) {
			log.Printf("tcp-relay: reject %s", c.RemoteAddr())
			c.Close()
			continue
		}
		go relay(c, to)
	}
}

func allowed(a net.Addr, nw *net.IPNet) bool {
	ta, ok := a.(*net.TCPAddr)
	return ok && nw.Contains(ta.IP)
}

func relay(c net.Conn, to string) {
	defer c.Close()
	u, err := net.DialTimeout("tcp", to, 10*time.Second)
	if err != nil {
		log.Printf("tcp-relay: dial %s: %v", to, err)
		return
	}
	defer u.Close()
	done := make(chan struct{}, 2)
	pipe := func(dst, src net.Conn) {
		io.Copy(dst, src)
		if tc, ok := dst.(*net.TCPConn); ok {
			tc.CloseWrite()
		}
		done <- struct{}{}
	}
	go pipe(u, c)
	go pipe(c, u)
	<-done
	<-done
}
