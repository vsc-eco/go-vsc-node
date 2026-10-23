package tss

import (
	"testing"

	"vsc-node/modules/db/vsc/witnesses"

	"github.com/stretchr/testify/assert"
)

// B1: a round message's sender is the session party behind the peer id, not just the
// first account that announced it.
func TestB1_SenderAccountPrefersTheSessionsParty(t *testing.T) {
	d := &ReshareDispatcher{
		BaseDispatcher:  BaseDispatcher{participants: []Participant{{Account: "magi.test1"}, {Account: "magi.test2"}}},
		newParticipants: []Participant{{Account: "tibfox"}},
	}
	candidates := []witnesses.Witness{{Account: "squatter"}, {Account: "tibfox"}}
	assert.Equal(t, "tibfox", senderAccount(candidates, d), "a new-committee party wins over a non-party")
	candidates = []witnesses.Witness{{Account: "squatter"}, {Account: "magi.test2"}}
	assert.Equal(t, "magi.test2", senderAccount(candidates, d), "an old-committee party wins")
	candidates = []witnesses.Witness{{Account: "a"}, {Account: "b"}}
	assert.Equal(t, "a", senderAccount(candidates, d), "no party among them: the lookup's first, same on every node")
	assert.Equal(t, "a", senderAccount(candidates, nil), "no dispatcher (unknown session): the lookup's first")
}
