// Package wikipush delivers one Wiki source document to the person's Space,
// signed as the Homelab Runtime with a short-lived Ed25519 service-call token
// (AUTHENTICATION.md §8, the `homelab` caller). The target route is the agent
// host's `POST /v1/discussion/spaces/{space}/wiki/homelab`, which derives the
// tenant from the token's on-behalf-of owner and never from the body.
package wikipush

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/kombifyio/homelab/internal/gocommon/servicecall"

	"github.com/kombifyio/homelab/internal/wikisource"
	"github.com/kombifyio/homelab/pkg/serviceauth"
)

const (
	// Caller is the service-call caller names this runtime.
	Caller = "homelab"
	// Target is the audience, the agent host.
	Target = "agents"
	// Scope is the one route authority this token carries.
	Scope = "wiki:homelab-source"
	// BodyLimit bounds the document one report pushes.
	BodyLimit = 4 << 20
)

// ErrRefused reports that the agent host verified the token and refused the
// document; the message names its reason code and never the token.
var ErrRefused = errors.New("wikipush: the Wiki refused this source document")

// Delivery is the agent host's answer.
type Delivery struct {
	Status   int    `json:"status"`
	Space    string `json:"space,omitempty"`
	Accepted bool   `json:"accepted"`
	Reason   string `json:"reason,omitempty"`
}

type reportRequest struct {
	Document wikisource.Document `json:"document"`
}

type reportAnswer struct {
	Accepted bool   `json:"accepted"`
	Space    string `json:"space,omitempty"`
	Code     string `json:"code,omitempty"`
	Message  string `json:"message,omitempty"`
	Sources  int    `json:"sources,omitempty"`
}

// Client pushes documents to one configured origin and Space.
type Client struct {
	origin string
	space  string
	call   servicecall.Client
	http   *http.Client
}

// New builds the client from the deployment configuration. origin must be an
// https origin without path, query, fragment or user info; space is the Wiki
// Space the person's Homelab reports into.
func New(origin, space, serviceName string, tokenTTL time.Duration) (*Client, error) {
	parsed, err := url.Parse(strings.TrimSpace(origin))
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil ||
		(parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("wikipush: a fixed https origin is required")
	}
	space = strings.TrimSpace(space)
	if space == "" {
		return nil, errors.New("wikipush: a Wiki Space slug is required")
	}
	caller := strings.TrimSpace(serviceName)
	if caller == "" {
		caller = Caller
	}
	config, err := serviceauth.Apply(servicecall.Config{
		ServiceName: caller,
		Target:      Target,
		Scope:       Scope,
		TokenTTL:    tokenTTL,
	})
	if err != nil {
		return nil, err
	}
	call, err := servicecall.NewClient(config)
	if err != nil {
		return nil, err
	}
	return &Client{
		origin: strings.TrimRight(strings.TrimSpace(origin), "/"),
		space:  space,
		call:   *call,
		http:   &http.Client{Timeout: 30 * time.Second},
	}, nil
}

// Report pushes one document on behalf of owner. The owner is the token's
// on-behalf-of subject and never comes from the document body.
func (c *Client) Report(ctx context.Context, owner, org string, document wikisource.Document) (Delivery, error) {
	if c == nil {
		return Delivery{}, errors.New("wikipush: not configured")
	}
	owner = strings.TrimSpace(owner)
	if owner == "" {
		return Delivery{}, errors.New("wikipush: an owner is required")
	}
	document.OwnerSubjectID = owner
	body, err := json.Marshal(reportRequest{Document: document})
	if err != nil {
		return Delivery{}, err
	}
	if len(body) > BodyLimit {
		return Delivery{}, fmt.Errorf("wikipush: document exceeds %d bytes", BodyLimit)
	}
	endpoint := fmt.Sprintf("%s/v1/discussion/spaces/%s/wiki/homelab", c.origin, url.PathEscape(c.space))
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return Delivery{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := c.call.Do(ctx, request, &servicecall.OnBehalfOf{Sub: owner, OrgID: strings.TrimSpace(org)})
	if err != nil {
		return Delivery{}, err
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	var answer reportAnswer
	_ = json.Unmarshal(raw, &answer)
	delivery := Delivery{Status: response.StatusCode, Space: answer.Space, Accepted: answer.Accepted, Reason: answer.Code}
	if response.StatusCode < 200 || response.StatusCode >= 300 || !answer.Accepted {
		if delivery.Reason == "" {
			delivery.Reason = strings.TrimSpace(answer.Message)
		}
		if delivery.Reason == "" {
			delivery.Reason = fmt.Sprintf("status_%d", response.StatusCode)
		}
		return delivery, fmt.Errorf("%w: %s", ErrRefused, delivery.Reason)
	}
	return delivery, nil
}
