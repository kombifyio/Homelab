package routes

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/kombifyio/homelab/internal/routes/tenantguard"
	ksapi "github.com/kombifyio/homelab/pkg/api"
	"github.com/kombifyio/homelab/pkg/controlplane"
	"github.com/kombifyio/homelab/pkg/httpx"
)

// The creation draft surface (KOMBIFY-DESIGN-SYSTEM-STANDARD.md section 9).
//
// Every frame of the creation flow (page, sheet, single step, the embedded
// module in the Cloud popup) keeps where the owner stands under one key per
// process and target, so "Continue in Homelab" or another device opens the
// same step with the same answers. Only the signed-in owner reads or writes
// their drafts, in their tenant. A draft never holds a secret: a field that
// carries a password, passphrase, token, key or other secret is refused here,
// whatever the client sent.

const (
	maxCreationDraftBodyBytes = controlplane.MaxCreationDraftAnswersBytes + 1<<10
	creationDraftCapability   = "homelab.creation.draft"

	creationDraftReasonInvalid        = "creation_draft_invalid"
	creationDraftReasonSecret         = "creation_draft_secret_field"
	creationDraftReasonNoControlPlane = "creation_draft_requires_control_plane"
)

// creationDraftWriteRequest is the closed wire contract of one write.
type creationDraftWriteRequest struct {
	Process  string          `json:"process"`
	Step     string          `json:"step"`
	Furthest string          `json:"furthest"`
	Answers  json.RawMessage `json:"answers"`
}

// creationDraftView is what a frame reads back.
type creationDraftView struct {
	Key       string          `json:"key"`
	Process   string          `json:"process"`
	Step      string          `json:"step"`
	Furthest  string          `json:"furthest"`
	Answers   json.RawMessage `json:"answers"`
	ExpiresAt time.Time       `json:"expires_at"`
	UpdatedAt time.Time       `json:"updated_at"`
}

type creationDraftResponse struct {
	Draft *creationDraftView `json:"draft"`
}

type creationDraftAPI struct {
	drafts controlplane.CreationDraftStore
}

func registerCreationDraftRoutes(r *httpx.Router, drafts controlplane.CreationDraftStore) {
	api := &creationDraftAPI{drafts: drafts}
	r.GET("/api/v1/creation/drafts/{key}", api.handleGet)
	r.PUT("/api/v1/creation/drafts/{key}", api.handlePut)
	r.DELETE("/api/v1/creation/drafts/{key}", api.handleDelete)
}

func (api *creationDraftAPI) handleGet(e *httpx.Event) error {
	tenantID, ownerID, key, err := api.scope(e)
	if err != nil {
		return err
	}
	stored, err := api.drafts.GetCreationDraft(e.Request.Context(), tenantID, ownerID, key)
	if errors.Is(err, controlplane.ErrNotFound) {
		return httpx.Success(e, http.StatusOK, creationDraftResponse{})
	}
	if err != nil {
		return httpx.InternalError(e, "failed to read the creation draft")
	}
	return httpx.Success(e, http.StatusOK, creationDraftResponse{Draft: creationDraftViewOf(stored)})
}

func (api *creationDraftAPI) handlePut(e *httpx.Event) error {
	tenantID, ownerID, key, err := api.scope(e)
	if err != nil {
		return err
	}
	body, tooLarge, readErr := readRequestBodyLimited(e.Request.Body, maxCreationDraftBodyBytes)
	if readErr != nil {
		return httpx.BadRequest(e, "failed to read the creation draft")
	}
	if tooLarge {
		return httpx.Error(e, http.StatusRequestEntityTooLarge, ksapi.ErrCodeBadRequest, "request body exceeds size limit", nil)
	}
	var request creationDraftWriteRequest
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	decodeErr := decoder.Decode(&request)
	if decodeErr == nil {
		var trailing any
		if trailingErr := decoder.Decode(&trailing); trailingErr != io.EOF {
			decodeErr = errors.New("request must contain one JSON object")
		}
	}
	if decodeErr != nil {
		return creationDraftRefusal(e, creationDraftReasonInvalid, "Creation draft is not valid: "+decodeErr.Error())
	}

	saved, err := api.drafts.PutCreationDraft(e.Request.Context(), controlplane.CreationDraft{
		ID:             uuid.NewString(),
		TenantID:       tenantID,
		OwnerSubjectID: ownerID,
		Key:            key,
		Process:        strings.TrimSpace(request.Process),
		Step:           strings.TrimSpace(request.Step),
		Furthest:       strings.TrimSpace(request.Furthest),
		Answers:        request.Answers,
	})
	// The message names the refused field, never its value.
	if errors.Is(err, controlplane.ErrCreationDraftSecret) {
		return creationDraftRefusal(e, creationDraftReasonSecret, "The "+err.Error())
	}
	if errors.Is(err, controlplane.ErrCreationDraftInvalid) {
		return creationDraftRefusal(e, creationDraftReasonInvalid, "The "+err.Error())
	}
	if err != nil {
		return httpx.InternalError(e, "failed to store the creation draft")
	}
	return httpx.Success(e, http.StatusOK, creationDraftResponse{Draft: creationDraftViewOf(saved)})
}

func (api *creationDraftAPI) handleDelete(e *httpx.Event) error {
	tenantID, ownerID, key, err := api.scope(e)
	if err != nil {
		return err
	}
	if err := api.drafts.DeleteCreationDraft(e.Request.Context(), tenantID, ownerID, key); err != nil {
		return httpx.InternalError(e, "failed to discard the creation draft")
	}
	return httpx.Success(e, http.StatusOK, creationDraftResponse{})
}

// scope authenticates the caller, resolves the tenant and the key, and fails
// closed when there is nowhere to keep a draft.
func (api *creationDraftAPI) scope(e *httpx.Event) (string, string, string, error) {
	ownerID, err := requireUnifierAuth(e)
	if err != nil {
		return "", "", "", err
	}
	tenantID, err := tenantguard.TenantScope(requestExplicitTenantID(e), ownerID, creationDraftCapability)
	if err != nil {
		return "", "", "", err
	}
	key := e.Request.PathValue("key")
	if !controlplane.ValidCreationDraftKey(key) {
		return "", "", "", httpx.Reject(e, http.StatusBadRequest, ksapi.ErrCodeBadRequest,
			"Creation draft key is not valid", map[string]any{
				"reason_code": creationDraftReasonInvalid,
				"retryable":   false,
			})
	}
	if api.drafts == nil {
		return "", "", "", httpx.Reject(e, http.StatusServiceUnavailable, ksapi.ErrCodeUnavailable,
			"Creation drafts require the control-plane database", map[string]any{
				"reason_code": creationDraftReasonNoControlPlane,
				"retryable":   true,
			})
	}
	return tenantID, ownerID, key, nil
}

func creationDraftRefusal(e *httpx.Event, reason, message string) error {
	return httpx.Error(e, http.StatusBadRequest, ksapi.ErrCodeBadRequest, message, map[string]any{
		"reason_code": reason,
		"retryable":   false,
	})
}

func creationDraftViewOf(stored *controlplane.CreationDraft) *creationDraftView {
	if stored == nil {
		return nil
	}
	return &creationDraftView{
		Key:       stored.Key,
		Process:   stored.Process,
		Step:      stored.Step,
		Furthest:  stored.Furthest,
		Answers:   stored.Answers,
		ExpiresAt: stored.ExpiresAt,
		UpdatedAt: stored.UpdatedAt,
	}
}
