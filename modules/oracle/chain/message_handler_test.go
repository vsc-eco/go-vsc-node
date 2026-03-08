package chain

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReceiveSignature_RoutesToChannel(t *testing.T) {
	sc := makeSignatureChannels()
	c := &ChainOracle{
		signatureChannels: sc,
	}

	ch, err := sc.makeSession("BTC-100-200")
	require.NoError(t, err)

	response := chainRelayResponse{
		Signature: "test-sig",
		Account:   "witness1",
		BlsDid:    "did:key:z6witness1",
	}
	payloadBytes, _ := json.Marshal(response)

	msg := &chainOracleMessage{
		MessageType: signatureResponse,
		SessionID:   "BTC-100-200",
		Payload:     payloadBytes,
	}

	err = receiveSignature(c, msg)
	require.NoError(t, err)

	// Verify it arrived on the channel
	received := <-ch
	assert.Equal(t, "test-sig", received.Signature)
	assert.Equal(t, "witness1", received.Account)
	assert.Equal(t, "did:key:z6witness1", received.BlsDid)
}

func TestReceiveSignature_NoSession(t *testing.T) {
	sc := makeSignatureChannels()
	c := &ChainOracle{
		signatureChannels: sc,
	}

	response := chainRelayResponse{
		Signature: "test-sig",
		Account:   "witness1",
		BlsDid:    "did:key:z6test",
	}
	payloadBytes, _ := json.Marshal(response)

	msg := &chainOracleMessage{
		MessageType: signatureResponse,
		SessionID:   "nonexistent",
		Payload:     payloadBytes,
	}

	err := receiveSignature(c, msg)
	assert.ErrorIs(t, err, errInvalidSession)
}

func TestReceiveSignature_InvalidPayload(t *testing.T) {
	sc := makeSignatureChannels()
	c := &ChainOracle{
		signatureChannels: sc,
	}

	msg := &chainOracleMessage{
		MessageType: signatureResponse,
		SessionID:   "BTC-100-200",
		Payload:     json.RawMessage(`{invalid json`),
	}

	err := receiveSignature(c, msg)
	assert.Error(t, err)
}
