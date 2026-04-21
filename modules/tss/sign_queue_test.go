package tss

import (
	"testing"

	test_utils "vsc-node/lib/test_utils"
	tss_db "vsc-node/modules/db/vsc/tss"

	"github.com/stretchr/testify/assert"
)

// TestReserveBatch_AdvancesLastAttemptForSelectedOnly confirms the slot-0
// reservation only touches requests returned by the queue query, and that the
// reservation amount matches SignAttemptReservation from the DB layer.
func TestReserveBatch_AdvancesLastAttemptForSelectedOnly(t *testing.T) {
	mock := &test_utils.MockTssRequestsDb{Requests: map[string]tss_db.TssRequest{}}
	for i, msg := range []string{"a", "b", "c", "d"} {
		_ = mock.SetSignedRequest(tss_db.TssRequest{
			Id: msg, KeyId: "k1", Msg: msg,
			Status: tss_db.SignPending, CreatedHeight: uint64(100 + i),
		})
	}

	// Simulate selection of the first 2 by LastAttempt order.
	selected, err := mock.FindUnsignedRequests(1000, 2)
	assert.NoError(t, err)
	assert.Len(t, selected, 2)

	const bh = uint64(500)
	for _, req := range selected {
		assert.NoError(t, mock.ReserveAttempt(req.KeyId, req.Msg, bh))
	}

	for _, req := range selected {
		doc := mock.Requests[req.Id]
		assert.Equal(t, bh+tss_db.SignAttemptReservation, doc.LastAttempt)
		assert.Equal(t, uint(1), doc.AttemptCount)
	}
	// Untouched rows retain their original LastAttempt.
	for _, msg := range []string{"c", "d"} {
		doc := mock.Requests[msg]
		assert.Equal(t, doc.CreatedHeight, doc.LastAttempt)
		assert.Equal(t, uint(0), doc.AttemptCount)
	}
}

// TestReserveBatch_ReplayIsNoop simulates a block replay after restart: the
// same FindUnsignedRequests + ReserveAttempt sequence runs twice and must
// yield identical state to a single pass.
func TestReserveBatch_ReplayIsNoop(t *testing.T) {
	mock := &test_utils.MockTssRequestsDb{Requests: map[string]tss_db.TssRequest{}}
	_ = mock.SetSignedRequest(tss_db.TssRequest{
		Id: "r", KeyId: "k1", Msg: "r",
		Status: tss_db.SignPending, CreatedHeight: 100,
	})

	const bh = uint64(500)

	run := func() {
		selected, err := mock.FindUnsignedRequests(bh, 10)
		assert.NoError(t, err)
		for _, req := range selected {
			assert.NoError(t, mock.ReserveAttempt(req.KeyId, req.Msg, bh))
		}
	}

	run()
	afterFirst := mock.Requests["r"]
	run()
	afterSecond := mock.Requests["r"]

	assert.Equal(t, afterFirst.LastAttempt, afterSecond.LastAttempt)
	assert.Equal(t, afterFirst.AttemptCount, afterSecond.AttemptCount)
}
