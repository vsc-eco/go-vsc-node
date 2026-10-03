package poaseats_test

import (
	"encoding/json"
	"math/rand"
	"os"
	"testing"

	"vsc-node/lib/test_utils"
	"vsc-node/modules/db/vsc/poaseats"
)

// TestEmitMinerHistory drives the REAL POA seat state machine through randomised
// operation sequences and captures {fn, state_before, state_after} for every
// call, so lever3's trace-miner can mine the system's OWN laws from its actual
// behaviour rather than from a catalog of expected ones.
//
// This is the Phase-1 "run the machine" step. It is not a unit test and asserts
// nothing: its output is the history artifact. Enabled by POA_MINE=1.
func TestEmitMinerHistory(t *testing.T) {
	if os.Getenv("POA_MINE") == "" {
		t.Skip("set POA_MINE=1 to emit the miner history")
	}

	type snap map[string]interface{}
	type step struct {
		Fn          string `json:"fn"`
		StateBefore snap   `json:"state_before"`
		StateAfter  snap   `json:"state_after"`
	}

	const haltWindow = uint64(120) // mocknet PoaExitHaltBlocks

	history := []step{}
	rng := rand.New(rand.NewSource(0xC0FFEE))

	// Several independent runs so the miner sees varied trajectories.
	for run := 0; run < 40; run++ {
		db := test_utils.NewMockPoaSeatsDb()
		height := uint64(100)
		accounts := []string{"alice", "bob", "carol", "dave", "erin", "frank", "grace", "heidi"}
		admitted := []string{}

		observe := func() snap {
			seats, _ := db.GetSeatsAtHeight(height)
			var seated, exited, neverSeated, halted, bootstrap uint64
			for _, s := range seats {
				switch {
				case s.LastSeatedHeight == 0:
					neverSeated++
				case s.ExitHeight == 0:
					seated++
				default:
					exited++
				}
				if s.Bootstrap {
					bootstrap++
				}
				// The exit-halt predicate, mirrored: held while seated, or
				// while inside the window after exit.
				if s.LastSeatedHeight > 0 && (s.ExitHeight == 0 || height < s.ExitHeight+haltWindow) {
					halted++
				}
			}
			return snap{
				"height":       height,
				"seats_total":  uint64(len(seats)),
				"seated":       seated,
				"exited":       exited,
				"never_seated": neverSeated,
				"halted":       halted,
				"bootstrap":    bootstrap,
			}
		}

		record := func(fn string, before snap, act func()) {
			act()
			history = append(history, step{Fn: fn, StateBefore: before, StateAfter: observe()})
		}

		for op := 0; op < 30; op++ {
			before := observe()
			switch rng.Intn(4) {
			case 0: // admit a new seat
				if len(admitted) < len(accounts) {
					acct := accounts[len(admitted)]
					record("admit", before, func() {
						if err := db.AdmitSeat(poaseats.Seat{
							Account: acct, UboId: "ubo-" + acct, AdmittedHeight: height,
						}); err == nil {
							admitted = append(admitted, acct)
						}
					})
				}
			case 1: // a seat is present in a ratified election
				if len(admitted) > 0 {
					acct := admitted[rng.Intn(len(admitted))]
					record("seat", before, func() { _ = db.SetSeating(acct, height) })
				}
			case 2: // a seat is absent from a ratified election
				if len(admitted) > 0 {
					acct := admitted[rng.Intn(len(admitted))]
					record("exit", before, func() { _ = db.SetExit(acct, height) })
				}
			case 3: // chain advances
				record("advance", before, func() { height += uint64(1 + rng.Intn(80)) })
			}
		}
	}

	blob, err := json.MarshalIndent(history, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	out := os.Getenv("POA_MINE_OUT")
	if out == "" {
		out = "/home/clauderfly/poa-testrun/miner/history.json"
	}
	if err := os.WriteFile(out, blob, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %d transitions to %s", len(history), out)
}
