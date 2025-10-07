package chain

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

var (
	errInvalidChainSymbol = errors.New("invalid chain symbol")
)

func makeChainSession(chain chainRelay, chainState *chainState) string {
	return fmt.Sprintf("%s-%d", chain.Symbol(), chainState.blockHeight)

}

func parseChainSession(sessionID string) (string, *chainState, error) {
	buf := strings.Split(sessionID, "-")
	if len(buf) != 2 {
		return "", nil, errInvalidSessionID
	}

	// parse chain state
	var (
		chainSymbol    = buf[0]
		blockHeightStr = buf[1]
	)

	bh, err := strconv.ParseUint(blockHeightStr, 10, 64)
	if err != nil {
		return "", nil, err
	}

	chainState := &chainState{
		blockHeight: bh,
	}

	return chainSymbol, chainState, nil
}
