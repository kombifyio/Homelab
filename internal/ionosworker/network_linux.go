package ionosworker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	ipPath  = "/usr/sbin/ip"
	nftPath = "/usr/sbin/nft"
)

func networkAddresses(record *generation) (host, guest string) {
	return fmt.Sprintf("10.200.0.%d", record.Slot*4+1), fmt.Sprintf("10.200.0.%d", record.Slot*4+2)
}

func (runtime *Runtime) ensureNetwork(ctx context.Context, record *generation) error {
	if runtime.checkNetwork(ctx, record) == nil {
		return nil
	}
	if err := removeNetwork(ctx, record); err != nil {
		return err
	}
	id := record.Spec.NetworkNamespace
	link := hostLink(id)
	host, guest := networkAddresses(record)
	operations := [][]string{
		{"netns", "add", id},
		{"link", "add", link, "type", "veth", "peer", "name", "eth0", "netns", id},
		{"-n", id, "addr", "add", guest + "/30", "dev", "eth0"},
		{"addr", "add", host + "/30", "dev", link},
	}
	for _, args := range operations {
		if _, err := command(ctx, ipPath, args...); err != nil {
			return err
		}
	}
	if _, err := command(ctx, ipPath, "netns", "exec", id, "/usr/sbin/sysctl", "-q", "-w", "net.ipv6.conf.all.disable_ipv6=1", "net.ipv6.conf.default.disable_ipv6=1"); err != nil {
		return err
	}
	if _, err := commandInput(ctx, runtime.netRules, ipPath, "netns", "exec", id, nftPath, "-f", "-"); err != nil {
		return err
	}
	for _, args := range [][]string{
		{"-n", id, "link", "set", "lo", "up"},
		{"-n", id, "link", "set", "eth0", "up"},
		{"link", "set", link, "up"},
		{"-n", id, "route", "add", "default", "via", host, "dev", "eth0"},
	} {
		if _, err := command(ctx, ipPath, args...); err != nil {
			return err
		}
	}
	return runtime.checkNetwork(ctx, record)
}

func removeNetwork(ctx context.Context, record *generation) error {
	id := record.Spec.NetworkNamespace
	for _, resource := range []struct {
		filename string
		args     []string
	}{
		{filepath.Join("/sys/class/net", hostLink(id)), []string{"link", "delete", hostLink(id)}},
		{netnsPath(id), []string{"netns", "delete", id}},
	} {
		if _, err := os.Lstat(resource.filename); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return err
		}
		if _, err := command(ctx, ipPath, resource.args...); err != nil {
			return err
		}
	}
	return nil
}

type networkLink struct {
	IfIndex   int      `json:"ifindex"`
	LinkIndex int      `json:"link_index"`
	Flags     []string `json:"flags"`
	AddrInfo  []struct {
		Local     string `json:"local"`
		PrefixLen int    `json:"prefixlen"`
	} `json:"addr_info"`
}

func readLink(ctx context.Context, address string, args ...string) (networkLink, error) {
	data, err := command(ctx, ipPath, args...)
	var links []networkLink
	if err != nil || json.Unmarshal(data, &links) != nil || len(links) != 1 {
		return networkLink{}, errors.New("worker network link missing")
	}
	link := links[0]
	if len(link.AddrInfo) != 1 || link.AddrInfo[0].Local != address || link.AddrInfo[0].PrefixLen != 30 || !strings.Contains(" "+strings.Join(link.Flags, " ")+" ", " UP ") {
		return networkLink{}, errors.New("worker network address or state drift")
	}
	return link, nil
}

func (runtime *Runtime) checkNetwork(ctx context.Context, record *generation) error {
	id := record.Spec.NetworkNamespace
	host, guest := networkAddresses(record)
	hostSide, err := readLink(ctx, host, "-j", "-4", "addr", "show", "dev", hostLink(id))
	if err != nil {
		return err
	}
	guestSide, err := readLink(ctx, guest, "-n", id, "-j", "-4", "addr", "show", "dev", "eth0")
	if err != nil || hostSide.IfIndex <= 0 || guestSide.IfIndex <= 0 || hostSide.LinkIndex != guestSide.IfIndex || guestSide.LinkIndex != hostSide.IfIndex {
		return errors.New("worker network namespace veth pairing unconfirmed")
	}
	routes, err := command(ctx, ipPath, "-n", id, "-4", "route", "show", "default")
	if err != nil || strings.Join(strings.Fields(string(routes)), " ") != "default via "+host+" dev eth0" {
		return errors.New("worker network namespace default route unconfirmed")
	}
	if _, err := command(ctx, nftPath, "list", "table", "inet", "kombify_shared_worker"); err != nil {
		return err
	}
	if _, err := command(ctx, ipPath, "netns", "exec", id, nftPath, "list", "table", "inet", "kombify_workload"); err != nil {
		return err
	}
	return nil
}
