package witnesses

import (
	"errors"
	"vsc-node/lib/dids"
)

func (w *Witness) ConsensusKey() (dids.BlsDID, error) {
	for _, key := range w.DidKeys {
		if key.Type == "consensus" {
			return dids.ParseBlsDID(key.Key)
		}
	}

	return "", errors.New("consensus key not found")
}

// VerifyConsensusPoP verifies the proof-of-possession of this witness's
// announced consensus BLS key against the witness account (audit H-6). A valid
// PoP proves the announcer holds the secret behind the announced pubkey,
// defeating rogue-key aggregate-signature forgery. Pure function of the
// on-chain witness record (dids.VerifyBlsPoP has no state/time/RNG), so every
// node reaches the identical verdict — safe to gate election membership on.
// Returns an error when there is no consensus key, the PoP is missing, or the
// PoP fails to verify.
func (w *Witness) VerifyConsensusPoP() error {
	for _, key := range w.DidKeys {
		if key.Type == "consensus" {
			return dids.VerifyBlsPoP(dids.BlsDID(key.Key), w.Account, key.PoP)
		}
	}
	return errors.New("consensus key not found")
}
