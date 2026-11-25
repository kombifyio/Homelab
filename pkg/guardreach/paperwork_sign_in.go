package guardreach

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/kombifyio/homelab/pkg/guardidentity"
)

const paperworkSignInScope = "connect:homelab-nodes:paperwork-sign-in"

var (
	ErrPaperworkSignInUnavailable = errors.New("linked Paperwork sign-in is unavailable")
	ErrPaperworkSignInNotReady    = errors.New("the current Guard must have an active Paperwork route")
	paperworkSignInDigest         = regexp.MustCompile(`^[0-9a-f]{64}$`)
	paperworkSignInTenant         = regexp.MustCompile(`^org_[A-Za-z0-9]+$`)
)

type PaperworkSignInProgress struct {
	Reservation string `json:"reservation,omitempty"`
	Origin      string `json:"origin,omitempty"`
	Generation  int64  `json:"generation,omitempty"`
	State       string `json:"state,omitempty"`
	ApprovedAt  string `json:"approved_at,omitempty"`
}

func (progress PaperworkSignInProgress) active() bool {
	return progress.Reservation != "" && progress.State != "revoked"
}

type PaperworkSignInView struct {
	State     string `json:"state"`
	CanEnable bool   `json:"can_enable"`
}

type PaperworkSignInTask struct {
	Kind        string `json:"kind"`
	Reservation string `json:"reservation"`
	Origin      string `json:"origin"`
	TenantID    string `json:"tenant_id"`
	ServerID    string `json:"server_id"`
	Generation  int64  `json:"generation"`
	Envelope    string `json:"envelope,omitempty"`
	Steady      bool   `json:"steady,omitempty"`
}

type PaperworkSignInCommand struct {
	Kind           string          `json:"kind"`
	Reservation    string          `json:"reservation"`
	Origin         string          `json:"origin"`
	ServerID       string          `json:"server_id"`
	OwnerApproved  bool            `json:"owner_approved,omitempty"`
	Recipient      json.RawMessage `json:"recipient,omitempty"`
	EnvelopeSHA256 string          `json:"envelope_sha256,omitempty"`
}

type PaperworkSignInReport struct {
	Body  string `json:"body"`
	Proof string `json:"proof"`
}

type paperworkSignInReceipt struct {
	State       string `json:"state"`
	Reservation string `json:"reservation"`
	Envelope    string `json:"envelope"`
}

type paperworkSignInGateway interface {
	paperworkSignIn(context.Context, Row, PaperworkSignInReport) (paperworkSignInReceipt, error)
}

func (g *Gateway) paperworkSignIn(ctx context.Context, row Row, report PaperworkSignInReport) (paperworkSignInReceipt, error) {
	var receipt paperworkSignInReceipt
	_, err := g.do(ctx, paperworkSignInScope, http.MethodPost, gatewayNodesPath+"/paperwork-sign-in", []byte(report.Body),
		g.obo(row.OwnerSubjectID, row.ConnectTenantID), &receipt, report.Proof)
	return receipt, err
}

func paperworkRouteReady(row Row) bool {
	return row.Desired == DesiredActive && row.Generation > 0 && row.KeyThumbprint != "" &&
		row.Connect.registeredAt(row.Generation) && row.Me.registeredAt(row.Generation) &&
		row.ConnectTenantID == row.TenantID && paperworkSignInTenant.MatchString(row.TenantID) &&
		row.Public.Enabled && row.Public.Service == paperworkService && row.Public.TargetAddr() == paperworkTarget &&
		strings.HasSuffix(row.Me.Ref, "."+meZoneDomain)
}

func (progress PaperworkSignInProgress) matches(row Row) bool {
	return progress.Generation == row.Generation && progress.Origin == "https://"+row.Me.Ref && paperworkRouteReady(row)
}

func paperworkSignInView(row Row) *PaperworkSignInView {
	state := row.PaperworkSignIn.State
	if !row.PaperworkSignInAvailable && (state == "" || state == "revoked") {
		return nil
	}
	canEnable := row.PaperworkSignInAvailable && paperworkRouteReady(row) && (state == "" || state == "revoked")
	switch {
	case state == "":
		state = "off"
	case state == "revoked":
	case !row.PaperworkSignInAvailable || !row.PaperworkSignIn.matches(row):
		state = "revoking"
	case state == "enable" || state == "checking" || state == "read":
		state = "pending"
	}
	return &PaperworkSignInView{State: state, CanEnable: canEnable}
}

func (s *Service) paperworkAvailable() bool {
	_, wired := s.cfg.Connect.(paperworkSignInGateway)
	return s.cfg.PaperworkSignIn && s.cfg.PaperworkRoutes != nil && wired
}

func (s *Service) SetPaperworkSignIn(ctx context.Context, tenantID, workerID, owner string, enabled bool) (*Row, error) {
	available := s.paperworkAvailable()
	if enabled && !available {
		return nil, ErrPaperworkSignInUnavailable
	}
	var reservation uuid.UUID
	var err error
	if enabled {
		reservation, err = uuid.NewRandom()
		if err != nil {
			return nil, err
		}
	}
	now := s.now()
	var refused error
	row, err := s.cfg.Store.Update(ctx, tenantID, workerID, func(current *Row) bool {
		if current.OwnerSubjectID != owner {
			refused = ErrNotFound
			return false
		}
		progress := current.PaperworkSignIn
		if !enabled {
			if progress.State == "" || progress.State == "revoked" || progress.State == "revoking" {
				return false
			}
			current.PaperworkSignIn.State = "revoking"
			return true
		}
		if !paperworkRouteReady(*current) || progress.State == "revoking" {
			refused = ErrPaperworkSignInNotReady
			return false
		}
		if progress.State != "" && progress.State != "revoked" {
			return false
		}
		current.PaperworkSignIn = PaperworkSignInProgress{
			Reservation: reservation.String(), Origin: "https://" + current.Me.Ref, Generation: current.Generation,
			State: "enable", ApprovedAt: now.UTC().Format(time.RFC3339),
		}
		return true
	})
	if err != nil || refused != nil {
		return nil, errors.Join(err, refused)
	}
	row.PaperworkSignInAvailable = available
	return row, nil
}

func (s *Service) paperworkOriginRefresh(row Row) bool {
	return s.paperworkAvailable() && row.PaperworkSignIn.State == "enable" && row.PaperworkSignIn.matches(row)
}

func (s *Service) paperworkTask(row Row) *PaperworkSignInTask {
	progress := row.PaperworkSignIn
	if !progress.active() || !row.Connect.registeredAt(row.Generation) {
		return nil
	}
	kind := "read"
	switch {
	case progress.State == "revoking" || !s.paperworkAvailable() || !progress.matches(row):
		kind = "revoke"
	case progress.State == "enable":
		kind = "enable"
	}
	return &PaperworkSignInTask{Kind: kind, Reservation: progress.Reservation, Origin: progress.Origin,
		TenantID: row.ConnectTenantID, ServerID: row.ServerID, Generation: row.Generation,
		Steady: kind == "read" && progress.State == "delivered"}
}

func (s *Service) advancePaperworkSignIn(ctx context.Context, row Row, report *PaperworkSignInReport) *PaperworkSignInTask {
	task := s.paperworkTask(row)
	gateway, wired := s.cfg.Connect.(paperworkSignInGateway)
	if task == nil || report == nil || !wired || len(report.Body) > 8<<10 {
		return nil
	}
	var command PaperworkSignInCommand
	decoder := json.NewDecoder(strings.NewReader(report.Body))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&command) != nil || decoder.Decode(new(json.RawMessage)) != io.EOF ||
		command.Origin != task.Origin || command.ServerID != task.ServerID || command.Reservation != task.Reservation {
		return nil
	}
	switch command.Kind {
	case "enable":
		if task.Kind != "enable" || !command.OwnerApproved || len(command.Recipient) == 0 || command.Recipient[0] != '{' || command.EnvelopeSHA256 != "" {
			return nil
		}
	case "ack":
		if task.Kind != "read" || !paperworkSignInDigest.MatchString(command.EnvelopeSHA256) || command.OwnerApproved || command.Recipient != nil {
			return nil
		}
	default:
		if task.Kind != command.Kind || command.OwnerApproved || command.Recipient != nil || command.EnvelopeSHA256 != "" {
			return nil
		}
	}
	digest := sha256.Sum256([]byte(report.Body))
	proof, err := guardidentity.VerifyHostProof(report.Proof, guardidentity.HostProofExpectation{
		Issuer: row.WorkerID, Subject: row.ServerID, TenantID: row.ConnectTenantID, Generation: row.Generation,
		Audience: guardidentity.PaperworkSignInAudience(hex.EncodeToString(digest[:])), Now: s.now(),
	})
	if err != nil || proof.Thumbprint != row.KeyThumbprint {
		return nil
	}
	progress := row.PaperworkSignIn
	if command.Kind == "enable" {
		if !s.transitionPaperworkSignIn(ctx, row, progress, "checking") {
			return nil
		}
		progress.State = "checking"
	}
	receipt, err := gateway.paperworkSignIn(ctx, row, *report)
	if err != nil {
		var denial *GatewayError
		if errors.As(err, &denial) && progress.State == "checking" {
			switch {
			case command.Kind == "enable" && denial.ReasonCode == "not_enabled":
				s.transitionPaperworkSignIn(ctx, row, progress, "revoking")
			case command.Kind == "read" && denial.Status == http.StatusConflict:
				s.transitionPaperworkSignIn(ctx, row, progress, "enable")
			}
		}
		return nil
	}
	if receipt.State == "revoked" {
		s.transitionPaperworkSignIn(ctx, row, progress, "revoked")
		return nil
	}
	if receipt.Reservation != task.Reservation || command.Kind == "revoke" {
		return nil
	}
	switch receipt.State {
	case "pending", "ready":
		if !s.transitionPaperworkSignIn(ctx, row, progress, "read") {
			return nil
		}
		if command.Kind == "read" && receipt.State == "ready" && len(receipt.Envelope) > 0 && len(receipt.Envelope) <= 32<<10 && json.Valid(bytes.TrimSpace([]byte(receipt.Envelope))) {
			task.Kind, task.Envelope = "apply", receipt.Envelope
			return task
		}
	case "pending_reconcile", "delivered":
		s.transitionPaperworkSignIn(ctx, row, progress, receipt.State)
	}
	return nil
}

func (s *Service) transitionPaperworkSignIn(ctx context.Context, row Row, progress PaperworkSignInProgress, state string) bool {
	changed := false
	_, err := s.cfg.Store.Update(ctx, row.TenantID, row.WorkerID, func(current *Row) bool {
		if current.PaperworkSignIn != progress || (state != "revoked" && !paperworkRouteReady(*current)) || current.Generation != row.Generation ||
			current.Me.Ref != row.Me.Ref || current.OwnerSubjectID != row.OwnerSubjectID || current.KeyThumbprint != row.KeyThumbprint {
			return false
		}
		changed = true
		current.PaperworkSignIn.State = state
		return progress.State != state
	})
	return changed && err == nil
}
