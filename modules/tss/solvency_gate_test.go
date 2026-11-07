package tss

import (
	"errors"
	"testing"

	"vsc-node/modules/common/consensusversion"
	"vsc-node/modules/common/params"
	systemconfig "vsc-node/modules/common/system-config"
	tss_db "vsc-node/modules/db/vsc/tss"
	stateEngine "vsc-node/modules/state-processing"

	promise "github.com/chebyrash/promise"
)

// fakeSolvencyScheduler is a minimal GetScheduler for the gate tests: the
// governance FLAG (halted) and the M1.1b contract theft FLAG (theftHalted), plus
// the chain-active consensus version (minVer) the vault-rotation-v2 in-force
// gate resolves from the on-chain election floor.
type fakeSolvencyScheduler struct {
	halted      bool
	theftHalted bool
	minVer      consensusversion.Version
}

func (f *fakeSolvencyScheduler) GetSchedule(uint64) []stateEngine.WitnessSlot { return nil }
func (f *fakeSolvencyScheduler) TssMinimumConsensusVersion(uint64) consensusversion.Version {
	return f.minVer
}
func (f *fakeSolvencyScheduler) BtcKeysignHalted() bool { return f.halted }
func (f *fakeSolvencyScheduler) BtcTheftHalted() bool   { return f.theftHalted }

// TestBtcKeysignFrozen_FlagAndScope covers the deterministic FLAG layer and the
// BTC-only scoping. The SIGNAL layer stays inert here (MainnetConfig ships an
// empty BtcVaultAddresses => fail open), so nil contractState/da are never
// dereferenced — which is exactly the production/test invariant we rely on.
func TestBtcKeysignFrozen_FlagAndScope(t *testing.T) {
	sconf := systemconfig.MainnetConfig()
	btc := sconf.OracleParams().ContractId("BTC")
	if btc == "" {
		t.Fatal("expected a mainnet BTC contract id to be configured")
	}
	btcKey := btc + "-main"
	nonBtcKey := "vsc1SomeEthKeyNotBtc-main"

	cases := []struct {
		name        string
		halted      bool // M1.1a governance flag
		theftHalted bool // M1.1b contract theft flag
		keyId       string
		want        bool
	}{
		{"both flags off, BTC key -> not frozen", false, false, btcKey, false},
		{"gov flag on, BTC key -> frozen", true, false, btcKey, true},
		{"gov flag on, non-BTC key -> not frozen (scope)", true, false, nonBtcKey, false},
		{"gov flag on, empty key -> not frozen", true, false, "", false},
		{"gov flag on, bare contract id (no -suffix) -> not frozen", true, false, btc, false},
		// M1.1b: the theft flag freezes independently of the governance flag.
		{"theft flag on, BTC key -> frozen", false, true, btcKey, true},
		{"theft flag on, non-BTC key -> not frozen (scope)", false, true, nonBtcKey, false},
		{"both flags on, BTC key -> frozen", true, true, btcKey, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mgr := &TssManager{sconf: sconf, scheduler: &fakeSolvencyScheduler{halted: tc.halted, theftHalted: tc.theftHalted}}
			if got := mgr.btcKeysignFrozen(tc.keyId); got != tc.want {
				t.Fatalf("btcKeysignFrozen(%q, halted=%v, theftHalted=%v) = %v, want %v", tc.keyId, tc.halted, tc.theftHalted, got, tc.want)
			}
		})
	}
}

func TestParseSupplySats(t *testing.T) {
	cases := []struct {
		in   string
		want uint64
		ok   bool
	}{
		{"123456", 123456, true},
		{`"7890"`, 7890, true},
		{"  42  ", 42, true},
		{"0", 0, true},
		{"", 0, false},
		{"abc", 0, false},
		{"-5", 0, false},
		{"1.5", 0, false},
		{"0x10", 0, false},
	}
	for _, tc := range cases {
		got, ok := parseSupplySats([]byte(tc.in))
		if ok != tc.ok || (ok && got != tc.want) {
			t.Fatalf("parseSupplySats(%q) = (%d, %v), want (%d, %v)", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

// flagOnConfig wraps a real SystemConfig, overriding ONLY ConsensusParams so the
// vault-rotation-v2 flag reads as ACTIVE while OracleParams (the BTC contract id
// used by isBtcVaultKey) stays real. Embedding the interface gives every other
// method for free — no 15-method fake.
type flagOnConfig struct {
	systemconfig.SystemConfig
	cp params.ConsensusParams
}

func (f flagOnConfig) ConsensusParams() params.ConsensusParams { return f.cp }

// TestShouldSkipReshareForVaultRotation pins the load-bearing per-keyId gate-off
// decision at the tss.go reshare loop (M1.3, U-1): the BTC vault key is skipped
// ONLY when the rotation flag is active and only at/after the pinned height;
// every OTHER chain keeps resharing (per-keyId, never loop-level); and the whole
// thing is INERT (never skips) while the flag is off — the property that lets the
// binary dark-launch before governance pins an activation height.
func TestShouldSkipReshareForVaultRotation(t *testing.T) {
	base := systemconfig.MainnetConfig()
	btc := base.OracleParams().ContractId("BTC")
	if btc == "" {
		t.Fatal("expected a mainnet BTC contract id to be configured")
	}
	btcKey := btc + "-main"
	siblingKey := "vsc1SomeEthKeyNotBtc-main"

	// Flag ON at height 100, keeping real mainnet OracleParams via embedding.
	cpOn := base.ConsensusParams() // returned by value → safe to mutate our copy
	cpOn.VaultRotationV2ActivationHeight = 100
	onCfg := flagOnConfig{SystemConfig: base, cp: cpOn}

	cases := []struct {
		name  string
		sconf systemconfig.SystemConfig
		keyId string
		bh    uint64
		want  bool
	}{
		// Flag OFF (real MainnetConfig, activation height 0): NEVER skip — inert.
		{"flag off, BTC key -> never skip (inert)", base, btcKey, 1 << 40, false},
		{"flag off, sibling key -> never skip", base, siblingKey, 1 << 40, false},
		// Flag ON: the guard short-circuits below the activation height and for
		// non-BTC keys (no vault-state read). With nil deps here the retiring set is
		// empty, so a BTC key that reaches the read is treated as the ACTIVE/only gen
		// (not superseded) and RESHARES (L9-1) — the superseded-skip discrimination is
		// covered by TestSkipReshareForSupersededGen below (needs a populated set).
		{"flag on, below height, BTC key -> guard short-circuits", onCfg, btcKey, 99, false},
		{"flag on, at height, BTC active/only gen -> reshares (L9-1)", onCfg, btcKey, 100, false},
		{"flag on, above height, BTC active/only gen -> reshares (L9-1)", onCfg, btcKey, 200, false},
		{"flag on, sibling key -> keep resharing (per-keyId)", onCfg, siblingKey, 200, false},
		{"flag on, empty key -> not BTC -> keep", onCfg, "", 200, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mgr := &TssManager{sconf: tc.sconf}
			if got := mgr.shouldSkipReshareForVaultRotation(tc.keyId, tc.bh); got != tc.want {
				t.Fatalf("shouldSkipReshareForVaultRotation(%q, bh=%d) = %v, want %v", tc.keyId, tc.bh, got, tc.want)
			}
		})
	}
}

// TestVaultRotationV2InForce_PinOrFloor pins the TSS-side activation form
// (vault_rotation_gate.go): the shared gate is the height pin OR the attested
// 0.8.0 chain-active floor — never the bare pin — so the TSS half of the batch
// (reshare skip, S3 output scoping, retiring readiness) activates together with
// the contract-execution half on a floor-only network, where the pin must stay 0.
// A nil scheduler resolves pin-only (fail-safe fallback for test construction).
func TestVaultRotationV2InForce_PinOrFloor(t *testing.T) {
	base := systemconfig.MainnetConfig()

	pinned := base.ConsensusParams()
	pinned.VaultRotationV2ActivationHeight = 100

	cases := []struct {
		name      string
		sconf     systemconfig.SystemConfig
		scheduler GetScheduler
		bh        uint64
		want      bool
	}{
		// Shipped networks: pin 0, floor below 0.8.0 → inert.
		{"unpinned, floor 0.7.0 -> inert", base, &fakeSolvencyScheduler{minVer: consensusversion.V0_7_0}, 1 << 40, false},
		{"unpinned, nil scheduler -> inert (pin-only fallback)", base, nil, 1 << 40, false},
		// Pinned ephemeral/devnet path: the pin wins and short-circuits before the
		// election read — the scheduler's version is IGNORED (0.0.0 here).
		{"pinned at 100, bh=0 (below pin) -> inert", flagOnConfig{SystemConfig: base, cp: pinned}, &fakeSolvencyScheduler{}, 0, false},
		{"pinned at 100, at pin height -> in force", flagOnConfig{SystemConfig: base, cp: pinned}, &fakeSolvencyScheduler{}, 100, true},
		{"pinned at 100, above pin height -> in force", flagOnConfig{SystemConfig: base, cp: pinned}, &fakeSolvencyScheduler{}, 200, true},
		// Floor-only (mainnet path): pin 0 everywhere, the attested 0.8.0 floor activates.
		{"floor-only, 0.8.0 active -> in force", base, &fakeSolvencyScheduler{minVer: consensusversion.V0_8_0}, 1 << 40, true},
		{"floor-only, above 0.8.0 -> in force", base, &fakeSolvencyScheduler{minVer: consensusversion.Version{Major: 0, Consensus: 9}}, 1 << 40, true},
		{"floor-only, floor still 0.7.0 -> inert", base, &fakeSolvencyScheduler{minVer: consensusversion.V0_7_0}, 12345, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mgr := &TssManager{sconf: tc.sconf, scheduler: tc.scheduler}
			if got := mgr.vaultRotationV2InForce(tc.bh); got != tc.want {
				t.Fatalf("vaultRotationV2InForce(bh=%d) = %v, want %v", tc.bh, got, tc.want)
			}
		})
	}
	var noSconf TssManager
	if noSconf.vaultRotationV2InForce(1 << 40) {
		t.Fatal("nil sconf must resolve inert (fail-safe)")
	}
}

// TestShouldSkipReshareForVaultRotation_FloorActivation proves the floor path
// reaches the L9-1 set computation instead of short-circuiting at the guard: on
// a floor-only network (pin 0, attested 0.8.0) the BTC vault key's skip decision
// flows through the shared retiring predicate (empty set with nil deps → the
// ACTIVE/only gen reshares), byte-identically to the pinned path.
func TestShouldSkipReshareForVaultRotation_FloorActivation(t *testing.T) {
	base := systemconfig.MainnetConfig()
	btc := base.OracleParams().ContractId("BTC")
	if btc == "" {
		t.Fatal("expected a mainnet BTC contract id to be configured")
	}
	btcKey := btc + "-main"
	mgr := &TssManager{
		sconf:     base,
		scheduler: &fakeSolvencyScheduler{minVer: consensusversion.V0_8_0},
	}
	if mgr.vaultRotationV2InForce(1<<40) != true {
		t.Fatal("expected the floor-only gate to be in force")
	}
	if got := mgr.shouldSkipReshareForVaultRotation(btcKey, 1<<40); got {
		t.Fatalf("floor-activated gate, active/only gen: shouldSkipReshareForVaultRotation(%q) = true, want false (L9-1: the active gen always reshares)", btcKey)
	}
}

// stubTssKeys is a TssKeys that never finds a key — enough to satisfy the deps
// construction inside btcSignRefused without a live Mongo.
type stubTssKeys struct{}

func (stubTssKeys) Init() error                                         { return nil }
func (stubTssKeys) Start() *promise.Promise[any]                        { return nil }
func (stubTssKeys) Stop() error                                         { return nil }
func (stubTssKeys) InsertKey(string, tss_db.TssKeyAlgo, uint64) error   { return nil }
func (stubTssKeys) FindKey(string) (tss_db.TssKey, error)               { return tss_db.TssKey{}, errStubNotFound }
func (stubTssKeys) SetKey(tss_db.TssKey) error                          { return nil }
func (stubTssKeys) FindNewKeys(uint64) ([]tss_db.TssKey, error)         { return nil, nil }
func (stubTssKeys) FindEpochKeys(uint64) ([]tss_db.TssKey, error)       { return nil, nil }
func (stubTssKeys) FindDeprecatingKeys(uint64) ([]tss_db.TssKey, error) { return nil, nil }
func (stubTssKeys) FindNewlyRetired(uint64) ([]tss_db.TssKey, error)    { return nil, nil }
func (stubTssKeys) DeprecateLegacyKeys() error                          { return nil }
func (stubTssKeys) SetSignatureVerified(string) error                   { return nil }

var errStubNotFound = errors.New("stub: not found")

// TestBtcSignRefused_FloorOnlyActivation is the VR2-02 wiring-gap regression
// test: the TSS half must be in force under EXACTLY the conditions that put the
// contract-execution half in force — a floor-only activation (mainnet path,
// pin 0, attested floor 0.8.0). A sign for a generation that cannot exist
// without a vault registry (mainv1), carrying a digest that is neither the
// BRK-2 check-sig nor a proven successor sweep, must be REFUSED once the gate
// is active; while the floor is below 0.8.0 the gate stays byte-identically
// inert (M1.1a-only behaviour).
func TestBtcSignRefused_FloorOnlyActivation(t *testing.T) {
	base := systemconfig.MainnetConfig()
	cp := base.ConsensusParams()
	bh := uint64(1 << 40)
	btc := base.OracleParams().ContractId("BTC")
	if btc == "" {
		t.Fatal("expected a mainnet BTC contract id to be configured")
	}

	// Precondition: the pin can never activate on mainnet (0 = disabled).
	if cp.VaultRotationV2ActivationHeight != 0 || cp.VaultRotationV2Enabled(bh) {
		t.Fatalf("mainnet pin must be 0/inert, got pin=%d enabled(bh)=%v",
			cp.VaultRotationV2ActivationHeight, cp.VaultRotationV2Enabled(bh))
	}

	// The contract-execution half is LIVE under floor-only activation — the
	// same gate form drives WithVaultRotationV2 in transactions.go.
	if !stateEngine.VaultRotationV2InForce(cp, bh, consensusversion.V0_8_0) {
		t.Fatal("contract-execution half is not in force under floor-only activation")
	}

	// The TSS half must agree: S3 output scoping binds.
	mgr := &TssManager{sconf: base, scheduler: &fakeSolvencyScheduler{minVer: consensusversion.V0_8_0}, tssKeys: stubTssKeys{}}
	if !mgr.btcSignRefused(btc+"-mainv1", make([]byte, 32), bh) {
		t.Fatal("S3 output scoping did not bind under floor-only activation — the contract-execution half is live while the TSS half stays inert (theft oracle open)")
	}

	// Below the line (floor still 0.7.0, pin 0): byte-identically inert.
	mgrPre := &TssManager{sconf: base, scheduler: &fakeSolvencyScheduler{minVer: consensusversion.V0_7_0}, tssKeys: stubTssKeys{}}
	if mgrPre.btcSignRefused(btc+"-mainv1", make([]byte, 32), bh) {
		t.Fatal("S3 must stay inert while the floor is below 0.8.0")
	}
}

// TestSkipReshareForSupersededGen is the L9-1 core: given the shared retiring
// predicate's KeyIds (superseded = retiring/draining/inactive gens), reshare is
// skipped IFF the key is one of those — the ACTIVE gen (never in the set) always
// reshares so its signer set follows committee churn instead of freezing. This is
// the discrimination the datalayer-backed shouldSkip wraps; the set itself is
// proven by TestComputeRetiringSignerSet.
func TestSkipReshareForSupersededGen(t *testing.T) {
	activeKey := "btcContract-main"     // gen 0, ACTIVE — must keep resharing
	retiringKey := "btcContract-mainv1" // gen 1, superseded — skip reshare
	drainingKey := "btcContract-mainv2" // gen 2, superseded — skip reshare

	// Populated set: gens 1 and 2 are superseded (fund-holding retiring/draining/
	// inactive); the active gen 0 is absent.
	superseded := map[string]bool{retiringKey: true, drainingKey: true}

	cases := []struct {
		name  string
		set   map[string]bool
		keyId string
		want  bool
	}{
		{"active gen not in superseded set -> reshares (L9-1 no-freeze)", superseded, activeKey, false},
		{"retiring gen in set -> skip reshare", superseded, retiringKey, true},
		{"draining gen in set -> skip reshare", superseded, drainingKey, true},
		// Empty set (no rotation in progress, or a corrupt/absent "v" collapsing the
		// set): NOTHING is skipped -> everything reshares (fail-SAFE liveness).
		{"empty set, active gen -> reshares", map[string]bool{}, activeKey, false},
		{"empty set, would-be superseded gen -> reshares (fail-safe)", map[string]bool{}, retiringKey, false},
		{"nil set -> reshares (no panic)", nil, retiringKey, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := skipReshareForSupersededGen(tc.set, tc.keyId); got != tc.want {
				t.Fatalf("skipReshareForSupersededGen(%v, %q) = %v, want %v", tc.set, tc.keyId, got, tc.want)
			}
		})
	}
}
