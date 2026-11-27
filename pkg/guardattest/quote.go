package guardattest

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/binary"
	"errors"
)

// Quote verification errors. The gate maps them to stable denial codes.
var (
	ErrQuoteMalformed = errors.New("guardattest: TPM quote is malformed")
	ErrQuoteSignature = errors.New("guardattest: TPM quote signature is invalid")
	ErrQuoteKey       = errors.New("guardattest: attestation key type is not supported")
)

const (
	tpmGeneratedValue = 0xff544347
	tpmSTAttestQuote  = 0x8018
	tpmAlgSHA256      = 0x000b

	maxQuoteBytes     = 2048
	maxSelections     = 16
	maxSelectBytes    = 4
	minRSAKeyBits     = 2048
	maxRSAKeyBits     = 8192
	pcrDigestSHA256   = sha256.Size
	maxQualifiedName  = 66
	maxQualifyingData = 64
)

// PCRSelection is one TPML_PCR_SELECTION entry of a quote.
type PCRSelection struct {
	Bank string
	PCRs []int
}

// ClockInfo is the TPMS_CLOCK_INFO of a quote. Safe is false when the TPM
// clock may have been set back since the last reset.
type ClockInfo struct {
	Clock        uint64 `json:"clock"`
	ResetCount   uint32 `json:"reset_count"`
	RestartCount uint32 `json:"restart_count"`
	Safe         bool   `json:"safe"`
}

// QuoteClaims is the signed content of a verified TPM2_Quote.
type QuoteClaims struct {
	QualifiedSigner []byte
	ExtraData       []byte
	Clock           ClockInfo
	FirmwareVersion uint64
	Selections      []PCRSelection
	PCRDigest       []byte
}

// QuoteVerifier checks a quote's signature against the enrolled attestation
// key and returns the signed claims. Implementations must return an error for
// any quote whose signature does not verify; the gate treats every error as a
// denial.
type QuoteVerifier interface {
	VerifyQuote(key any, quote, signature []byte) (QuoteClaims, error)
}

// TPMQuoteVerifier verifies a TPM 2.0 TPMS_ATTEST quote (TPM_ST_ATTEST_QUOTE)
// signed with SHA-256 by an ECDSA P-256 key or an RSA key of 2048 to 8192 bits
// (public exponent at least 65537). An ECDSA signature is the ASN.1 DER
// SEQUENCE of r and s, not the raw r||s concatenation; an RSA signature is the
// PKCS#1 v1.5 value. The collector converts the TPMT_SIGNATURE into that form.
type TPMQuoteVerifier struct{}

// VerifyQuote verifies the signature first and parses the signed bytes only
// afterwards, so unauthenticated input is never interpreted beyond its length.
func (TPMQuoteVerifier) VerifyQuote(key any, quote, signature []byte) (QuoteClaims, error) {
	if len(quote) == 0 || len(quote) > maxQuoteBytes || len(signature) == 0 || len(signature) > 1024 {
		return QuoteClaims{}, ErrQuoteMalformed
	}
	digest := sha256.Sum256(quote)
	switch k := key.(type) {
	case *ecdsa.PublicKey:
		if k == nil || k.Curve != elliptic.P256() || k.X == nil || k.Y == nil {
			return QuoteClaims{}, ErrQuoteKey
		}
		// ECDH conversion rejects a point that is not on the curve.
		if _, err := k.ECDH(); err != nil {
			return QuoteClaims{}, ErrQuoteKey
		}
		if !ecdsa.VerifyASN1(k, digest[:], signature) {
			return QuoteClaims{}, ErrQuoteSignature
		}
	case *rsa.PublicKey:
		if k == nil || k.N == nil || k.N.Sign() <= 0 || k.N.Bit(0) != 1 ||
			k.N.BitLen() < minRSAKeyBits || k.N.BitLen() > maxRSAKeyBits ||
			k.E < 65537 || k.E%2 == 0 || k.E > 1<<31-1 {
			return QuoteClaims{}, ErrQuoteKey
		}
		if rsa.VerifyPKCS1v15(k, crypto.SHA256, digest[:], signature) != nil {
			return QuoteClaims{}, ErrQuoteSignature
		}
	default:
		return QuoteClaims{}, ErrQuoteKey
	}
	return parseQuote(quote)
}

type quoteReader struct {
	b   []byte
	bad bool
}

func (r *quoteReader) take(n int) []byte {
	if r.bad || n < 0 || n > len(r.b) {
		r.bad = true
		return nil
	}
	out := r.b[:n]
	r.b = r.b[n:]
	return out
}

func (r *quoteReader) u8() uint8 {
	if v := r.take(1); v != nil {
		return v[0]
	}
	return 0
}

func (r *quoteReader) u16() uint16 {
	if v := r.take(2); v != nil {
		return binary.BigEndian.Uint16(v)
	}
	return 0
}

func (r *quoteReader) u32() uint32 {
	if v := r.take(4); v != nil {
		return binary.BigEndian.Uint32(v)
	}
	return 0
}

func (r *quoteReader) u64() uint64 {
	if v := r.take(8); v != nil {
		return binary.BigEndian.Uint64(v)
	}
	return 0
}

func (r *quoteReader) sized(limit int) []byte {
	n := int(r.u16())
	if n > limit {
		r.bad = true
		return nil
	}
	v := r.take(n)
	return append([]byte(nil), v...)
}

// parseQuote decodes an authenticated TPMS_ATTEST of type quote.
func parseQuote(raw []byte) (QuoteClaims, error) {
	r := &quoteReader{b: raw}
	if r.u32() != tpmGeneratedValue || r.u16() != tpmSTAttestQuote {
		return QuoteClaims{}, ErrQuoteMalformed
	}
	var claims QuoteClaims
	claims.QualifiedSigner = r.sized(maxQualifiedName)
	// A TPM2B_NAME is a 2-byte algorithm id plus a digest, never empty.
	if len(claims.QualifiedSigner) < 2 {
		return QuoteClaims{}, ErrQuoteMalformed
	}
	claims.ExtraData = r.sized(maxQualifyingData)
	claims.Clock.Clock = r.u64()
	claims.Clock.ResetCount = r.u32()
	claims.Clock.RestartCount = r.u32()
	switch r.u8() {
	case 0:
	case 1:
		claims.Clock.Safe = true
	default:
		return QuoteClaims{}, ErrQuoteMalformed
	}
	claims.FirmwareVersion = r.u64()
	count := r.u32()
	if r.bad || count == 0 || count > maxSelections {
		return QuoteClaims{}, ErrQuoteMalformed
	}
	for i := uint32(0); i < count; i++ {
		alg := r.u16()
		size := int(r.u8())
		if size == 0 || size > maxSelectBytes {
			return QuoteClaims{}, ErrQuoteMalformed
		}
		mask := r.take(size)
		if r.bad {
			return QuoteClaims{}, ErrQuoteMalformed
		}
		sel := PCRSelection{Bank: "alg-" + hex16(alg)}
		if alg == tpmAlgSHA256 {
			sel.Bank = BankSHA256
		}
		for byteIdx, b := range mask {
			for bit := 0; bit < 8; bit++ {
				if b&(1<<bit) != 0 {
					sel.PCRs = append(sel.PCRs, byteIdx*8+bit)
				}
			}
		}
		claims.Selections = append(claims.Selections, sel)
	}
	claims.PCRDigest = r.sized(pcrDigestSHA256)
	if r.bad || len(r.b) != 0 || len(claims.PCRDigest) != pcrDigestSHA256 {
		return QuoteClaims{}, ErrQuoteMalformed
	}
	return claims, nil
}

func hex16(v uint16) string {
	const digits = "0123456789abcdef"
	return string([]byte{digits[v>>12&0xf], digits[v>>8&0xf], digits[v>>4&0xf], digits[v&0xf]})
}
