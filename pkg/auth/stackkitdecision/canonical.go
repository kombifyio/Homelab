// Package stackkitdecision is the producer side of the platform's signed
// decision receipt for a destructive StackKits operation (platform-8f0n2.20).
//
// Homelab is the decision ledger's only filer. It files the exact binding a
// StackKits node recomputes before the operation's first effect, reads the
// signed receipt back, checks what it can before anything is dispatched, and
// rebuilds the compact JWS the node verifies and spends. The node stays the
// authority that verifies and spends the receipt.
//
// The binding is defined once, in the StackKits consumer's
// docs/DECISION-RECEIPTS.md section "Binding definition". Homelab cannot
// import that package (another module, an internal path), so this package
// builds the same bytes itself; its tests pin them to the consumer's vectors.
package stackkitdecision

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// maxSafeInteger is the largest integer JSON.stringify prints without a
// fractional part. The ledger's canonical form accepts safe integers only.
const maxSafeInteger = 9007199254740991

// canonicalJSON renders a value in the ledger's canonicalDecisionJson form:
// object members sorted by UTF-16 code unit, strings escaped exactly as
// JSON.stringify escapes them, safe integers only, no whitespace. It is the
// same writer as the consumer's internal/crypto/decisionreceipt/canonical.go.
func canonicalJSON(value any) (string, error) {
	var out strings.Builder
	if err := writeCanonicalValue(&out, value); err != nil {
		return "", err
	}
	return out.String(), nil
}

func writeCanonicalValue(out *strings.Builder, value any) error {
	switch typed := value.(type) {
	case nil:
		out.WriteString("null")
	case bool:
		if typed {
			out.WriteString("true")
		} else {
			out.WriteString("false")
		}
	case string:
		return writeCanonicalString(out, typed)
	case int:
		out.WriteString(strconv.FormatInt(int64(typed), 10))
	case int64:
		out.WriteString(strconv.FormatInt(typed, 10))
	case json.Number:
		// A receipt read back from the ledger is decoded with UseNumber, so its
		// integers keep their exact value.
		parsed, err := strconv.ParseInt(typed.String(), 10, 64)
		if err != nil || parsed > maxSafeInteger || parsed < -maxSafeInteger {
			return fmt.Errorf("canonical JSON accepts safe integers only")
		}
		out.WriteString(strconv.FormatInt(parsed, 10))
	case float64:
		if math.IsNaN(typed) || math.IsInf(typed, 0) || typed != math.Trunc(typed) || math.Abs(typed) > maxSafeInteger {
			return fmt.Errorf("canonical JSON accepts safe integers only")
		}
		out.WriteString(strconv.FormatInt(int64(typed), 10))
	case []any:
		out.WriteByte('[')
		for index, item := range typed {
			if index > 0 {
				out.WriteByte(',')
			}
			if err := writeCanonicalValue(out, item); err != nil {
				return err
			}
		}
		out.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Slice(keys, func(left, right int) bool { return lessUTF16(keys[left], keys[right]) })
		out.WriteByte('{')
		for index, key := range keys {
			if index > 0 {
				out.WriteByte(',')
			}
			if err := writeCanonicalString(out, key); err != nil {
				return err
			}
			out.WriteByte(':')
			if err := writeCanonicalValue(out, typed[key]); err != nil {
				return err
			}
		}
		out.WriteByte('}')
	default:
		return fmt.Errorf("canonical JSON rejects %T", value)
	}
	return nil
}

// writeCanonicalString escapes a string exactly as JSON.stringify does: the
// short escapes for the control characters JSON.stringify names, \u00xx for
// every other control character, and every other code unit verbatim. Go's
// encoding/json differs (it writes \u0008 and \u000c, and it escapes U+2028
// and U+2029), so it cannot produce the ledger's bytes.
func writeCanonicalString(out *strings.Builder, value string) error {
	if !utf8.ValidString(value) {
		return fmt.Errorf("canonical JSON rejects malformed UTF-8")
	}
	out.WriteByte('"')
	for _, character := range value {
		switch character {
		case '"':
			out.WriteString(`\"`)
		case '\\':
			out.WriteString(`\\`)
		case '\b':
			out.WriteString(`\b`)
		case '\f':
			out.WriteString(`\f`)
		case '\n':
			out.WriteString(`\n`)
		case '\r':
			out.WriteString(`\r`)
		case '\t':
			out.WriteString(`\t`)
		default:
			if character < 0x20 {
				out.WriteString(fmt.Sprintf(`\u%04x`, character))
				continue
			}
			out.WriteRune(character)
		}
	}
	out.WriteByte('"')
	return nil
}

// lessUTF16 compares two strings by UTF-16 code unit, the order the ledger's
// Object.keys().sort() uses. It differs from Go's byte order for code points
// above U+FFFF, so it is spelled out instead of using sort.Strings.
func lessUTF16(left, right string) bool {
	if left == right {
		return false
	}
	leftUnits := utf16.Encode([]rune(left))
	rightUnits := utf16.Encode([]rune(right))
	for index := 0; index < len(leftUnits) && index < len(rightUnits); index++ {
		if leftUnits[index] != rightUnits[index] {
			return leftUnits[index] < rightUnits[index]
		}
	}
	return len(leftUnits) < len(rightUnits)
}
