package chain

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"os"
	"slices"
	"strconv"
	"strings"

	"vsc-node/lib/btcvault"
	"vsc-node/lib/datalayer"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/rpcclient"
	"github.com/ipfs/go-cid"
	"go.mongodb.org/mongo-driver/mongo"
)

// N4 — the BTC solvency watch's observation (from 0.13.0; the vote and its
// tally: modules/oracle/solvency_watch.go and state-processing btc_solvency.go).
//
// THORChain's nodes report what their own chain daemon sees in the vault and
// halt the chain when 2/3 see less than the book. The BTC mapping contract
// keeps its book as UTXOs, so a witness checks each one: every confirmed
// registered UTXO (id >= btcvault.UtxoConfirmedPoolStart, each SPV-proven when
// it was registered) that no authorized spend has reserved must still be
// unspent on its own bitcoind. One that is not is either a spend the contract
// never authorized (a theft, caught here without anyone reporting it) or a
// registration of an output that never existed (a bad deposit proof, which
// the contract's theft report cannot catch). An authorized spend is never
// counted: an unmap reserves its inputs ("ru-") when it is built, and a
// migration sweep lists its inputs in its "ms-" record, in flight while its
// txid is in "msl" (it reserves only unconfirmed-pool inputs). The settle
// deletes the UTXO with its reservation or sweep record.
//
// I-40 (testnet 11-05): the watch read only "ru-", so a mined migration sweep
// counted every confirmed input it spent as missing until it settled; the
// gen-13 -> gen-14 sweep (5 inputs, 338,992 sats) made 2/3 of the witnesses
// vote insolvent and halted the BTC vault keysigns at the first checkpoint
// after it was mined.
//
// The observation abstains (returns an error, so no vote) on anything it
// cannot read, and when the node's bitcoind is behind the contract's relayed
// height (an output registered at a height the daemon has not reached would
// look spent). A UTXO found spent is checked again against the contract's
// newest state after the Bitcoin query: an authorized spend is reserved
// minutes before it can be mined, so a reservation made after the first read
// is seen then, and the outpoint is dropped.

// SolvencyStateReader returns a reader over the BTC contract's newest committed
// state: absent keys are (nil, false, nil), unreadable ones an error.
type SolvencyStateReader func() (func(key string) ([]byte, bool, error), error)

// SolvencyL1 is the witness's own Bitcoin node.
type SolvencyL1 interface {
	Tip() (uint64, error)
	// Unspent reports whether the outpoint is in the confirmed UTXO set.
	Unspent(txid string, vout uint32) (bool, error)
}

// SolvencyObservation is what a witness saw.
type SolvencyObservation struct {
	Insolvent   bool
	MissingSats int64
	Missing     int
	Checked     int
	Tip         uint64
	Digest      string // sha256 of the sorted missing outpoints, hex; "" when none
}

type solvencyCandidate struct {
	id uint16
	op btcvault.UtxoOutpoint
}

// ObserveBtcSolvency checks the book against L1. insolvent iff the missing
// sats exceed gapSats.
func ObserveBtcSolvency(state SolvencyStateReader, l1 SolvencyL1, gapSats uint64) (SolvencyObservation, error) {
	read, err := state()
	if err != nil {
		return SolvencyObservation{}, err
	}
	rawH, ok, err := read(btcvault.BtcRelayedHeightKey)
	if err != nil || !ok {
		return SolvencyObservation{}, fmt.Errorf("contract relayed height unreadable (present=%v): %w", ok, err)
	}
	relayed, err := strconv.ParseUint(string(rawH), 10, 64)
	if err != nil {
		return SolvencyObservation{}, fmt.Errorf("contract relayed height %q: %w", rawH, err)
	}
	tip, err := l1.Tip()
	if err != nil {
		return SolvencyObservation{}, fmt.Errorf("bitcoind tip: %w", err)
	}
	if tip < relayed {
		return SolvencyObservation{}, fmt.Errorf("bitcoind tip %d is behind the contract's relayed height %d", tip, relayed)
	}
	obs := SolvencyObservation{Tip: tip}
	candidates, err := spentCandidates(read, l1, &obs)
	if err != nil {
		return SolvencyObservation{}, err
	}
	if len(candidates) > 0 {
		// Second look at the contract's newest state (see above).
		fresh, err := state()
		if err != nil {
			return SolvencyObservation{}, err
		}
		freshSweeping, err := sweepInputs(fresh)
		if err != nil {
			return SolvencyObservation{}, err
		}
		kept := candidates[:0]
		for _, c := range candidates {
			still, err := unreservedOutpoint(fresh, freshSweeping, c.id)
			if err != nil {
				return SolvencyObservation{}, err
			}
			if still != nil && *still == c.op {
				kept = append(kept, c)
			}
		}
		candidates = kept
	}
	missing := make([]string, 0, len(candidates))
	for _, c := range candidates {
		if c.op.Amount > 0 && obs.MissingSats <= math.MaxInt64-c.op.Amount {
			obs.MissingSats += c.op.Amount
		}
		missing = append(missing, c.op.TxId+":"+strconv.FormatUint(uint64(c.op.Vout), 10))
	}
	obs.Missing = len(missing)
	if len(missing) > 0 {
		slices.Sort(missing)
		sum := sha256.Sum256([]byte(strings.Join(missing, "\n")))
		obs.Digest = hex.EncodeToString(sum[:])
	}
	obs.Insolvent = obs.MissingSats > 0 && uint64(obs.MissingSats) > gapSats
	return obs, nil
}

// spentCandidates walks the confirmed, unreserved book and returns the
// outpoints L1 does not have.
func spentCandidates(read func(string) ([]byte, bool, error), l1 SolvencyL1, obs *SolvencyObservation) ([]solvencyCandidate, error) {
	rawR, ok, err := read(btcvault.UtxoRegistryKey)
	if err != nil {
		return nil, fmt.Errorf("utxo registry: %w", err)
	}
	if !ok {
		return nil, nil // nothing registered
	}
	reg, err := btcvault.UnmarshalUtxoRegistry(rawR)
	if err != nil {
		return nil, err
	}
	sweeping, err := sweepInputs(read)
	if err != nil {
		return nil, err
	}
	var out []solvencyCandidate
	for _, e := range reg {
		if e.Id < btcvault.UtxoConfirmedPoolStart {
			continue
		}
		op, err := unreservedOutpoint(read, sweeping, e.Id)
		if err != nil {
			return nil, err
		}
		if op == nil {
			continue // reserved by an authorized spend
		}
		obs.Checked++
		live, err := l1.Unspent(op.TxId, op.Vout)
		if err != nil {
			return nil, fmt.Errorf("gettxout %s:%d: %w", op.TxId, op.Vout, err)
		}
		if !live {
			out = append(out, solvencyCandidate{id: e.Id, op: *op})
		}
	}
	return out, nil
}

// sweepInputs returns the UTXO ids the in-flight migration sweeps spend: the
// inputs of every "ms-" record whose txid is in "msl". A listed sweep without a
// record adds nothing (the contract writes and deletes the two together); an
// unreadable or malformed value is an error, so the watch abstains.
func sweepInputs(read func(string) ([]byte, bool, error)) (map[uint16]bool, error) {
	raw, ok, err := read(btcvault.MigrationSweepRegistryKey)
	if err != nil {
		return nil, fmt.Errorf("migration sweep registry: %w", err)
	}
	if !ok || len(raw) == 0 {
		return nil, nil
	}
	txids, err := btcvault.UnmarshalMigrationSweepRegistry(raw)
	if err != nil {
		return nil, err
	}
	ids := map[uint16]bool{}
	for _, txid := range txids {
		rec, ok, err := read(btcvault.MigrationSweepKey(txid))
		if err != nil {
			return nil, fmt.Errorf("migration sweep %s: %w", txid, err)
		}
		if !ok {
			continue
		}
		in, err := btcvault.MigrationSweepInputIds(rec)
		if err != nil {
			return nil, fmt.Errorf("migration sweep %s: %w", txid, err)
		}
		for _, id := range in {
			ids[id] = true
		}
	}
	return ids, nil
}

// unreservedOutpoint returns registered id's outpoint, or nil when it is
// reserved, spent by an in-flight migration sweep (sweeping), no longer
// registered, or has no record.
func unreservedOutpoint(read func(string) ([]byte, bool, error), sweeping map[uint16]bool, id uint16) (*btcvault.UtxoOutpoint, error) {
	if sweeping[id] {
		return nil, nil
	}
	res, ok, err := read(btcvault.ReservedUtxoKey(id))
	if err != nil {
		return nil, fmt.Errorf("reservation %d: %w", id, err)
	}
	if ok && len(res) > 0 {
		return nil, nil
	}
	rec, ok, err := read(btcvault.UtxoKey(id))
	if err != nil {
		return nil, fmt.Errorf("utxo %d: %w", id, err)
	}
	if !ok {
		return nil, nil
	}
	op, err := btcvault.UnmarshalUtxoOutpoint(rec)
	if err != nil {
		return nil, fmt.Errorf("utxo %d: %w", id, err)
	}
	return &op, nil
}

// btcSolvencyL1 is SolvencyL1 over one bitcoind RPC client.
type btcSolvencyL1 struct{ client *rpcclient.Client }

func (b btcSolvencyL1) Tip() (uint64, error) {
	n, err := b.client.GetBlockCount()
	if err != nil {
		return 0, err
	}
	if n < 0 {
		return 0, errors.New("negative block count")
	}
	return uint64(n), nil
}

func (b btcSolvencyL1) Unspent(txid string, vout uint32) (bool, error) {
	h, err := chainhash.NewHashFromStr(txid)
	if err != nil {
		return false, err
	}
	res, err := b.client.GetTxOut(h, vout, false)
	if err != nil {
		return false, err
	}
	return res != nil, nil
}

// BtcSolvencyInputs returns the BTC contract's state reader and this node's
// bitcoind (one client; call done when finished), or ok=false when either is
// not configured.
func (c *ChainOracle) BtcSolvencyInputs() (state SolvencyStateReader, l1 SolvencyL1, done func(), ok bool) {
	relay, isBtc := c.chainRelayers["BTC"].(*bitcoinRelayer)
	if !isBtc || relay.ContractId() == "" || relay.rpcConfig.Host == "" || c.contractState == nil || c.da == nil {
		return nil, nil, nil, false
	}
	client, err := relay.connect()
	if err != nil {
		return nil, nil, nil, false
	}
	contractId := relay.ContractId()
	state = func() (func(string) ([]byte, bool, error), error) {
		out, err := c.contractState.GetLastOutputStrict(contractId, math.MaxInt64)
		if errors.Is(err, mongo.ErrNoDocuments) {
			return func(string) ([]byte, bool, error) { return nil, false, nil }, nil
		}
		if err != nil {
			return nil, err
		}
		if out.StateMerkle == "" {
			return func(string) ([]byte, bool, error) { return nil, false, nil }, nil
		}
		root, err := cid.Parse(out.StateMerkle)
		if err != nil {
			return nil, err
		}
		local := c.da.LocalOnly()
		if local == nil {
			return nil, errors.New("local datalayer view unavailable")
		}
		return func(key string) ([]byte, bool, error) {
			kc, err := datalayer.GetPathCid(local, root, key)
			if errors.Is(err, os.ErrNotExist) {
				return nil, false, nil
			}
			if err != nil {
				return nil, false, err
			}
			if kc == nil {
				return nil, false, nil
			}
			raw, err := local.GetRaw(*kc)
			if err != nil {
				return nil, false, err
			}
			return raw, true, nil
		}, nil
	}
	return state, btcSolvencyL1{client: client}, client.Shutdown, true
}
