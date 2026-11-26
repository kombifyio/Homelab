// Package ionospooltunnel owns the private, single-worker WireGuard transport.
package ionospooltunnel

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"io"
	"net/netip"
	"os"
	"strconv"
	"strings"
)

var poolNetwork = netip.MustParsePrefix("100.64.200.0/24")

// ReadinessError deliberately carries only a closed code, never parser input,
// file contents, paths, WireGuard IPC output or underlying crypto errors.
type ReadinessError struct {
	Code string `json:"reason_code"`
}

func (e *ReadinessError) Error() string { return "IONOS pool unavailable: " + e.Code }

func refuse(code string) error { return &ReadinessError{Code: code} }

type config struct {
	privateKey string
	publicKey  string
	address    netip.Addr
	peer       netip.Addr
	endpoint   netip.AddrPort
	listenPort uint16
}

// readConfig accepts only the custody format. In particular, wg-quick shell
// hooks, DNS, routing tables, extra peers and broad AllowedIPs are rejected.
func readConfig(path string, worker bool) (config, error) {
	file, err := os.Open(path)
	if err != nil {
		return config{}, refuse("wireguard_file_unavailable")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0022 != 0 || (worker && info.Mode().Perm()&0077 != 0) {
		return config{}, refuse("wireguard_file_permissions")
	}
	data, err := io.ReadAll(io.LimitReader(file, 8193))
	if err != nil || len(data) > 8192 {
		return config{}, refuse("wireguard_config_invalid")
	}
	return parseConfig(data, worker)
}

func parseConfig(data []byte, worker bool) (config, error) {
	invalid := func() (config, error) { return config{}, refuse("wireguard_config_invalid") }
	values := map[string]string{}
	section := ""
	sections := map[string]bool{}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := strings.TrimSpace(strings.SplitN(scanner.Text(), "#", 2)[0])
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") {
			if (line != "[Interface]" && line != "[Peer]") || sections[line] {
				return invalid()
			}
			section = line
			sections[line] = true
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		allowed := section == "[Interface]" && (key == "PrivateKey" || key == "Address" || (worker && key == "ListenPort")) || section == "[Peer]" && (key == "PublicKey" || key == "AllowedIPs" || (!worker && (key == "Endpoint" || key == "PersistentKeepalive")))
		if !ok || !allowed || value == "" || values[key] != "" {
			return invalid()
		}
		values[key] = value
	}
	if scanner.Err() != nil || !sections["[Interface]"] || !sections["[Peer]"] {
		return invalid()
	}
	var cfg config
	for key, destination := range map[string]*string{"PrivateKey": &cfg.privateKey, "PublicKey": &cfg.publicKey} {
		decoded, err := base64.StdEncoding.Strict().DecodeString(values[key])
		if err != nil || len(decoded) != 32 || bytes.Equal(decoded, make([]byte, 32)) {
			return invalid()
		}
		*destination = hex.EncodeToString(decoded)
	}
	address, err := netip.ParsePrefix(values["Address"])
	if err != nil || address.Bits() != 32 || !poolNetwork.Contains(address.Addr()) {
		return invalid()
	}
	allowed, err := netip.ParsePrefix(values["AllowedIPs"])
	if err != nil || allowed.Bits() != 32 || !poolNetwork.Contains(allowed.Addr()) || allowed.Addr() == address.Addr() {
		return invalid()
	}
	cfg.address, cfg.peer = address.Addr(), allowed.Addr()
	if worker {
		port, err := strconv.ParseUint(values["ListenPort"], 10, 16)
		if err != nil || port != 51820 {
			return invalid()
		}
		cfg.listenPort = uint16(port)
	} else {
		endpoint, err := netip.ParseAddrPort(values["Endpoint"])
		if err != nil || !endpoint.Addr().Is4() || !endpoint.Addr().IsGlobalUnicast() || endpoint.Port() != 51820 {
			return invalid()
		}
		if keepalive := values["PersistentKeepalive"]; keepalive != "" && keepalive != "25" {
			return invalid()
		}
		cfg.endpoint = endpoint
	}
	return cfg, nil
}

// WorkerConfig returns a canonical wg-quick file with no executable directives.
// Enrollment writes this directly to root-only custody; it must never be logged.
func WorkerConfig(path string) ([]byte, error) {
	cfg, err := readConfig(path, true)
	if err != nil {
		return nil, err
	}
	key := func(value string) string {
		decoded, _ := hex.DecodeString(value)
		return base64.StdEncoding.EncodeToString(decoded)
	}
	return []byte("[Interface]\nPrivateKey = " + key(cfg.privateKey) + "\nAddress = " + cfg.address.String() + "/32\nListenPort = 51820\n\n[Peer]\nPublicKey = " + key(cfg.publicKey) + "\nAllowedIPs = " + cfg.peer.String() + "/32\n"), nil
}
