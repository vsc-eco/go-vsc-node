package mapper

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	contractinterface "vsc-node/cmd/mapping-bot/contract-interface"
	"vsc-node/cmd/mapping-bot/database"
	"vsc-node/lib/btcvault"
)

// BOT-ADDR-2 / BOT-DEAD-1: which deposits the bot maps.
//
// The contract credits a deposit by its INSTRUCTION: for every instruction in a map
// it derives one address per deposit-matchable vault generation (active, retiring,
// draining, inactive; utxo-mapping init.go depositAddressGenerations) and credits the
// outputs that pay one of them. A PURGED generation's addresses are no longer
// derived, so a deposit there is not credited and only that generation's CSV backup
// key can move it.
//
// The bot used to watch the single address it handed out per request (the then-active
// generation's) for 30 days after the first request. It therefore kept mapping
// deposits to addresses of purged generations (credited nothing, reported success),
// never saw deposits to an instruction's other live generations, and forgot an
// instruction 30 days after it was first requested even while the address stayed
// creditable.
//
// It now mirrors the contract's rule: the watched set is every known instruction
// (database.InstructionStore) derived with every generation the contract matches,
// rebuilt whenever the vault registry changes. Purged generations are derived too,
// only to raise an alert: their deposits are reported, never mapped. A contract with
// no registry (pre-rotation BTC, the other UTXO chains) has exactly one key pair, the
// legacy pubkey/backupkey, and is handled as a single creditable generation.

// DepositKeySet is one vault generation's keys as the contract matches deposits.
type DepositKeySet struct {
	Generation uint32
	Status     btcvault.VaultStatus
	Primary    []byte
	Backup     []byte
	// Creditable: the contract derives this generation's deposit addresses and
	// credits deposits to them. False only for a PURGED generation (alert only).
	Creditable bool
	// Legacy: no vault registry; the contract's single legacy key pair.
	Legacy bool
}

// DepositKeySets is the contract's deposit-matching view at one moment.
type DepositKeySets struct {
	// Fingerprint changes whenever the generations or their keys change.
	Fingerprint string
	// HasRegistry: the contract matches by its vault registry (BTC vault v2).
	HasRegistry bool
	Sets        []DepositKeySet
}

// FetchVaultKeySets reads the key sets the contract matches deposits against. A read
// error is returned (the caller holds the block and retries); an empty contract
// yields no sets.
func (b *Bot) FetchVaultKeySets(ctx context.Context) (DepositKeySets, error) {
	st, err := b.fetchStateHex(ctx, []string{
		contractinterface.VaultRegistryKey, contractinterface.VaultActiveGenKey,
		contractinterface.PrimaryPublicKeyStateKey, contractinterface.BackupPublicKeyStateKey,
	})
	if err != nil {
		return DepositKeySets{}, err
	}
	raw := st[contractinterface.VaultRegistryKey]
	activeRaw := st[contractinterface.VaultActiveGenKey]
	if len(raw) == 0 {
		if registryMissingAfterRotation(activeRaw) {
			// Generation > 0 is active, so a rotation happened and the registry
			// cannot be empty: this is a broken read, and the legacy slots hold gen
			// 0's (retired) keys. (An empty registry with generation 0 active is the
			// normal pre-fold state: SaveToState writes "v" and "va" on every save,
			// and the contract itself falls back to the legacy pair there.)
			return DepositKeySets{}, fmt.Errorf("vault registry missing while generation %d is active", btcvaultActive(activeRaw))
		}
		primary := st[contractinterface.PrimaryPublicKeyStateKey]
		backup := st[contractinterface.BackupPublicKeyStateKey]
		if len(primary) == 0 {
			return DepositKeySets{Fingerprint: "none"}, nil
		}
		h := sha256.Sum256(append(append([]byte("legacy|"), primary...), backup...))
		return DepositKeySets{
			Fingerprint: "legacy:" + hex.EncodeToString(h[:]),
			Sets: []DepositKeySet{{
				Status: btcvault.VaultStatusActive, Primary: primary, Backup: backup,
				Creditable: true, Legacy: true,
			}},
		}, nil
	}
	vaults, err := btcvault.UnmarshalVaultRegistry(raw)
	if err != nil {
		return DepositKeySets{}, fmt.Errorf("undecodable vault registry: %w", err)
	}
	h := sha256.Sum256(append(append([]byte("v|"), raw...), activeRaw...))
	out := DepositKeySets{Fingerprint: "v:" + hex.EncodeToString(h[:]), HasRegistry: true}
	for _, v := range vaults {
		// Same key condition as the contract: a generation without a real primary
		// AND backup derives no matchable address (a zero backup would make the CSV
		// path unspendable).
		if !realPubKey(v.Primary) || !realPubKey(v.Backup) {
			continue
		}
		creditable := btcvault.IsFundHoldingStatus(v.Status)
		if !creditable && v.Status != btcvault.VaultStatusPurged {
			continue // Pending: no deposits are directed to it
		}
		out.Sets = append(out.Sets, DepositKeySet{
			Generation: v.Generation, Status: v.Status,
			Primary: v.Primary, Backup: v.Backup, Creditable: creditable,
		})
	}
	// Same fail-safe as the contract (depositAddressGenerations): with no creditable,
	// keyed generation (a fresh deploy whose genesis is still pending), it credits the
	// legacy key pair.
	hasCreditable := false
	for _, s := range out.Sets {
		hasCreditable = hasCreditable || s.Creditable
	}
	primary := st[contractinterface.PrimaryPublicKeyStateKey]
	backup := st[contractinterface.BackupPublicKeyStateKey]
	if !hasCreditable && len(primary) > 0 {
		out.Sets = append(out.Sets, DepositKeySet{
			Status: btcvault.VaultStatusActive, Primary: primary, Backup: backup,
			Creditable: true, Legacy: true,
		})
	}
	return out, nil
}

// registryMissingAfterRotation: an empty registry is only legitimate while
// generation 0 is active (pre-fold / fresh deploy). See FetchVaultKeySets.
func registryMissingAfterRotation(activeRaw []byte) bool {
	gen, ok := btcvault.ReadUint32BE(activeRaw)
	return ok && gen > 0
}

func btcvaultActive(activeRaw []byte) uint32 {
	gen, _ := btcvault.ReadUint32BE(activeRaw)
	return gen
}

func realPubKey(k []byte) bool {
	return len(k) == btcvault.CompressedPubKeyLen && !bytes.Equal(k, make([]byte, len(k)))
}

// depositTarget is what an address derives from.
type depositTarget struct {
	Instruction string
	Generation  uint32
	Status      btcvault.VaultStatus
	Creditable  bool
	Legacy      bool
	// Underivable: a stored address no generation the contract knows derives.
	Underivable bool
}

// instructionReloadSkew re-reads instructions touched slightly before the last load,
// so a request written by another bot instance with a slower clock is not missed.
const instructionReloadSkew = 10 * time.Minute

// depositIndex maps every address derived from a known instruction to its target.
type depositIndex struct {
	mu          sync.Mutex
	fingerprint string
	hasRegistry bool
	sets        []DepositKeySet
	known       map[string]struct{}
	byAddr      map[string]depositTarget
	loadedAt    time.Time
}

func (b *Bot) depositIdx() *depositIndex {
	b.depositIdxOnce.Do(func() { b.depositIndex = &depositIndex{} })
	return b.depositIndex
}

// refreshDepositIndex brings the index up to date with the contract's generations
// and the stored instructions. Called once per parsed block.
func (b *Bot) refreshDepositIndex(ctx context.Context) error {
	ks, err := b.gql().FetchVaultKeySets(ctx)
	if err != nil {
		return fmt.Errorf("reading the vault generations: %w", err)
	}
	idx := b.depositIdx()

	idx.mu.Lock()
	if ks.Fingerprint != idx.fingerprint {
		idx.fingerprint = ks.Fingerprint
		idx.hasRegistry = ks.HasRegistry
		idx.sets = ks.Sets
		idx.known = map[string]struct{}{}
		idx.byAddr = map[string]depositTarget{}
		idx.loadedAt = time.Time{}
	}
	since := idx.loadedAt
	idx.mu.Unlock()

	if !since.IsZero() {
		since = since.Add(-instructionReloadSkew)
	}
	startedAt := time.Now().UTC()
	instructions, err := b.instrDB().ListSince(ctx, since)
	if err != nil {
		return fmt.Errorf("reading the deposit instructions: %w", err)
	}

	idx.mu.Lock()
	defer idx.mu.Unlock()
	if idx.fingerprint != ks.Fingerprint {
		return nil // a concurrent refresh moved on; it loads its own set
	}
	for _, instr := range instructions {
		b.addInstructionLocked(idx, instr)
	}
	idx.loadedAt = startedAt
	return nil
}

// noteInstruction adds a just-requested instruction to the loaded index, so this
// instance watches it before its next refresh.
func (b *Bot) noteInstruction(instr string) {
	idx := b.depositIdx()
	idx.mu.Lock()
	defer idx.mu.Unlock()
	if idx.byAddr != nil {
		b.addInstructionLocked(idx, instr)
	}
}

// resetDepositIndex forces a full reload at the next refresh (after a prune).
func (b *Bot) resetDepositIndex() {
	idx := b.depositIdx()
	idx.mu.Lock()
	idx.fingerprint = ""
	idx.mu.Unlock()
}

func (b *Bot) addInstructionLocked(idx *depositIndex, instr string) {
	if _, ok := idx.known[instr]; ok {
		return
	}
	if b.Chain == nil || b.Chain.AddressGen == nil {
		return
	}
	idx.known[instr] = struct{}{}
	for _, s := range idx.sets {
		if len(s.Backup) == 0 {
			continue // legacy contract without a backup key: nothing derivable
		}
		addr, _, err := b.Chain.AddressGen.GenerateDepositAddress(
			hex.EncodeToString(s.Primary), hex.EncodeToString(s.Backup), instr)
		if err != nil {
			b.L.Debug("cannot derive a deposit address", "instruction", instr, "generation", s.Generation, "err", err)
			continue
		}
		idx.byAddr[addr] = depositTarget{
			Instruction: instr, Generation: s.Generation, Status: s.Status,
			Creditable: s.Creditable, Legacy: s.Legacy,
		}
	}
}

// lookupDeposit resolves an output address. found=false: not a deposit address the
// bot knows. found with Creditable=false: a deposit the contract will not credit.
func (b *Bot) lookupDeposit(ctx context.Context, addr string) (depositTarget, bool, error) {
	idx := b.depositIdx()
	idx.mu.Lock()
	t, ok := idx.byAddr[addr]
	hasRegistry := idx.hasRegistry
	idx.mu.Unlock()
	if ok {
		return t, true, nil
	}
	// An address in the legacy address store whose instruction the index has not
	// derived yet (a request on another instance since the last refresh): derive
	// that instruction now and look again.
	instr, err := b.addrDB().GetInstruction(ctx, addr)
	if err != nil {
		if errors.Is(err, database.ErrAddrNotFound) {
			return depositTarget{}, false, nil
		}
		return depositTarget{}, false, err
	}
	b.noteInstruction(instr)
	idx.mu.Lock()
	t, ok = idx.byAddr[addr]
	idx.mu.Unlock()
	if ok {
		return t, true, nil
	}
	if hasRegistry {
		// Stored, but no generation the contract knows derives it: the contract
		// cannot credit it.
		return depositTarget{Instruction: instr, Underivable: true}, true, nil
	}
	// No registry: the stored mapping is the only record (legacy behaviour).
	return depositTarget{Instruction: instr, Creditable: true, Legacy: true}, true, nil
}

// reportUncreditableDeposit logs, once per output, a deposit the contract will not
// credit. It is an operator alert: the coin sits on Bitcoin outside the vault's
// books, and only the generation's CSV backup key can move it.
func (b *Bot) reportUncreditableDeposit(txId string, vout int, sats int64, addr string, t depositTarget, blockHeight uint64) {
	if !warnUnprovableOnce("uncreditable-" + txId + ":" + strconv.Itoa(vout)) {
		return
	}
	if t.Underivable {
		b.L.Error("deposit to a stored address that no vault generation derives; the contract will not credit it, not mapped",
			"btcTxId", txId, "vout", vout, "sats", sats, "address", addr, "instruction", t.Instruction, "blockHeight", blockHeight)
		return
	}
	b.L.Error("deposit to a retired (purged) vault generation; the contract will not credit it, not mapped. Only that generation's CSV backup key can move the coin",
		"btcTxId", txId, "vout", vout, "sats", sats, "address", addr, "generation", t.Generation,
		"instruction", t.Instruction, "blockHeight", blockHeight)
}

// creditableAddresses returns every address the contract would credit for instr.
func (b *Bot) creditableAddresses(instr string) []string {
	idx := b.depositIdx()
	idx.mu.Lock()
	defer idx.mu.Unlock()
	if b.Chain == nil || b.Chain.AddressGen == nil {
		return nil
	}
	var out []string
	for _, s := range idx.sets {
		if !s.Creditable || len(s.Backup) == 0 {
			continue
		}
		addr, _, err := b.Chain.AddressGen.GenerateDepositAddress(
			hex.EncodeToString(s.Primary), hex.EncodeToString(s.Backup), instr)
		if err == nil {
			out = append(out, addr)
		}
	}
	return out
}

// recordDeposits marks the instructions of a map that landed, so they are never
// pruned (database.InstructionStore.DeleteStale).
func (b *Bot) recordDeposits(ctx context.Context, instructions []string) {
	seen := make(map[string]struct{}, len(instructions))
	for _, instr := range instructions {
		if _, ok := seen[instr]; ok {
			continue
		}
		seen[instr] = struct{}{}
		if err := b.instrDB().MarkDeposit(ctx, instr); err != nil {
			b.L.Error("could not record a deposit for its instruction", "instruction", instr, "err", err)
		}
	}
}

// TouchInstruction records a request for instr (created or renewed) and starts
// watching it on this instance. Returns true when the instruction is new.
func (b *Bot) TouchInstruction(ctx context.Context, instr string) (bool, error) {
	created, err := b.instrDB().Touch(ctx, instr)
	if err != nil {
		return false, err
	}
	b.noteInstruction(instr)
	return created, nil
}

// HandleExistingDeposits scans the recent history of every address the contract
// credits for instr (each live generation, plus requested, the address just handed
// out) and maps what it finds. Best effort, like HandleExistingTxs.
func (b *Bot) HandleExistingDeposits(instr, requested string) {
	addrs := map[string]struct{}{}
	if requested != "" {
		addrs[requested] = struct{}{}
	}
	for _, a := range b.creditableAddresses(instr) {
		addrs[a] = struct{}{}
	}
	for a := range addrs {
		b.HandleExistingTxs(a)
	}
}

// ResetDepositIndex forces a full reload of the watched instructions at the next
// block (after the stale-instruction prune).
func (b *Bot) ResetDepositIndex() { b.resetDepositIndex() }
