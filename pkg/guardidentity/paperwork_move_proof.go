package guardidentity

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
)

func SignPaperworkMoveProof(key ed25519.PrivateKey, in HostProofInput, grant string) (string, error) {
	if in.Audience != "" || len(grant) != 43 {
		return "", errors.New("Paperwork proof requires an opaque move grant and no chosen audience")
	}
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(grant)
	if err != nil || len(decoded) != 32 {
		return "", errors.New("invalid Paperwork move grant")
	}
	clear(decoded)
	digest := sha256.Sum256([]byte(grant))
	in.Audience = "urn:kombify:paperwork-selfhost-move:sha256:" + hex.EncodeToString(digest[:])
	return signHostProof(key, in)
}

// SignPaperworkPullProof signs one exact data-request digest. The trusted node
// worker computes the digest from origin/method/target/session/catalog/range;
// this is never exposed as a public arbitrary-audience signing endpoint.
func SignPaperworkPullProof(key ed25519.PrivateKey, in HostProofInput, requestSHA256 string) (string, error) {
	if in.Audience != "" || len(requestSHA256) != 64 {
		return "", errors.New("Paperwork pull requires an exact request digest")
	}
	raw, err := hex.DecodeString(requestSHA256)
	if err != nil || len(raw) != sha256.Size || hex.EncodeToString(raw) != requestSHA256 {
		return "", errors.New("invalid Paperwork pull request digest")
	}
	in.Audience = "urn:kombify:paperwork-selfhost-pull:sha256:" + requestSHA256
	return signHostProof(key, in)
}

func PaperworkSignInAudience(requestSHA256 string) string {
	return "urn:kombify:paperwork-linked-sign-in:sha256:" + requestSHA256
}

func SignPaperworkSignInProof(key ed25519.PrivateKey, in HostProofInput, body []byte) (string, error) {
	if in.Audience != "" || len(body) == 0 || len(body) > 8<<10 {
		return "", errors.New("Paperwork sign-in requires a bounded command body")
	}
	digest := sha256.Sum256(body)
	in.Audience = PaperworkSignInAudience(hex.EncodeToString(digest[:]))
	return signHostProof(key, in)
}
