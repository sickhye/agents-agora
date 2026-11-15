package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"math/big"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// a2 tls init [--dir D] [--hosts h1,h2] [--force]
// a2 tls renew-server [--dir D] [--hosts h1,h2]   (new server cert, same CA)
// Makes a private CA (ca.pem, ca-key.pem) and a server certificate signed by it
// (server.pem, server-key.pem). Keys are 0600, the dir 0700. The CA is name-constrained to
// the given hosts, so even its key cannot issue a certificate that works for another site.
// Clients pin ca.pem (the only file to hand over). Prints paths, never key material.

const defaultTLSHosts = "localhost,127.0.0.1,::1" // add the server name or LAN IP with --hosts

func cmdTLS(args []string) error {
	if len(args) > 0 && args[0] == "renew-server" {
		return cmdTLSRenew(args[1:])
	}
	if len(args) == 0 || args[0] != "init" {
		return usageError("tls: init or renew-server")
	}
	fs := flag.NewFlagSet("tls init", flag.ContinueOnError)
	dir := fs.String("dir", "", "output dir (default ~/.config/agentsagora/tls)")
	hosts := fs.String("hosts", defaultTLSHosts, "server names and IPs, comma separated")
	force := fs.Bool("force", false, "replace existing files (clients then need the new ca.pem)")
	if err := fs.Parse(args[1:]); err != nil {
		return usageError(err.Error())
	}
	if *dir == "" {
		*dir = tlsDir()
	}
	ca, srv, err := tlsInit(expandHome(*dir), *hosts, *force, time.Now())
	if err != nil {
		return err
	}
	fmt.Println("CA certificate (hand this one over):", ca)
	fmt.Println("server certificate:", srv)
	fmt.Println("keys stay in", expandHome(*dir), "(mode 0600, not printed)")
	return nil
}

// cmdTLSRenew: tls renew-server [--dir D] [--hosts h1,h2]. The running server loads the new
// certificate on its next start.
func cmdTLSRenew(args []string) error {
	fs := flag.NewFlagSet("tls renew-server", flag.ContinueOnError)
	dir := fs.String("dir", "", "TLS dir (default ~/.config/agentsagora/tls)")
	hosts := fs.String("hosts", "", "server names and IPs (default: those of the current server certificate)")
	if err := fs.Parse(args); err != nil {
		return usageError(err.Error())
	}
	if *dir == "" {
		*dir = tlsDir()
	}
	p, err := tlsRenewServer(expandHome(*dir), *hosts, time.Now())
	if err != nil {
		return err
	}
	fmt.Println("new server certificate:", p, "(same CA; restart the server to load it)")
	return nil
}

func splitHosts(s string) (dns []string, ips []net.IP, err error) {
	for _, h := range strings.Split(s, ",") {
		h = strings.TrimSpace(h)
		if h == "" {
			continue
		}
		if ip, perr := netip.ParseAddr(h); perr == nil {
			ips = append(ips, net.IP(ip.AsSlice()))
		} else if strings.ContainsAny(h, " /*") {
			return nil, nil, fmt.Errorf("bad host %q", h)
		} else {
			dns = append(dns, h)
		}
	}
	if len(dns)+len(ips) == 0 {
		return nil, nil, errors.New("no hosts")
	}
	return dns, ips, nil
}

func serial() *big.Int {
	n, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	return n
}

func tlsInit(dir, hosts string, force bool, now time.Time) (caPath, srvPath string, err error) {
	dns, ips, err := splitHosts(hosts)
	if err != nil {
		return "", "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", "", err
	}
	caPath, caKeyPath := filepath.Join(dir, "ca.pem"), filepath.Join(dir, "ca-key.pem")
	srvPath, srvKeyPath := filepath.Join(dir, "server.pem"), filepath.Join(dir, "server-key.pem")
	if !force {
		for _, p := range []string{caPath, caKeyPath, srvPath, srvKeyPath} {
			if _, err := os.Stat(p); err == nil {
				return "", "", fmt.Errorf("%s exists; --force replaces all four files (clients then need the new ca.pem)", p)
			}
		}
	}
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", "", err
	}
	var ipNets []*net.IPNet
	for _, ip := range ips {
		bits := 128
		if ip.To4() != nil {
			ip, bits = ip.To4(), 32
		}
		ipNets = append(ipNets, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
	}
	caTmpl := &x509.Certificate{
		SerialNumber:                serial(),
		Subject:                     pkix.Name{CommonName: "AgentsAgora private CA", Organization: []string{"AgentsAgora"}},
		NotBefore:                   now.Add(-time.Hour),
		NotAfter:                    now.AddDate(10, 0, 0),
		IsCA:                        true,
		BasicConstraintsValid:       true,
		MaxPathLenZero:              true,
		KeyUsage:                    x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		PermittedDNSDomainsCritical: true,
		PermittedDNSDomains:         dns,
		PermittedIPRanges:           ipNets,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		return "", "", err
	}
	caCert, _ := x509.ParseCertificate(caDER)
	srvKey, srvDER, err := issueServerCert(caCert, caKey, dns, ips, now)
	if err != nil {
		return "", "", err
	}
	if err := writeKey(caKeyPath, caKey); err != nil {
		return "", "", err
	}
	if err := writeKey(srvKeyPath, srvKey); err != nil {
		return "", "", err
	}
	if err := writeCert(caPath, caDER); err != nil {
		return "", "", err
	}
	if err := writeCert(srvPath, srvDER); err != nil {
		return "", "", err
	}
	return caPath, srvPath, nil
}

func writeKey(path string, k *ecdsa.PrivateKey) error {
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		return err
	}
	return writeFile0600(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

// writeCert writes a public certificate with mode 0644.
func writeCert(path string, der []byte) error {
	if err := writeFile0600(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})); err != nil {
		return err
	}
	return os.Chmod(path, 0o644)
}

// pinnedTLS trusts only the given CA file (no system roots).
func pinnedTLS(caFile string) (*tls.Config, error) {
	b, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("CA file: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(b) {
		return nil, fmt.Errorf("CA file %s has no certificate", caFile)
	}
	return &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS13}, nil
}

// serverCertDays keeps the server certificate inside the 398-day limit that Safari and
// Chrome apply (macOS also refuses any TLS certificate valid for more than 825 days).
const serverCertDays = 397

// issueServerCert makes a new server key and a certificate for dns/ips signed by the CA.
func issueServerCert(caCert *x509.Certificate, caKey *ecdsa.PrivateKey, dns []string, ips []net.IP, now time.Time) (*ecdsa.PrivateKey, []byte, error) {
	srvKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	nb := now.Add(-time.Hour)
	srvTmpl := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: "AgentsAgora server"},
		NotBefore:    nb,
		NotAfter:     nb.AddDate(0, 0, serverCertDays),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     dns,
		IPAddresses:  ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, srvTmpl, caCert, &srvKey.PublicKey, caKey)
	if err != nil {
		return nil, nil, err
	}
	return srvKey, der, nil
}

// tlsRenewServer replaces server.pem and server-key.pem with a new pair signed by the
// existing CA (ca.pem and ca-key.pem stay as they are, so clients keep their ca.pem).
// hosts "" reuses the names and addresses of the current server certificate.
func tlsRenewServer(dir, hosts string, now time.Time) (string, error) {
	caPath, caKeyPath := filepath.Join(dir, "ca.pem"), filepath.Join(dir, "ca-key.pem")
	srvPath, srvKeyPath := filepath.Join(dir, "server.pem"), filepath.Join(dir, "server-key.pem")
	st, err := os.Stat(caKeyPath)
	if err != nil {
		return "", fmt.Errorf("CA key: %w (run: a2 tls init)", err)
	}
	if openToOthers(st) {
		return "", fmt.Errorf("CA key %s must be mode 0600", caKeyPath)
	}
	caCert, err := readCertPEM(caPath)
	if err != nil {
		return "", err
	}
	kb, err := os.ReadFile(caKeyPath)
	if err != nil {
		return "", err
	}
	blk, _ := pem.Decode(kb)
	if blk == nil {
		return "", fmt.Errorf("%s: no PEM block", caKeyPath)
	}
	k, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
	if err != nil {
		return "", fmt.Errorf("%s: %w", caKeyPath, err)
	}
	caKey, ok := k.(*ecdsa.PrivateKey)
	if !ok {
		return "", fmt.Errorf("%s: not an ECDSA key", caKeyPath)
	}
	var dns []string
	var ips []net.IP
	if hosts != "" {
		if dns, ips, err = splitHosts(hosts); err != nil {
			return "", err
		}
	} else {
		old, err := readCertPEM(srvPath)
		if err != nil {
			return "", fmt.Errorf("%w (give --hosts)", err)
		}
		dns, ips = old.DNSNames, old.IPAddresses
	}
	srvKey, der, err := issueServerCert(caCert, caKey, dns, ips, now)
	if err != nil {
		return "", err
	}
	if err := writeKey(srvKeyPath, srvKey); err != nil {
		return "", err
	}
	if err := writeCert(srvPath, der); err != nil {
		return "", err
	}
	return srvPath, nil
}

func readCertPEM(path string) (*x509.Certificate, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	blk, _ := pem.Decode(b)
	if blk == nil || blk.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("%s: no certificate", path)
	}
	return x509.ParseCertificate(blk.Bytes)
}
