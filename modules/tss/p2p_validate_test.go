package tss

import (
	"context"
	"testing"

	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/stretchr/testify/assert"
)

func TestValidateMessage_FiltersStaleMessages(t *testing.T) {
	tssMgr := &TssManager{
		sessionMap: make(map[string]sessionInfo),
		actionMap:  make(map[string]Dispatcher),
	}

	spec := p2pSpec{tssMgr: tssMgr}

	// Register active session
	sessionId := "keygen-1000-0-key1"
	tssMgr.sessionMap[sessionId] = sessionInfo{
		leader: "leader",
		bh:     1000,
		action: ActionTypeKeyGen,
	}

	// Valid message (current round)
	validMsg := p2pMessage{
		Type:    "msg",
		Session: sessionId,
		Round:   1000,
		Action:  "keygen",
	}

	ctx := context.Background()
	fromPeer := peer.ID("test-peer")
	pubsubMsg := &pubsub.Message{}

	result := spec.ValidateMessage(ctx, fromPeer, pubsubMsg, validMsg)
	assert.True(t, result, "Valid message should be accepted")

	// Stale message (old round - more than 1 block behind)
	staleMsg := p2pMessage{
		Type:    "msg",
		Session: sessionId,
		Round:   998, // More than 1 block behind
		Action:  "keygen",
	}

	result = spec.ValidateMessage(ctx, fromPeer, pubsubMsg, staleMsg)
	assert.False(t, result, "Stale message should be rejected")

	// Unknown session
	unknownMsg := p2pMessage{
		Type:    "msg",
		Session: "unknown-session",
		Round:   1000,
		Action:  "keygen",
	}

	result = spec.ValidateMessage(ctx, fromPeer, pubsubMsg, unknownMsg)
	assert.False(t, result, "Unknown session message should be rejected")
}

func TestValidateMessage_ActionTypeMismatch(t *testing.T) {
	tssMgr := &TssManager{
		sessionMap: make(map[string]sessionInfo),
		actionMap:  make(map[string]Dispatcher),
	}

	spec := p2pSpec{tssMgr: tssMgr}

	sessionId := "sign-1000-0-key1"
	tssMgr.sessionMap[sessionId] = sessionInfo{
		leader: "leader",
		bh:     1000,
		action: ActionTypeSign,
	}

	// Message with wrong action type
	wrongActionMsg := p2pMessage{
		Type:    "msg",
		Session: sessionId,
		Round:   1000,
		Action:  "keygen", // Wrong - should be "sign"
	}

	ctx := context.Background()
	fromPeer := peer.ID("test-peer")
	pubsubMsg := &pubsub.Message{}

	result := spec.ValidateMessage(ctx, fromPeer, pubsubMsg, wrongActionMsg)
	assert.False(t, result, "Message with wrong action type should be rejected")
}

func TestValidateMessage_AllowsSignatureMessages(t *testing.T) {
	tssMgr := &TssManager{
		sessionMap: make(map[string]sessionInfo),
		actionMap:  make(map[string]Dispatcher),
	}

	spec := p2pSpec{tssMgr: tssMgr}

	// Signature-related messages should always be accepted
	askSigsMsg := p2pMessage{
		Type: "ask_sigs",
	}

	resSigMsg := p2pMessage{
		Type: "res_sig",
	}

	ctx := context.Background()
	fromPeer := peer.ID("test-peer")
	pubsubMsg := &pubsub.Message{}

	assert.True(t, spec.ValidateMessage(ctx, fromPeer, pubsubMsg, askSigsMsg))
	assert.True(t, spec.ValidateMessage(ctx, fromPeer, pubsubMsg, resSigMsg))
}

func TestValidateMessage_RoundWindow(t *testing.T) {
	tssMgr := &TssManager{
		sessionMap: make(map[string]sessionInfo),
		actionMap:  make(map[string]Dispatcher),
	}

	spec := p2pSpec{tssMgr: tssMgr}

	sessionId := "keygen-1000-0-key1"
	tssMgr.sessionMap[sessionId] = sessionInfo{
		leader: "leader",
		bh:     1000,
		action: ActionTypeKeyGen,
	}

	ctx := context.Background()
	fromPeer := peer.ID("test-peer")
	pubsubMsg := &pubsub.Message{}

	// Test message at round +1 (should be accepted)
	msgRoundPlus1 := p2pMessage{
		Type:    "msg",
		Session: sessionId,
		Round:   1001,
		Action:  "keygen",
	}
	result := spec.ValidateMessage(ctx, fromPeer, pubsubMsg, msgRoundPlus1)
	assert.True(t, result, "Message at round+1 should be accepted")

	// Test message at round -1 (should be accepted)
	msgRoundMinus1 := p2pMessage{
		Type:    "msg",
		Session: sessionId,
		Round:   999,
		Action:  "keygen",
	}
	result = spec.ValidateMessage(ctx, fromPeer, pubsubMsg, msgRoundMinus1)
	assert.True(t, result, "Message at round-1 should be accepted")

	// Test message at round -2 (should be rejected)
	msgRoundMinus2 := p2pMessage{
		Type:    "msg",
		Session: sessionId,
		Round:   998,
		Action:  "keygen",
	}
	result = spec.ValidateMessage(ctx, fromPeer, pubsubMsg, msgRoundMinus2)
	assert.False(t, result, "Message at round-2 should be rejected")
}
