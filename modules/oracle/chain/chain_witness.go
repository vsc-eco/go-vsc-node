package chain

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"strings"
)

const rawBlsSignatureLength = 96

// signs off chain data and returns a signature
func witnessChainData(c *ChainOracle, msg *chainOracleMessage) (string, error) {
	chainSymbol, startBlock, endBlock, err := parseChainSessionID(msg.SessionID)
	if err != nil {
		return "", fmt.Errorf("invalid session id: %w", err)
	}

	fmt.Println(
		"TODO: fetch %s block from %d to %d",
		chainSymbol, startBlock, endBlock,
	)

	chain, ok := c.chainRelayers[strings.ToUpper(chainSymbol)]
	if !ok {
		return "", errInvalidChainSymbol
	}
	count := (endBlock - startBlock) + 1

	blocks, err := chain.ChainData(startBlock, count)
	fmt.Println("verify blocks", len(blocks), err)

	/*
		if err := chain.VerifyChainData(msg.Payload); err != nil {
			return "", fmt.Errorf("invalid chain data: %w", err)
		}

			signature, err := signChainData(msg.Payload)
			if err != nil {
				return "", fmt.Errorf("failed to sign chain data: %w", err)
			}

			return signature, nil
	*/
	return "", nil
}

// TODO: sign chain data, 96 bytes signature with base64.RawStdEncoding
func signChainData(payload []byte) (string, error) {
	var buf [rawBlsSignatureLength]byte
	n := copy(buf[:], payload)
	if _, err := io.ReadFull(rand.Reader, buf[n:]); err != nil {
		return "", err
	}

	signature := base64.RawStdEncoding.EncodeToString(buf[:])
	return signature, nil
}
