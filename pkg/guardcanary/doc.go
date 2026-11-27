// Package guardcanary joins three separately observed pieces of evidence for
// one stateless OCI application update on one Linux host (kombify-StackKits-jaax,
// Guard C):
//
//  1. trusted image admission: the exact pinned sha256 digest, a verified
//     signature by an allow-listed signer/issuer over that digest, and a
//     retained, admissible previous digest and configuration that is the one
//     running now;
//  2. bounded canary health: startup, readiness, error-rate and latency probes
//     of the candidate that are fresh, clock-skew checked and exclusive to the
//     candidate digest, failing closed when missing, stale or inconclusive;
//  3. executed rollback: when the canary is not healthy the retained previous
//     digest and configuration are applied again, the previous digest is proven
//     running and healthy, and the rejected digest is proven no longer running.
//
// The result is a hash-chained qualification receipt that names exact versions,
// scope, evidence age, explicit unknowns and the bounds it never claims. The
// package decides nothing about how a signature is verified, how a container
// runtime applies a release or how probes are taken: those are the injected
// [Verifier] and [Runtime] ports, so the receipt only ever states what those
// ports observed. It replaces no updater and implies no database rollback.
package guardcanary
