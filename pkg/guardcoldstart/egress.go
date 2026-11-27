package guardcoldstart

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strings"
	"sync"
)

// Classes of one outbound attempt.
const (
	EgressLocal        = "local"
	EgressControlPlane = "control_plane"
	EgressWAN          = "wan"
)

// ErrEgressDenied is returned for every attempt the rehearsal refuses.
var ErrEgressDenied = errors.New("guardcoldstart: outbound access is cut during the rehearsal")

// ErrNoLocalDialer means a local attempt had no dialer to carry it.
var ErrNoLocalDialer = errors.New("guardcoldstart: no local dialer")

// DialFunc is the shape of net.Dialer.DialContext.
type DialFunc func(ctx context.Context, network, address string) (net.Conn, error)

// EgressAttempt is one recorded outbound attempt.
type EgressAttempt struct {
	Address string `json:"address"`
	Class   string `json:"class"`
	Allowed bool   `json:"allowed"`
}

// Egress is the only network a rehearsal step may use. It records every
// attempt, passes an attempt to the local dialer only when the address is an IP
// literal inside the plan's local prefixes that is no declared remote
// dependency, and refuses the rest. A step that reaches for DNS, a registry,
// the identity provider or the control plane is recorded, denied and fails the
// rehearsal.
type Egress struct {
	prefixes []netip.Prefix
	remote   map[string]string
	local    DialFunc

	mu       sync.Mutex
	attempts []EgressAttempt
}

// NewEgress builds the egress of one plan. local may be nil when nothing is
// expected to dial.
func NewEgress(plan Plan, local DialFunc) (*Egress, error) {
	e := &Egress{remote: map[string]string{}, local: local}
	for _, p := range plan.LocalPrefixes {
		prefix, err := netip.ParsePrefix(p)
		if err != nil {
			return nil, err
		}
		e.prefixes = append(e.prefixes, prefix.Masked())
	}
	for _, d := range plan.Dependencies {
		e.remote[strings.ToLower(d.Host)] = d.Kind
	}
	return e, nil
}

// Dial is the DialFunc handed to every step.
func (e *Egress) Dial(ctx context.Context, network, address string) (net.Conn, error) {
	class, allowed := e.classify(address)
	e.mu.Lock()
	e.attempts = append(e.attempts, EgressAttempt{Address: address, Class: class, Allowed: allowed})
	e.mu.Unlock()
	if !allowed {
		return nil, ErrEgressDenied
	}
	if e.local == nil {
		return nil, ErrNoLocalDialer
	}
	return e.local(ctx, network, address)
}

// Attempts returns a copy of the recorded attempts in order.
func (e *Egress) Attempts() []EgressAttempt {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]EgressAttempt(nil), e.attempts...)
}

// Violations counts the attempts that were refused.
func (e *Egress) Violations() int {
	n := 0
	for _, a := range e.Attempts() {
		if !a.Allowed {
			n++
		}
	}
	return n
}

func (e *Egress) classify(address string) (string, bool) {
	host := address
	if h, _, err := net.SplitHostPort(address); err == nil {
		host = h
	}
	host = strings.ToLower(strings.Trim(host, "[]"))
	if kind, declared := e.remote[host]; declared {
		if kind == DependencyControlPlane {
			return EgressControlPlane, false
		}
		return EgressWAN, false
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		// A name needs resolution, and remote DNS is cut.
		return EgressWAN, false
	}
	ip = ip.Unmap()
	for _, prefix := range e.prefixes {
		if prefix.Contains(ip) {
			return EgressLocal, true
		}
	}
	return EgressWAN, false
}
