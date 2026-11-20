package stackkitdecision

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"
)

// The refusal names the platform binds (workspace contract, part 4). A check
// in Homelab answers with the same names the node uses.
const (
	RefusalInvalid        = "stackkit_receipt_invalid"
	RefusalExpired        = "stackkit_receipt_expired"
	RefusalSpent          = "stackkit_receipt_spent"
	RefusalTargetMismatch = "stackkit_receipt_target_mismatch"
)

const (
	// ReceiptType is the JWS header type the node requires.
	ReceiptType = "kombify.decision-receipt.v1"
	// MaxLifetime is the longest receipt window the node admits.
	MaxLifetime = 900 * time.Second
	// DispatchMargin is the window a receipt must still have when Homelab
	// hands it to a node: a receipt that lapses on the way is refused there
	// anyway, so it is not sent.
	DispatchMargin  = 30 * time.Second
	maxCompactBytes = 16384
)

var signaturePart = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// Refusal is a named refusal; the name is the contract.
type Refusal struct {
	Name    string
	Message string
}

func (r *Refusal) Error() string { return r.Name + ": " + r.Message }

func refuse(name, format string, args ...any) *Refusal {
	return &Refusal{Name: name, Message: fmt.Sprintf(format, args...)}
}

// Receipt is the ledger's receipt object as its read route returns it: the
// signed claims and the detached signature {alg, kid, value}.
type Receipt struct {
	ReceiptID    string `json:"receiptId"`
	DecisionID   string `json:"decisionId"`
	OperationID  string `json:"operationId"`
	SubjectStore string `json:"subjectStore"`
	IssuedAt     string `json:"issuedAt"`
	ExpiresAt    string `json:"expiresAt"`
	Outcome      string `json:"outcome"`
	OptionKind   string `json:"optionKind"`
	AnsweredBy   struct {
		Kind string `json:"kind"`
		ID   string `json:"id"`
	} `json:"answeredBy"`
	Surface      string `json:"surface"`
	Assurance    string `json:"assurance"`
	IntentDigest string `json:"intentDigest"`
	Lifetime     struct {
		Scope string `json:"scope"`
	} `json:"lifetime"`
	Signature struct {
		Alg   string `json:"alg"`
		Kid   string `json:"kid"`
		Value string `json:"value"`
	} `json:"signature"`
}

// Admitted is one receipt Homelab checked and may hand to the node.
type Admitted struct {
	ReceiptID  string
	DecisionID string
	ExpiresAt  time.Time
	// Compact is the compact JWS the node verifies and spends.
	Compact string
}

// Check verifies what Homelab can verify before anything is dispatched:
// the store, the outcome and option kind, that a person answered on a surface
// other than equipment, single use, that the intent digest equals the one
// Homelab filed, that the decision is the one it filed, and the time
// window. The signature is the node's to verify against its purpose-key
// snapshot; Homelab only rebuilds the exact signed bytes.
func Check(raw json.RawMessage, filedDecisionID, filedIntentDigest string, now time.Time) (*Admitted, error) {
	var receipt Receipt
	if len(raw) == 0 || len(raw) > maxCompactBytes || json.Unmarshal(raw, &receipt) != nil {
		return nil, refuse(RefusalInvalid, "the ledger returned no readable receipt")
	}
	if receipt.SubjectStore != SubjectStore {
		return nil, refuse(RefusalInvalid, "the receipt is for store %q, not %q", receipt.SubjectStore, SubjectStore)
	}
	if receipt.Outcome != "allowed" || receipt.OptionKind != "allow_once" {
		return nil, refuse(RefusalInvalid, "the receipt does not allow this call once")
	}
	if receipt.AnsweredBy.Kind != "person" || receipt.AnsweredBy.ID == "" || receipt.Surface == "" || receipt.Surface == "equipment" {
		return nil, refuse(RefusalInvalid, "the receipt was not answered by a person on an admitted surface")
	}
	if receipt.Lifetime.Scope != "once" {
		return nil, refuse(RefusalInvalid, "the receipt does not cover a single use")
	}
	issuedAt, issuedErr := time.Parse(time.RFC3339, receipt.IssuedAt)
	expiresAt, expiresErr := time.Parse(time.RFC3339, receipt.ExpiresAt)
	if issuedErr != nil || expiresErr != nil || !issuedAt.Before(expiresAt) || expiresAt.Sub(issuedAt) > MaxLifetime {
		return nil, refuse(RefusalInvalid, "the receipt time window is invalid")
	}
	if receipt.DecisionID != filedDecisionID || receipt.IntentDigest != filedIntentDigest {
		return nil, refuse(RefusalTargetMismatch, "the receipt binds another decision or intent than the one filed")
	}
	if !now.Add(DispatchMargin).Before(expiresAt) {
		return nil, refuse(RefusalExpired, "the receipt is outside its time window")
	}
	compact, err := CompactJWS(raw)
	if err != nil {
		return nil, refuse(RefusalInvalid, "%v", err)
	}
	return &Admitted{ReceiptID: receipt.ReceiptID, DecisionID: receipt.DecisionID, ExpiresAt: expiresAt.UTC(), Compact: compact}, nil
}

// CompactJWS rebuilds the compact JWS the node verifies from the ledger's
// receipt object: base64url(canonical header {alg, kid, typ}) "."
// base64url(canonical receipt without its signature member) "."
// signature.value, as the consumer's "The carrier" section defines it.
func CompactJWS(raw json.RawMessage) (string, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var document map[string]any
	if err := decoder.Decode(&document); err != nil {
		return "", errors.New("the receipt is not a JSON object")
	}
	signature, ok := document["signature"].(map[string]any)
	if !ok {
		return "", errors.New("the receipt carries no signature")
	}
	alg, _ := signature["alg"].(string)
	kid, _ := signature["kid"].(string)
	value, _ := signature["value"].(string)
	if alg == "" || kid == "" || !signaturePart.MatchString(value) {
		return "", errors.New("the receipt signature is incomplete")
	}
	delete(document, "signature")
	header, err := canonicalJSON(map[string]any{"alg": alg, "kid": kid, "typ": ReceiptType})
	if err != nil {
		return "", err
	}
	payload, err := canonicalJSON(document)
	if err != nil {
		return "", err
	}
	encode := base64.RawURLEncoding.EncodeToString
	compact := encode([]byte(header)) + "." + encode([]byte(payload)) + "." + value
	if len(compact) > maxCompactBytes {
		return "", errors.New("the receipt is too long")
	}
	return compact, nil
}
