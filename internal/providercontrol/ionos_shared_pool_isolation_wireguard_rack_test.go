package providercontrol

import (
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kombifyio/homelab/internal/ionospooltunnel"
)

// These entry points run in separate rack containers via the committed script.
// The worker uses a kernel WireGuard interface; the control plane has no TUN
// device, NET_ADMIN capability, kernel interface, DNS or host route to the peer.
func rackDirectory(t *testing.T) string {
	t.Helper()
	directory := os.Getenv("IONOS_POOL_RACK_DIR")
	if directory == "" {
		t.Skip("rack container proof only")
	}
	return directory
}

func TestRackGenerateCustody(t *testing.T) {
	directory := rackDirectory(t)
	endpoint := os.Getenv("IONOS_POOL_RACK_ENDPOINT")
	if endpoint == "" {
		t.Fatal("worker UDP endpoint required")
	}
	write := func(name string, data []byte) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(directory, name), data, 0600); err != nil {
			t.Fatal("fixture custody write failed")
		}
	}
	controlKey, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal("fixture key generation failed")
	}
	workerKey, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal("fixture key generation failed")
	}
	key := base64.StdEncoding.EncodeToString
	write("control.conf", []byte("[Interface]\nPrivateKey = "+key(controlKey.Bytes())+"\nAddress = 100.64.200.2/32\n[Peer]\nPublicKey = "+key(workerKey.PublicKey().Bytes())+"\nEndpoint = "+endpoint+"\nAllowedIPs = 100.64.200.10/32\nPersistentKeepalive = 25\n"))
	write("wg-kombify.conf", []byte("[Interface]\nPrivateKey = "+key(workerKey.Bytes())+"\nAddress = 100.64.200.10/32\nListenPort = 51820\n[Peer]\nPublicKey = "+key(controlKey.PublicKey().Bytes())+"\nAllowedIPs = 100.64.200.2/32\n"))
	caPublic, caPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal("fixture CA generation failed")
	}
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "rack transport fixture"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, caPublic, caPrivate)
	if err != nil {
		t.Fatal("fixture CA signing failed")
	}
	write("ca.crt", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}))
	for i, name := range []string{"worker", "client"} {
		public, private, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal("fixture TLS key generation failed")
		}
		certificate := &x509.Certificate{SerialNumber: big.NewInt(int64(i + 2)), NotBefore: ca.NotBefore, NotAfter: ca.NotAfter, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
		if name == "worker" {
			certificate.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
			certificate.IPAddresses = []net.IP{net.ParseIP("100.64.200.10")}
		}
		der, err := x509.CreateCertificate(rand.Reader, certificate, ca, public, caPrivate)
		if err != nil {
			t.Fatal("fixture TLS signing failed")
		}
		privateDER, err := x509.MarshalPKCS8PrivateKey(private)
		if err != nil {
			t.Fatal("fixture TLS encoding failed")
		}
		write(name+".crt", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
		write(name+".key", pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER}))
	}
}

func TestRackWireGuardWorker(t *testing.T) {
	directory := rackDirectory(t)
	if _, err := ionospooltunnel.WorkerConfig(filepath.Join(directory, "wg-kombify.conf")); err != nil {
		t.Fatal("worker peer file refused")
	}
	ca, err := os.ReadFile(filepath.Join(directory, "ca.crt"))
	if err != nil {
		t.Fatal("fixture CA unavailable")
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		t.Fatal("fixture CA refused")
	}
	server := &http.Server{
		Addr:              "100.64.200.10:8443",
		TLSConfig:         &tls.Config{MinVersion: tls.VersionTLS13, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: roots},
		ReadHeaderTimeout: time.Second, WriteTimeout: time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
			host, _, _ := net.SplitHostPort(request.RemoteAddr)
			if host != "100.64.200.2" || request.TLS == nil || len(request.TLS.VerifiedChains) == 0 {
				http.Error(w, "peer refused", http.StatusForbidden)
				return
			}
			_, _ = io.WriteString(w, "mtls-through-wireguard")
		}),
	}
	t.Cleanup(func() { _ = server.Close() })
	if err := server.ListenAndServeTLS(filepath.Join(directory, "worker.crt"), filepath.Join(directory, "worker.key")); err != nil && !errors.Is(err, http.ErrServerClosed) {
		t.Fatal("rack worker listener failed")
	}
}

func TestRackWireGuardClient(t *testing.T) {
	directory := rackDirectory(t)
	const origin = "https://100.64.200.10:8443"
	openClient := func(file string) (*ionospooltunnel.Tunnel, *http.Client, error) {
		return ionospooltunnel.OpenHTTPClient(origin, filepath.Join(directory, file), filepath.Join(directory, "ca.crt"), filepath.Join(directory, "client.crt"), filepath.Join(directory, "client.key"), 5*time.Second)
	}
	if tunnel, client, err := openClient("missing.conf"); tunnel != nil || client != nil || err == nil {
		t.Fatal("missing tunnel file did not refuse transport construction")
	} else {
		var readiness *ionospooltunnel.ReadinessError
		if !errors.As(err, &readiness) || readiness.Code != "wireguard_file_unavailable" {
			t.Fatal("missing tunnel file returned wrong readiness error")
		}
	}
	for _, missing := range []struct{ ca, cert, code string }{
		{"ca.crt", "missing.crt", "mtls_client_identity_invalid"},
		{"missing.crt", "client.crt", "mtls_server_ca_invalid"},
	} {
		tunnel, client, err := ionospooltunnel.OpenHTTPClient(origin, filepath.Join(directory, "control.conf"), filepath.Join(directory, missing.ca), filepath.Join(directory, missing.cert), filepath.Join(directory, "client.key"), time.Second)
		var readiness *ionospooltunnel.ReadinessError
		if tunnel != nil || client != nil || !errors.As(err, &readiness) || readiness.Code != missing.code {
			t.Fatal("missing mTLS file did not refuse transport construction")
		}
	}
	tunnel, client, err := openClient("control.conf")
	if err != nil {
		t.Fatal("rack tunnel initialization failed")
	}
	t.Cleanup(tunnel.Close)
	for _, destination := range []string{"100.64.200.11:8443", "100.64.200.10:443", "worker.example:8443", "127.0.0.1:8443"} {
		if _, err := tunnel.DialContext(t.Context(), "tcp", destination); err == nil {
			t.Fatal("unpinned destination was accepted")
		}
	}
	deadline := time.Now().Add(45 * time.Second)
	for {
		response, err := client.Get(origin)
		if err == nil {
			body, readErr := io.ReadAll(response.Body)
			_ = response.Body.Close()
			if readErr != nil || response.StatusCode != http.StatusOK || string(body) != "mtls-through-wireguard" {
				t.Fatal("mTLS peer response refused")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("mTLS through userspace tunnel did not complete")
		}
		time.Sleep(250 * time.Millisecond)
	}
	if status := tunnel.Status(); !status.PeerReachable || status.HandshakeAgeSeconds == nil || status.Error != nil {
		t.Fatal("successful request did not produce fresh handshake health")
	}
	transport := client.Transport.(*http.Transport).Clone()
	transport.TLSClientConfig.Certificates = nil
	unauthenticated := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	if response, err := unauthenticated.Get(origin); err == nil {
		_ = response.Body.Close()
		t.Fatal("worker admitted a peer without its mTLS client identity")
	}
	transport.CloseIdleConnections()
	if direct, err := net.DialTimeout("tcp", strings.TrimPrefix(origin, "https://"), time.Second); err == nil {
		_ = direct.Close()
		t.Fatal("worker unexpectedly reachable through host routing")
	}
	t.Log("mTLS request through userspace WireGuard; fresh handshake; missing-file refusal; missing-client-cert refusal; no host route: PASS")
}
