package ionospooltunnel

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.zx2c4.com/wireguard/conn"
	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun/netstack"
)

// Status exposes measured peer freshness, never WireGuard's raw IPC state.
type Status struct {
	Enabled             bool            `json:"enabled"`
	State               string          `json:"state"`
	PeerReachable       bool            `json:"peer_reachable"`
	HandshakeAgeSeconds *int64          `json:"handshake_age_seconds,omitempty"`
	Error               *ReadinessError `json:"error,omitempty"`
}

type Tunnel struct {
	device *device.Device
	net    *netstack.Net
	worker netip.AddrPort
	close  sync.Once
}

func open(cfg config) (*Tunnel, error) {
	tun, network, err := netstack.CreateNetTUN([]netip.Addr{cfg.address}, nil, 1420)
	if err != nil {
		return nil, refuse("wireguard_start_failed")
	}
	d := device.NewDevice(tun, conn.NewDefaultBind(), device.NewLogger(device.LogLevelSilent, ""))
	settings := fmt.Sprintf("private_key=%s\nlisten_port=%d\nreplace_peers=true\npublic_key=%s\nreplace_allowed_ips=true\nallowed_ip=%s/32\n", cfg.privateKey, cfg.listenPort, cfg.publicKey, cfg.peer)
	if cfg.endpoint.IsValid() {
		settings += "endpoint=" + cfg.endpoint.String() + "\npersistent_keepalive_interval=25\n"
	}
	if err := d.IpcSet(settings); err != nil {
		d.Close()
		return nil, refuse("wireguard_start_failed")
	}
	if err := d.Up(); err != nil {
		d.Close()
		return nil, refuse("wireguard_start_failed")
	}
	return &Tunnel{device: d, net: network, worker: netip.AddrPortFrom(cfg.peer, 8443)}, nil
}

// Open starts a userspace tunnel with exactly one route to the admitted origin.
func Open(path string, origin netip.AddrPort) (*Tunnel, error) {
	cfg, err := readConfig(path, false)
	if err != nil {
		return nil, err
	}
	if cfg.peer != origin.Addr() || origin.Port() != 8443 {
		return nil, refuse("wireguard_origin_mismatch")
	}
	return open(cfg)
}

// DialContext never consults host routing, proxies or DNS, including on error.
func (t *Tunnel) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	target, err := netip.ParseAddrPort(address)
	if err != nil || target != t.worker || (network != "tcp" && network != "tcp4") {
		return nil, refuse("wireguard_destination_refused")
	}
	return t.net.DialContextTCPAddrPort(ctx, target)
}

func (t *Tunnel) Close() { t.close.Do(t.device.Close) }

func (t *Tunnel) Status() Status {
	status := Status{Enabled: true, State: "starting", Error: &ReadinessError{Code: "wireguard_handshake_pending"}}
	// IpcGet includes private keys. Consume it locally and project only the
	// handshake timestamp. Never return it or any parsing error to callers.
	state, err := t.device.IpcGet()
	if err != nil {
		status.State, status.Error.Code = "unavailable", "wireguard_state_unavailable"
		return status
	}
	for _, line := range strings.Split(state, "\n") {
		if seconds, ok := strings.CutPrefix(line, "last_handshake_time_sec="); ok {
			stamp, err := strconv.ParseInt(seconds, 10, 64)
			if err != nil || stamp <= 0 {
				return status
			}
			age := int64(time.Since(time.Unix(stamp, 0)).Seconds())
			if age < 0 {
				return status
			}
			status.HandshakeAgeSeconds = &age
			status.PeerReachable = age <= 180
			if status.PeerReachable {
				status.State, status.Error = "ready", nil
			} else {
				status.State, status.Error.Code = "stale", "wireguard_handshake_stale"
			}
			return status
		}
	}
	return status
}
