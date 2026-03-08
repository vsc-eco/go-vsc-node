package chain

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMakeChainOracleMessage_Request(t *testing.T) {
	request := chainRelayRequest{
		ContractId: "vsc1abc123",
		NetId:      "vsc-mainnet",
	}

	msg, err := makeChainOracleMessage(signatureRequest, "BTC-100-200", request)
	require.NoError(t, err)
	assert.Equal(t, signatureRequest, msg.MessageType)
	assert.Equal(t, "BTC-100-200", msg.SessionID)

	// Verify payload can be deserialized back
	var decoded chainRelayRequest
	err = json.Unmarshal(msg.Payload, &decoded)
	require.NoError(t, err)
	assert.Equal(t, "vsc1abc123", decoded.ContractId)
	assert.Equal(t, "vsc-mainnet", decoded.NetId)
}

func TestMakeChainOracleMessage_Response(t *testing.T) {
	response := chainRelayResponse{
		Signature: "base64sig==",
		Account:   "witness1",
		BlsDid:    "did:key:z6test",
	}

	msg, err := makeChainOracleMessage(signatureResponse, "BTC-100-200", response)
	require.NoError(t, err)
	assert.Equal(t, signatureResponse, msg.MessageType)

	var decoded chainRelayResponse
	err = json.Unmarshal(msg.Payload, &decoded)
	require.NoError(t, err)
	assert.Equal(t, "base64sig==", decoded.Signature)
	assert.Equal(t, "witness1", decoded.Account)
	assert.Equal(t, "did:key:z6test", decoded.BlsDid)
}

func TestMakeChainOracleMessage_EmptySessionID(t *testing.T) {
	_, err := makeChainOracleMessage(signatureRequest, "", chainRelayRequest{})
	assert.ErrorIs(t, err, errInvalidSessionID)
}

func TestChainOracleMessage_JSONRoundTrip(t *testing.T) {
	request := chainRelayRequest{
		ContractId: "vsc1contract",
		NetId:      "vsc-mainnet",
	}

	original, err := makeChainOracleMessage(signatureRequest, "DASH-500-600", request)
	require.NoError(t, err)

	// Marshal then unmarshal the full message
	jsonBytes, err := json.Marshal(original)
	require.NoError(t, err)

	var decoded chainOracleMessage
	err = json.Unmarshal(jsonBytes, &decoded)
	require.NoError(t, err)

	assert.Equal(t, original.MessageType, decoded.MessageType)
	assert.Equal(t, original.SessionID, decoded.SessionID)

	var decodedPayload chainRelayRequest
	err = json.Unmarshal(decoded.Payload, &decodedPayload)
	require.NoError(t, err)
	assert.Equal(t, request.ContractId, decodedPayload.ContractId)
	assert.Equal(t, request.NetId, decodedPayload.NetId)
}
