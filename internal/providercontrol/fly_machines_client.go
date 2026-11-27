package providercontrol

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"
)

const (
	flyMachinesAPIBase      = "https://api.machines.dev/v1"
	flyMachineResponseLimit = 1 << 20
)

var (
	ErrFlyMachineResponse   = errors.New("providercontrol: invalid Fly Machines API response")
	flyMachinePathIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
)

type FlyMachinesAPIError struct {
	StatusCode int
}

func (e *FlyMachinesAPIError) Error() string {
	return fmt.Sprintf("providercontrol: Fly Machines API returned HTTP %d", e.StatusCode)
}

type flyMachinesClient struct {
	base  string
	token string
	http  *http.Client
}

func newFlyMachinesClient(base, token string) flyMachinesClient {
	return flyMachinesClient{
		base: base, token: token,
		http: &http.Client{
			Timeout: 30 * time.Second,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

func (c flyMachinesClient) request(ctx context.Context, method, path string, payload, result any) error {
	var body io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return ErrFlyMachineSpec
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return ErrFlyMachineSpec
	}
	req.Header.Set("Authorization", flyAuthorizationHeader(c.token))
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	response, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("%w: transport failure", ErrFlyMachineResponse)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound && (method == http.MethodGet || method == http.MethodDelete) {
		return ErrFlyMachineNotFound
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return &FlyMachinesAPIError{StatusCode: response.StatusCode}
	}
	if result == nil {
		return nil
	}
	if response.StatusCode != http.StatusOK && (method != http.MethodPost || response.StatusCode != http.StatusCreated) {
		return ErrFlyMachineResponse
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, flyMachineResponseLimit+1))
	if err != nil || len(data) > flyMachineResponseLimit || json.Unmarshal(data, result) != nil {
		return ErrFlyMachineResponse
	}
	return nil
}

func flyAuthorizationHeader(token string) string {
	if strings.HasPrefix(token, "FlyV1 ") {
		return token
	}
	return "Bearer " + token
}

func flyMachinePathID(value string) bool {
	return flyMachinePathIDPattern.MatchString(value)
}

type flyAppInfo struct {
	Name    string `json:"name"`
	Network string `json:"network"`
}

type flyMachineCreate struct {
	Name   string           `json:"name,omitempty"`
	Region string           `json:"region"`
	Config flyMachineConfig `json:"config"`
}

type flyMachine struct {
	ID     string                  `json:"id"`
	Name   string                  `json:"name,omitempty"`
	Region string                  `json:"region"`
	State  string                  `json:"state"`
	Config flyMachineConfig        `json:"config"`
	Checks []flyMachineCheckStatus `json:"checks"`
}

type flyMachineConfig struct {
	Image    string                     `json:"image"`
	Metadata map[string]string          `json:"metadata"`
	Guest    flyMachineGuest            `json:"guest"`
	Restart  *flyMachineRestart         `json:"restart,omitempty"`
	Services []flyMachineService        `json:"services"`
	Checks   map[string]flyMachineCheck `json:"checks"`
	Mounts   []json.RawMessage          `json:"mounts,omitempty"`
}

type flyMachineRestart struct {
	Policy string `json:"policy"`
}

type flyMachineGuest struct {
	CPUKind     string `json:"cpu_kind"`
	CPUs        int    `json:"cpus"`
	MemoryMB    int    `json:"memory_mb"`
	MaxMemoryMB int    `json:"max_memory_mb"`
}

type flyMachineService struct {
	Protocol           string           `json:"protocol"`
	InternalPort       int              `json:"internal_port"`
	Autostop           json.RawMessage  `json:"autostop"`
	Autostart          bool             `json:"autostart"`
	MinMachinesRunning int              `json:"min_machines_running"`
	Ports              []flyMachinePort `json:"ports"`
}

type flyMachinePort struct {
	Port       int      `json:"port"`
	Handlers   []string `json:"handlers"`
	ForceHTTPS bool     `json:"force_https,omitempty"`
}

type flyMachineCheck struct {
	Type     string `json:"type"`
	Port     int    `json:"port"`
	Interval string `json:"interval"`
	Timeout  string `json:"timeout"`
}

type flyMachineCheckStatus struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}

func (machine flyMachine) satisfiesPolicy() bool {
	config := machine.Config
	guest := config.Guest
	if config.Image == "" || config.Restart == nil || config.Restart.Policy != "on-failure" ||
		guest.CPUKind != "shared" || guest.CPUs != 1 || !flyMachineMemoryAllowed(guest.MemoryMB) ||
		guest.MaxMemoryMB != guest.MemoryMB || len(config.Mounts) != 0 || len(config.Services) != 1 {
		return false
	}
	service := config.Services[0]
	if service.Protocol != "tcp" || service.InternalPort < 1 || service.InternalPort > 65535 ||
		!service.Autostart || service.MinMachinesRunning != 0 || len(service.Ports) != 1 {
		return false
	}
	switch string(service.Autostop) {
	case `"stop"`, "true":
	default:
		return false
	}
	port := service.Ports[0]
	check := config.Checks["workload"]
	return port.Port == 80 && !port.ForceHTTPS && slices.Equal(port.Handlers, []string{"http"}) &&
		check.Type == "tcp" && check.Port == service.InternalPort
}

func (machine flyMachine) health() FlyMachineHealth {
	if machine.State != "started" || !machine.satisfiesPolicy() {
		return FlyMachineHealthUnknown
	}
	health := FlyMachineHealthUnknown
	for _, check := range machine.Checks {
		if check.Name != "workload" {
			continue
		}
		switch check.Status {
		case "warning", "critical":
			return FlyMachineHealthUnhealthy
		case "passing":
			health = FlyMachineHealthHealthy
		}
	}
	return health
}
