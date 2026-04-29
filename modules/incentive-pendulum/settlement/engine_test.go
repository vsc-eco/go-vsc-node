package settlement

import (
	"errors"
	"testing"
	"vsc-node/modules/db/vsc/elections"
)

type mockElectionReader struct {
	at map[uint64]elections.ElectionResult
}

func (m *mockElectionReader) GetElectionByHeight(height uint64) (elections.ElectionResult, error) {
	res, ok := m.at[height]
	if !ok {
		return elections.ElectionResult{}, errors.New("missing")
	}
	return res, nil
}

func TestProcessBlockEpochBoundary(t *testing.T) {
	called := false
	engine := New(
		&mockElectionReader{
			at: map[uint64]elections.ElectionResult{
				0:  {ElectionCommonInfo: elections.ElectionCommonInfo{Epoch: 10}},
				10: {ElectionCommonInfo: elections.ElectionCommonInfo{Epoch: 11}},
			},
		},
		"hive:witness-2",
		func(slotHeight uint64) []ScheduledLeader {
			return []ScheduledLeader{
				{Account: "hive:witness-1", SlotHeight: 0},
				{Account: "hive:witness-2", SlotHeight: 10},
			}
		},
		func(info BoundaryInfo) error {
			called = true
			if info.PreviousEpoch != 10 || info.CurrentEpoch != 11 {
				t.Fatalf("unexpected epoch transition: %+v", info)
			}
			return nil
		},
	)

	if err := engine.ProcessBlock(0); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("should not call handler on first observed epoch")
	}

	if err := engine.ProcessBlock(10); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("expected boundary callback to be called")
	}
}

func TestProcessBlockEpochBoundaryWithoutSelfFilter(t *testing.T) {
	called := false
	engine := New(
		&mockElectionReader{
			at: map[uint64]elections.ElectionResult{
				0:  {ElectionCommonInfo: elections.ElectionCommonInfo{Epoch: 1}},
				10: {ElectionCommonInfo: elections.ElectionCommonInfo{Epoch: 2}},
			},
		},
		"",
		func(slotHeight uint64) []ScheduledLeader {
			return []ScheduledLeader{
				{Account: "hive:w1", SlotHeight: 0},
				{Account: "hive:w2", SlotHeight: 10},
			}
		},
		func(info BoundaryInfo) error {
			called = true
			return nil
		},
	)
	_ = engine.ProcessBlock(0)
	_ = engine.ProcessBlock(10)
	if !called {
		t.Fatal("expected callback when selfID filter disabled")
	}
}

func TestProcessBlockEpochBoundarySkipsWhenNotLeader(t *testing.T) {
	called := false
	engine := New(
		&mockElectionReader{
			at: map[uint64]elections.ElectionResult{
				0:  {ElectionCommonInfo: elections.ElectionCommonInfo{Epoch: 1}},
				10: {ElectionCommonInfo: elections.ElectionCommonInfo{Epoch: 2}},
			},
		},
		"hive:not-leader",
		func(slotHeight uint64) []ScheduledLeader {
			return []ScheduledLeader{
				{Account: "hive:w1", SlotHeight: 0},
				{Account: "hive:w2", SlotHeight: 10},
			}
		},
		func(info BoundaryInfo) error {
			called = true
			return nil
		},
	)
	_ = engine.ProcessBlock(0)
	_ = engine.ProcessBlock(10)
	if called {
		t.Fatal("callback should not run for non-leader self")
	}
}
