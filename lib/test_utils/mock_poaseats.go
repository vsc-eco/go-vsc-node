package test_utils

import (
	"errors"
	"fmt"
	"sort"

	"vsc-node/modules/aggregate"
	"vsc-node/modules/db/vsc/poaseats"
)

// MockPoaSeatsDb is an in-memory seat registry for state-engine and
// election-proposer tests.
//
// It deliberately mirrors the real implementation's SEMANTICS, not just its
// signatures, because the properties under test are semantic ones: the
// height-addressed read, the append-only rule, the uniqueness rules, and the
// idempotent exit. A mock that accepted a duplicate seat or moved an exit
// height forward would let a test pass against behaviour the real registry
// refuses.
type MockPoaSeatsDb struct {
	aggregate.Plugin
	Seats map[string]poaseats.Seat

	// FailReads makes every read return an error, so callers can prove they
	// fail-stop on a transient read rather than treating it as "no seat" (which
	// would open the gate and, in the election path, empty the committee).
	FailReads bool
}

func NewMockPoaSeatsDb() *MockPoaSeatsDb {
	return &MockPoaSeatsDb{Seats: map[string]poaseats.Seat{}}
}

func (m *MockPoaSeatsDb) GetSeatsAtHeight(height uint64) ([]poaseats.Seat, error) {
	if m.FailReads {
		return nil, errors.New("mock: seat read failure")
	}
	out := make([]poaseats.Seat, 0, len(m.Seats))
	for _, s := range m.Seats {
		if s.AdmittedHeight <= height {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Account < out[j].Account })
	return out, nil
}

func (m *MockPoaSeatsDb) GetSeat(account string) (poaseats.Seat, bool, error) {
	if m.FailReads {
		return poaseats.Seat{}, false, errors.New("mock: seat read failure")
	}
	s, ok := m.Seats[poaseats.NormalizeAccount(account)]
	return s, ok, nil
}

func (m *MockPoaSeatsDb) GetSeatByUbo(uboId string) (poaseats.Seat, bool, error) {
	if m.FailReads {
		return poaseats.Seat{}, false, errors.New("mock: seat read failure")
	}
	if uboId == "" {
		return poaseats.Seat{}, false, nil
	}
	for _, s := range m.Seats {
		if s.UboId == uboId {
			return s, true, nil
		}
	}
	return poaseats.Seat{}, false, nil
}

func (m *MockPoaSeatsDb) AdmitSeat(seat poaseats.Seat) error {
	seat.Account = poaseats.NormalizeAccount(seat.Account)
	if seat.Account == "" {
		return errors.New("mock: empty account")
	}
	if seat.AdmittedHeight == 0 {
		return errors.New("mock: refusing to admit at height 0")
	}
	if _, exists := m.Seats[seat.Account]; exists {
		return fmt.Errorf("mock: %s already holds a seat", seat.Account)
	}
	if seat.UboId != "" {
		if held, exists, _ := m.GetSeatByUbo(seat.UboId); exists {
			return fmt.Errorf("mock: ubo already holds the seat for %s", held.Account)
		}
	}
	m.Seats[seat.Account] = seat
	return nil
}

func (m *MockPoaSeatsDb) SetSeating(account string, height uint64) error {
	acct := poaseats.NormalizeAccount(account)
	s, ok := m.Seats[acct]
	if !ok {
		return nil
	}
	// Mirrors the real store's monotonic guard: an older election must never
	// clear a live exit and release a collateral hold.
	if s.LastSeatedHeight > height {
		return nil
	}
	s.LastSeatedHeight = height
	s.ExitHeight = 0
	m.Seats[acct] = s
	return nil
}

func (m *MockPoaSeatsDb) SetExit(account string, height uint64) error {
	acct := poaseats.NormalizeAccount(account)
	s, ok := m.Seats[acct]
	if !ok {
		return nil
	}
	// Same guard as the real store: an exit already recorded is never moved
	// forward, so the halt clock cannot be restarted by later elections.
	if s.LastSeatedHeight == 0 || s.ExitHeight != 0 {
		return nil
	}
	s.ExitHeight = height
	m.Seats[acct] = s
	return nil
}

// Seed is a test helper: admit a seat directly, panicking on a rule violation so
// a malformed fixture fails loudly at setup instead of subtly at assert time.
func (m *MockPoaSeatsDb) Seed(account, ubo string, admittedHeight uint64) {
	if err := m.AdmitSeat(poaseats.Seat{
		Account:        account,
		UboId:          ubo,
		AdmittedHeight: admittedHeight,
	}); err != nil {
		panic(err)
	}
}
