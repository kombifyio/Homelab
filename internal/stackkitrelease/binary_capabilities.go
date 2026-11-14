package stackkitrelease

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"os"
	"regexp"

	"github.com/kombifyio/techstack/pkg/ril/primitives"
)

// The marker is emitted by the executable's binary-capabilities command. It
// is read from the same bytes whose digest is checked against the exact pin.
const verifyPlanManifest = `stackkit.binary-capabilities/v1:{"capabilities":["stackkit.verify.expected-plan-hash.v1"]}:end-stackkit.binary-capabilities`

// rilPrimitiveManifest declares that each executor-bound StackKits command
// refuses a different --expected-ril-primitive-hash for its own primitive.
const rilPrimitiveManifest = `stackkit.binary-capabilities/v1:{"capabilities":["stackkit.ril-primitive-hash.v2"]}:end-stackkit.binary-capabilities`

// One marker per executor-bound primitive carries its catalog contract hash.
var rilPrimitiveHashPattern = regexp.MustCompile(`stackkit\.ril-primitive-contract/v2:([a-z0-9][a-z0-9-]{0,63}):(sha256:[a-f0-9]{64}):end-stackkit\.ril-primitive-contract`)

const rilPrimitiveHashMarkerMaxLength = len("stackkit.ril-primitive-contract/v2:") + 64 + len(":sha256:") + 64 + len(":end-stackkit.ril-primitive-contract")

type capabilityScanner struct {
	tail       []byte
	matches    int
	rilMatches int
	rilCounts  map[string]int
	rilHashes  map[string]string
}

func (s *capabilityScanner) Write(p []byte) (int, error) {
	n := len(p)
	data := append(s.tail, p...)
	countNew := func(marker []byte) int {
		count := 0
		for start := 0; start < len(data); {
			i := bytes.Index(data[start:], marker)
			if i < 0 {
				break
			}
			i += start
			if i+len(marker) > len(s.tail) {
				count++
			}
			start = i + 1
		}
		return count
	}
	s.matches += countNew([]byte(verifyPlanManifest))
	s.rilMatches += countNew([]byte(rilPrimitiveManifest))
	for _, loc := range rilPrimitiveHashPattern.FindAllSubmatchIndex(data, -1) {
		if loc[1] > len(s.tail) {
			if s.rilCounts == nil {
				s.rilCounts, s.rilHashes = map[string]int{}, map[string]string{}
			}
			id := string(data[loc[2]:loc[3]])
			s.rilCounts[id]++
			s.rilHashes[id] = string(data[loc[4]:loc[5]])
		}
	}
	keep := len(verifyPlanManifest) - 1
	if n := len(rilPrimitiveManifest) - 1; n > keep {
		keep = n
	}
	if n := rilPrimitiveHashMarkerMaxLength - 1; n > keep {
		keep = n
	}
	if len(data) > keep {
		data = data[len(data)-keep:]
	}
	s.tail = append(s.tail[:0], data...)
	return n, nil
}

// qualifiedRILHashes returns id -> contract hash only when the capability is
// declared exactly once and every primitive marker occurs exactly once; any
// ambiguity qualifies none.
func (s *capabilityScanner) qualifiedRILHashes() map[string]string {
	if s.rilMatches != 1 || len(s.rilCounts) == 0 {
		return nil
	}
	out := make(map[string]string, len(s.rilCounts))
	for id, count := range s.rilCounts {
		if count != 1 {
			return nil
		}
		out[id] = s.rilHashes[id]
	}
	return out
}

func (s *capabilityScanner) fileDigest(sum []byte, size int64) fileDigest {
	return fileDigest{sha256: hex.EncodeToString(sum), size: size, verifyExpectedPlan: s.matches == 1, rilPrimitiveHashes: s.qualifiedRILHashes()}
}

// VerifyExpectedPlanSupported is true only when authenticated executable bytes
// from this resolved release carry the source-bound capability declaration.
func (release Release) VerifyExpectedPlanSupported() bool { return release.verifyExpectedPlan }

// RILPrimitiveHash is the contract hash the authenticated executable declares
// for one executor-bound primitive, or "" unless it is qualified.
func (release Release) RILPrimitiveHash(id string) string {
	if _, ok := primitives.Lookup(id); !ok {
		return ""
	}
	return release.rilPrimitiveHashes[id]
}

// RILPrimitiveSupported reports whether the authenticated executable carries
// a qualified contract hash for exactly this executor-bound primitive.
func (release Release) RILPrimitiveSupported(id string) bool {
	return release.RILPrimitiveHash(id) != ""
}

// verifyCapabilitiesBinary accepts only bytes matching the authenticated Guard
// release identity; absent image data never grants the capability.
func verifyCapabilitiesBinary(path, signedDigest string) (bool, map[string]string, error) {
	binary, err := digestRegularFile(path, maxExecutableBytes)
	if os.IsNotExist(err) {
		return false, nil, nil
	}
	if err != nil {
		return false, nil, fmt.Errorf("verify capability binary: %w", err)
	}
	if binary.sha256 != signedDigest {
		return false, nil, fmt.Errorf("capability binary differs from signed managed runtime")
	}
	return binary.verifyExpectedPlan, binary.rilPrimitiveHashes, nil
}

func verifyCapabilityBinary(path, signedDigest string) (bool, error) {
	plan, _, err := verifyCapabilitiesBinary(path, signedDigest)
	return plan, err
}
