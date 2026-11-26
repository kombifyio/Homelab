package ionospooltunnel

import (
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"time"
)

// OpenHTTPClient couples mTLS to the tunnel dialer; no direct transport exists.
func OpenHTTPClient(origin, wireguardFile, caFile, certFile, keyFile string, timeout time.Duration) (*Tunnel, *http.Client, error) {
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return nil, nil, refuse("wireguard_origin_invalid")
	}
	target, err := netip.ParseAddrPort(parsed.Host)
	if err != nil || !poolNetwork.Contains(target.Addr()) || target.Port() != 8443 {
		return nil, nil, refuse("wireguard_origin_invalid")
	}
	// Validate the tunnel file before reading mTLS custody or opening sockets.
	cfg, err := readConfig(wireguardFile, false)
	if err != nil {
		return nil, nil, err
	}
	if cfg.peer != target.Addr() {
		return nil, nil, refuse("wireguard_origin_mismatch")
	}
	certificate, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, nil, refuse("mtls_client_identity_invalid")
	}
	ca, err := os.ReadFile(caFile)
	if err != nil {
		return nil, nil, refuse("mtls_server_ca_invalid")
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		return nil, nil, refuse("mtls_server_ca_invalid")
	}
	tunnel, err := open(cfg)
	if err != nil {
		return nil, nil, err
	}
	transport := &http.Transport{
		DialContext:         tunnel.DialContext,
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots, Certificates: []tls.Certificate{certificate}},
		TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: timeout,
	}
	return tunnel, &http.Client{Transport: transport, Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, nil
}
