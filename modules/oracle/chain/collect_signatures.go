package chain

import (
	"context"
	"time"
	"vsc-node/lib/dids"
	"vsc-node/lib/vsclog"
)

// signatureVerifier validates a single witness signature against an in-progress
// BLS circuit. Production passes circuit.AddAndVerify; tests pass a stub.
// It returns whether the signature was added (true) and any error encountered.
type signatureVerifier func(member dids.BlsDID, signature string) (bool, error)

// memberWeightFn returns the election weight for a given BLS DID.
// Production passes a closure over the election; tests pass a stub.
type memberWeightFn func(did dids.BlsDID) uint64

// signatureCollectionResult captures the outcome of a signature collection
// session. Exactly one of TimedOut/Cancelled may be true; both can be false
// when the threshold is met before either fires.
type signatureCollectionResult struct {
	// SignedWeight is the cumulative weight of accepted signatures, including
	// the producer's own initial weight.
	SignedWeight uint64
	// TimedOut is true if the timeout fired before reaching threshold.
	TimedOut bool
	// Cancelled is true if the context was cancelled (or sigChan closed)
	// before reaching threshold.
	Cancelled bool
}

// collectChainSignatures waits for BLS signatures from peer witnesses on the
// given session channel until any of:
//   - signedWeight strictly exceeds the threshold (success), or
//   - the timeout fires, or
//   - the context is cancelled / sigChan is closed.
//
// The producer's own self-signature is identified by selfDid and skipped
// (it is assumed to already be reflected in initialSignedWeight). Empty
// messages and signatures rejected by addAndVerify are ignored.
//
// This helper exists to keep the consensus loop testable in isolation: it
// has no dependency on real BLS keys, the data layer, or the txpool.
func collectChainSignatures(
	ctx context.Context,
	logger *vsclog.Logger,
	sigChan <-chan signatureMessage,
	timeout time.Duration,
	initialSignedWeight uint64,
	threshold uint64,
	selfDid dids.BlsDID,
	addAndVerify signatureVerifier,
	weightOf memberWeightFn,
	symbol string,
) signatureCollectionResult {
	signedWeight := initialSignedWeight
	// VR2-19: credit each signer AT MOST ONCE per collection round.
	//
	// Nothing upstream deduplicates: receiveSignature pushes every inbound message
	// onto the channel, and the circuit's addRaw always reports success on a valid
	// signature (it overwrites the map entry rather than rejecting a repeat). So a
	// witness that re-broadcast its OWN valid signature had its weight added again
	// on every copy, and the loop below exits on `signedWeight > threshold` — one
	// witness could therefore satisfy the threshold alone.
	//
	// Keying on the BlsDID is sufficient here (unlike a self-declared account
	// string elsewhere): the DID IS the encoded public key, and it is the key
	// addAndVerify verifies against, so a witness cannot be credited under a DID
	// whose private key it does not hold — a forged `member` simply fails
	// verification and never reaches this point.
	credited := make(map[dids.BlsDID]bool)
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	for signedWeight <= threshold {
		select {
		case <-ctx.Done():
			return signatureCollectionResult{
				SignedWeight: signedWeight,
				Cancelled:    true,
			}

		case <-timer.C:
			logger.Debug("signature collection timeout",
				"symbol", symbol,
				"signedWeight", signedWeight,
				"threshold", threshold,
			)
			return signatureCollectionResult{
				SignedWeight: signedWeight,
				TimedOut:     true,
			}

		case sigMsg, ok := <-sigChan:
			if !ok {
				// Channel closed externally — treat like cancellation.
				return signatureCollectionResult{
					SignedWeight: signedWeight,
					Cancelled:    true,
				}
			}
			if sigMsg.BlsDid == "" || sigMsg.Signature == "" {
				continue
			}
			// Skip our own signature (already counted in initialSignedWeight).
			if sigMsg.BlsDid == selfDid.String() {
				continue
			}

			member := dids.BlsDID(sigMsg.BlsDid)
			if credited[member] {
				// A repeat from a signer already counted this round. Verifying it
				// again would succeed (the signature is genuine), so the guard must
				// be here, before the weight is added.
				logger.Debug("ignoring duplicate signature from an already-credited signer",
					"symbol", symbol,
					"account", sigMsg.Account,
					"blsDid", sigMsg.BlsDid,
				)
				continue
			}
			added, err := addAndVerify(member, sigMsg.Signature)
			if err != nil {
				logger.Debug("failed to verify signature",
					"symbol", symbol,
					"account", sigMsg.Account,
					"err", err,
				)
				continue
			}
			if !added {
				logger.Debug("signature not added to circuit",
					"symbol", symbol,
					"account", sigMsg.Account,
					"blsDid", sigMsg.BlsDid,
				)
				continue
			}

			credited[member] = true
			weight := weightOf(member)
			signedWeight += weight
			logger.Debug("received signature",
				"symbol", symbol,
				"account", sigMsg.Account,
				"weight", weight,
				"signedWeight", signedWeight,
				"threshold", threshold,
			)
		}
	}

	return signatureCollectionResult{SignedWeight: signedWeight}
}
