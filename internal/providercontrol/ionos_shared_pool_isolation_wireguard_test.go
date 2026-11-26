package providercontrol

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kombifyio/homelab/internal/ionospooltunnel"
)

func testConfig() string {
	private := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("a", 32)))
	public := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("b", 32)))
	return "[Interface]\nPrivateKey = " + private + "\nAddress = 100.64.200.2/32\n[Peer]\nPublicKey = " + public + "\nEndpoint = 192.0.2.10:51820\nAllowedIPs = 100.64.200.10/32\nPersistentKeepalive = 25\n"
}

func configReadiness(t *testing.T, input string) *ionospooltunnel.ReadinessError {
	t.Helper()
	path := filepath.Join(t.TempDir(), "wireguard.conf")
	if err := os.WriteFile(path, []byte(input), 0600); err != nil {
		t.Fatal(err)
	}
	tunnel, client, err := ionospooltunnel.OpenHTTPClient("https://100.64.200.10:8443", path, "", "", "", time.Second)
	var readiness *ionospooltunnel.ReadinessError
	if tunnel != nil || client != nil || !errors.As(err, &readiness) {
		t.Fatal("expected closed readiness failure before transport creation")
	}
	return readiness
}

func TestCustodyWireGuardConfig(t *testing.T) {
	if configReadiness(t, testConfig()).Code != "mtls_client_identity_invalid" {
		t.Fatal("valid control-plane configuration refused")
	}
	for name, input := range map[string]string{
		"default-route":    strings.ReplaceAll(testConfig(), "100.64.200.10/32", "0.0.0.0/0"),
		"pool-subnet":      strings.ReplaceAll(testConfig(), "100.64.200.10/32", "100.64.200.0/24"),
		"two-routes":       strings.ReplaceAll(testConfig(), "100.64.200.10/32", "100.64.200.10/32, 100.64.200.11/32"),
		"self-route":       strings.ReplaceAll(testConfig(), "100.64.200.10/32", "100.64.200.2/32"),
		"outside-pool":     strings.ReplaceAll(testConfig(), "100.64.200.2/32", "10.0.0.2/32"),
		"interface-subnet": strings.ReplaceAll(testConfig(), "100.64.200.2/32", "100.64.200.2/24"),
		"shell-hook":       testConfig() + "PostUp = echo secret\n",
		"second-peer":      testConfig() + "[Peer]\nPublicKey = ignored\n",
		"duplicate-key":    testConfig() + "PublicKey = ignored\n",
		"bad-key":          strings.ReplaceAll(testConfig(), "PrivateKey = ", "PrivateKey = invalid"),
		"dns-endpoint":     strings.ReplaceAll(testConfig(), "192.0.2.10:51820", "worker.example:51820"),
		"wrong-port":       strings.ReplaceAll(testConfig(), ":51820", ":53"),
		"no-keepalive":     strings.ReplaceAll(testConfig(), "PersistentKeepalive = 25", "PersistentKeepalive = 0"),
	} {
		t.Run(name, func(t *testing.T) {
			if configReadiness(t, input).Code != "wireguard_config_invalid" {
				t.Fatal("invalid configuration did not return a closed readiness error")
			}
		})
	}
}

func TestMissingOrInvalidFileCannotCreateHTTPTransport(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wireguard.conf")
	for _, input := range []string{"", "[Interface]\nPrivateKey = do-not-leak\n", strings.Repeat("x", 8193)} {
		if input != "" {
			if err := os.WriteFile(path, []byte(input), 0600); err != nil {
				t.Fatal(err)
			}
		}
		tunnel, client, err := ionospooltunnel.OpenHTTPClient("https://100.64.200.10:8443", path, "", "", "", time.Second)
		var refusal *ionospooltunnel.ReadinessError
		if tunnel != nil || client != nil || !errors.As(err, &refusal) {
			t.Fatal("invalid custody created a transport")
		}
		payload, _ := json.Marshal(ionospooltunnel.Status{Enabled: true, State: "unavailable", Error: refusal})
		if strings.Contains(string(payload), "do-not-leak") || strings.Contains(string(payload), path) {
			t.Fatal("health exposed secret input or custody path")
		}
	}
}

func TestWireGuardOriginIsPinned(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wireguard.conf")
	if err := os.WriteFile(path, []byte(testConfig()), 0600); err != nil {
		t.Fatal(err)
	}
	if tunnel, err := ionospooltunnel.Open(path, netip.MustParseAddrPort("100.64.200.11:8443")); tunnel != nil || err == nil {
		t.Fatal("origin mismatch created a tunnel")
	}

}

func TestWorkerConfigCannotInstallHooksOrBroadPeers(t *testing.T) {
	worker := strings.ReplaceAll(testConfig(), "100.64.200.2/32", "100.64.200.20/32")
	worker = strings.ReplaceAll(worker, "100.64.200.10/32", "100.64.200.2/32")
	worker = strings.ReplaceAll(worker, "Endpoint = 192.0.2.10:51820\n", "")
	worker = strings.ReplaceAll(worker, "PersistentKeepalive = 25\n", "")
	worker = strings.ReplaceAll(worker, "[Peer]", "ListenPort = 51820\n[Peer]")
	path := filepath.Join(t.TempDir(), "worker.conf")
	if err := os.WriteFile(path, []byte(worker), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ionospooltunnel.WorkerConfig(path); err != nil {
		t.Fatal("valid worker configuration refused")
	}
	for _, input := range []string{worker + "PostUp = ignored\n", strings.ReplaceAll(worker, "100.64.200.2/32", "100.64.200.0/24"), strings.ReplaceAll(worker, "ListenPort = 51820", "ListenPort = 53")} {
		if err := os.WriteFile(path, []byte(input), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := ionospooltunnel.WorkerConfig(path); err == nil {
			t.Fatal("unsafe worker enrollment accepted")
		}
	}
}
