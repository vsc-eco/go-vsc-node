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
