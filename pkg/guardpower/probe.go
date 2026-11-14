package guardpower

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var upsNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._@-]{0,63}$`)

// Probe reads the current power state of one UPS.
type Probe interface {
	Read(context.Context) (Reading, error)
}

// ProbeFunc adapts a function to a Probe.
type ProbeFunc func(context.Context) (Reading, error)

// Read implements Probe.
func (f ProbeFunc) Read(ctx context.Context) (Reading, error) { return f(ctx) }

// Reading is one observation of the UPS.
type Reading struct {
	OnBattery      bool
	LowBattery     bool
	ChargePercent  *float64
	RuntimeSeconds *int64
	Status         string
	Model          string
	ObservedAt     time.Time
}

// NUTConfig configures the NUT (Network UPS Tools) client probe.
type NUTConfig struct {
	// UPS is the NUT UPS name, for example "ups". It is a name, never a path.
	UPS string
	// Binary is the upsc client; an empty value looks up "upsc" on PATH.
	Binary string
	// Timeout bounds one read.
	Timeout time.Duration
	// Run executes the fixed client command. Nil runs the real client.
	Run func(context.Context, string, ...string) ([]byte, error)
}

// NUTProbe reads a UPS through the standard NUT client. The Guard never speaks
// the NUT protocol itself and never accepts a host or a command from a caller:
// the UPS name comes from the host-owned policy and the argv is fixed.
type NUTProbe struct {
	cfg NUTConfig
}

// NewNUTProbe validates a NUT configuration.
func NewNUTProbe(cfg NUTConfig) (*NUTProbe, error) {
	cfg.UPS = strings.TrimSpace(cfg.UPS)
	if !upsNamePattern.MatchString(cfg.UPS) {
		return nil, errors.New("guardpower: the NUT UPS name is not a bounded name")
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 10 * time.Second
	}
	if cfg.Binary == "" {
		cfg.Binary = "upsc"
	}
	if cfg.Run == nil {
		cfg.Run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
			path, err := exec.LookPath(name)
			if err != nil {
				return nil, err
			}
			return exec.CommandContext(ctx, path, args...).Output() // #nosec G204 -- fixed client and host-owned UPS name.
		}
	}
	return &NUTProbe{cfg: cfg}, nil
}

// Read implements Probe.
func (p *NUTProbe) Read(ctx context.Context) (Reading, error) {
	if p == nil {
		return Reading{}, errors.New("guardpower: no UPS probe")
	}
	ctx, cancel := context.WithTimeout(ctx, p.cfg.Timeout)
	defer cancel()
	output, err := p.cfg.Run(ctx, p.cfg.Binary, p.cfg.UPS)
	if err != nil {
		return Reading{}, fmt.Errorf("guardpower: read UPS %q through NUT: %w", p.cfg.UPS, err)
	}
	reading, err := parseNUT(output)
	if err != nil {
		return Reading{}, err
	}
	reading.ObservedAt = time.Now().UTC()
	return reading, nil
}

// parseNUT reads the key: value output of `upsc <name>`. Unknown keys are
// ignored; a document without a status line is refused rather than guessed.
func parseNUT(output []byte) (Reading, error) {
	var reading Reading
	statusSeen := false
	scanner := bufio.NewScanner(bytes.NewReader(output))
	scanner.Buffer(make([]byte, 0, 4096), 256<<10)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		if applyNUTField(&reading, strings.TrimSpace(key), strings.TrimSpace(value)) {
			statusSeen = true
		}
	}
	if err := scanner.Err(); err != nil {
		return Reading{}, fmt.Errorf("guardpower: read the NUT answer: %w", err)
	}
	if !statusSeen {
		return Reading{}, errors.New("guardpower: the NUT answer has no ups.status")
	}
	return reading, nil
}

// applyNUTField records one key and reports whether it was the status line.
func applyNUTField(reading *Reading, key, value string) bool {
	switch key {
	case "ups.status":
		reading.Status = value
		reading.OnBattery = hasStatusToken(value, "OB") && !hasStatusToken(value, "OL")
		reading.LowBattery = hasStatusToken(value, "LB") || hasStatusToken(value, "FSD")
		return true
	case "battery.charge":
		if charge, err := strconv.ParseFloat(value, 64); err == nil && charge >= 0 && charge <= 100 {
			reading.ChargePercent = &charge
		}
	case "battery.runtime":
		if runtime, err := strconv.ParseInt(value, 10, 64); err == nil && runtime >= 0 {
			reading.RuntimeSeconds = &runtime
		}
	case "device.model", "ups.model":
		if reading.Model == "" && len(value) <= 64 {
			reading.Model = value
		}
	}
	return false
}

func hasStatusToken(status, token string) bool {
	for _, field := range strings.Fields(status) {
		if field == token {
			return true
		}
	}
	return false
}
