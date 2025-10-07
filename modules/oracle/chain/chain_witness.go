package chain

import (
	"encoding/hex"
	"fmt"
	"strings"
)

// signs off chain data and returns a signature
func witnessChainData(c *ChainOracle, msg *chainOracleMessage) (string, error) {
	chainSymbol, _, err := parseChainSessionID(msg.SessionID)
	if err != nil {
		return "", fmt.Errorf("invalid session id: %w", err)
	}

	chain, ok := c.chainRelayers[strings.ToUpper(chainSymbol)]
	if !ok {
		return "", errInvalidChainSymbol
	}

	if err := chain.VerifyChainData(msg.Payload); err != nil {
		return "", fmt.Errorf("invalid chain data: %w", err)
	}

	// TODO: sign chain data
	signature := hex.EncodeToString(msg.Payload)

	return signature, nil
}
