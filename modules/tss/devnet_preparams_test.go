package tss

import (
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	systemconfig "vsc-node/modules/common/system-config"

	"github.com/bnb-chain/tss-lib/v3/crypto/paillier"
	ecKeyGen "github.com/bnb-chain/tss-lib/v3/ecdsa/keygen"
)

// tinyPreParams is a pre-params set small enough to build by hand that satisfies
// every relation tss-lib's GeneratePreParams establishes: Paillier P=11, Q=13;
// Sophie Germain p=5, q=11 (safe primes 11, 23); Alpha=2, Beta=28 (2*28 = 56 =
// 1 mod 55); H1 = 3^2, H2 = H1^2 mod 253.
func tinyPreParams() *ecKeyGen.LocalPreParams {
	b := big.NewInt
	return &ecKeyGen.LocalPreParams{
		PaillierSK: &paillier.PrivateKey{
			PublicKey: paillier.PublicKey{N: b(143)},
			LambdaN:   b(60),
			PhiN:      b(120),
			P:         b(11),
			Q:         b(13),
		},
		NTildei: b(253),
		H1i:     b(9),
		H2i:     b(81),
		Alpha:   b(2),
		Beta:    b(28),
		P:       b(5),
		Q:       b(11),
	}
}

func TestValidateDevnetPreParams_AcceptsAConsistentSet(t *testing.T) {
	if err := ValidateDevnetPreParams(tinyPreParams()); err != nil {
		t.Fatalf("consistent set rejected: %v", err)
	}
}

// Any single damaged field must be caught.
func TestValidateDevnetPreParams_RejectsEveryCorruption(t *testing.T) {
	inc := func(x *big.Int) { x.Add(x, big.NewInt(1)) }
	cases := map[string]func(p *ecKeyGen.LocalPreParams){
		"Paillier N":        func(p *ecKeyGen.LocalPreParams) { inc(p.PaillierSK.N) },
		"Paillier PhiN":     func(p *ecKeyGen.LocalPreParams) { inc(p.PaillierSK.PhiN) },
		"Paillier LambdaN":  func(p *ecKeyGen.LocalPreParams) { inc(p.PaillierSK.LambdaN) },
		"Paillier P":        func(p *ecKeyGen.LocalPreParams) { inc(p.PaillierSK.P) },
		"NTildei":           func(p *ecKeyGen.LocalPreParams) { inc(p.NTildei) },
		"safe prime p":      func(p *ecKeyGen.LocalPreParams) { inc(p.P) },
		"safe prime q":      func(p *ecKeyGen.LocalPreParams) { inc(p.Q) },
		"Alpha":             func(p *ecKeyGen.LocalPreParams) { inc(p.Alpha) },
		"Beta":              func(p *ecKeyGen.LocalPreParams) { inc(p.Beta) },
		"H1i":               func(p *ecKeyGen.LocalPreParams) { inc(p.H1i) },
		"H2i":               func(p *ecKeyGen.LocalPreParams) { inc(p.H2i) },
		"H1i = 1":           func(p *ecKeyGen.LocalPreParams) { p.H1i = big.NewInt(1) },
		"H1i = NTildei":     func(p *ecKeyGen.LocalPreParams) { p.H1i = new(big.Int).Set(p.NTildei) },
		"Alpha missing":     func(p *ecKeyGen.LocalPreParams) { p.Alpha = nil },
		"PhiN missing":      func(p *ecKeyGen.LocalPreParams) { p.PaillierSK.PhiN = nil },
		"Paillier missing":  func(p *ecKeyGen.LocalPreParams) { p.PaillierSK = nil },
		"Paillier Q nil":    func(p *ecKeyGen.LocalPreParams) { p.PaillierSK.Q = nil },
		"safe prime q nil":  func(p *ecKeyGen.LocalPreParams) { p.Q = nil },
		"everything zeroed": func(p *ecKeyGen.LocalPreParams) { *p = ecKeyGen.LocalPreParams{} },
	}
	for name, corrupt := range cases {
		p := tinyPreParams()
		corrupt(p)
		if err := ValidateDevnetPreParams(p); err == nil {
			t.Errorf("%s: corrupted set accepted", name)
		}
	}
}

// Relations alone are not enough: the primes must be prime.
func TestValidateDevnetPreParams_RejectsCompositesThatSatisfyTheRelations(t *testing.T) {
	b := big.NewInt
	// Paillier P = 9: N, PhiN and LambdaN all consistent with it.
	p := tinyPreParams()
	p.PaillierSK = &paillier.PrivateKey{PublicKey: paillier.PublicKey{N: b(117)}, PhiN: b(96), LambdaN: b(24), P: b(9), Q: b(13)}
	if err := ValidateDevnetPreParams(p); err == nil || !strings.Contains(err.Error(), "not prime") {
		t.Errorf("composite Paillier factor: want a primality rejection, got %v", err)
	}
	// p = 7 is prime but 2p+1 = 15 is not; NTilde, Beta, H1/H2 consistent with it.
	p = tinyPreParams()
	p.P, p.Q, p.NTildei = b(7), b(11), b(15*23)
	p.Alpha, p.Beta = b(2), b(39) // 2*39 = 78 = 1 mod 77
	p.H1i, p.H2i = b(4), b(16)
	if err := ValidateDevnetPreParams(p); err == nil || !strings.Contains(err.Error(), "not prime") {
		t.Errorf("non-safe prime: want a primality rejection, got %v", err)
	}
}

func TestValidateDevnetPreParams_SurvivesJSON(t *testing.T) {
	raw, err := json.Marshal(tinyPreParams())
	if err != nil {
		t.Fatal(err)
	}
	var back ecKeyGen.LocalPreParams
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if err := ValidateDevnetPreParams(&back); err != nil {
		t.Fatalf("set no longer valid after a JSON round-trip: %v", err)
	}
}

// Many nodes claiming at once: every set goes to exactly one claimant, a corrupt
// file is set aside as invalid-*, and an exhausted pool says so.
func TestClaimDevnetPreParams_EachSetClaimedExactlyOnce(t *testing.T) {
	dir := t.TempDir()
	raw, _ := json.Marshal(tinyPreParams())
	const sets = 40
	for i := 0; i < sets; i++ {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("set-%04d.json", i)), raw, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	_ = os.WriteFile(filepath.Join(dir, "set-9999.json"), []byte(`{"NTildei":1}`), 0o644)

	var claimed atomic.Int64
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for claimDevnetPreParams(dir) != nil {
				claimed.Add(1)
			}
		}()
	}
	wg.Wait()

	if got := claimed.Load(); got != sets {
		t.Fatalf("claimed %d sets, want exactly %d", got, sets)
	}
	var claimedFiles, invalidFiles, left int
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		switch name := e.Name(); {
		case strings.HasPrefix(name, "claimed-"):
			claimedFiles++
		case strings.HasPrefix(name, "invalid-"):
			invalidFiles++
		default:
			left++
		}
	}
	if claimedFiles != sets || invalidFiles != 1 || left != 0 {
		t.Fatalf("pool after claiming: %d claimed, %d invalid, %d left; want %d, 1, 0", claimedFiles, invalidFiles, left, sets)
	}
	if claimDevnetPreParams(dir) != nil {
		t.Fatal("an exhausted pool must not hand out a set")
	}
}

func TestDevnetPreParamsSource_OnlyOnDevnetWithAPool(t *testing.T) {
	t.Setenv(devnetPreParamsPoolEnv, "")
	if devnetPreParamsSource(nil) != nil {
		t.Fatal("no system config: must use live generation")
	}
	if devnetPreParamsSource(systemconfig.DevnetConfig()) != nil {
		t.Fatal("devnet without a pool: must use live generation")
	}
	t.Setenv(devnetPreParamsPoolEnv, t.TempDir())
	if devnetPreParamsSource(systemconfig.DevnetConfig()) == nil {
		t.Fatal("devnet with a pool configured: want the pool source")
	}
	for name, sconf := range map[string]systemconfig.SystemConfig{
		"mainnet": systemconfig.MainnetConfig(),
		"testnet": systemconfig.TestnetConfig(),
	} {
		if devnetPreParamsSource(sconf) != nil {
			t.Fatalf("%s must never take pre-params from a fixture pool", name)
		}
	}
}

// Pre-params generation outlives any session, so it must count as in-flight work
// for the devnet pacemaker.
func TestGeneratePreParams_CountsAsBackgroundWork(t *testing.T) {
	release := make(chan struct{})
	mgr := &TssManager{
		preParams: make(chan ecKeyGen.LocalPreParams, preParamsPoolCap),
		genPreParams: func(time.Duration) (*ecKeyGen.LocalPreParams, error) {
			<-release
			return &ecKeyGen.LocalPreParams{}, nil
		},
	}
	done := make(chan struct{})
	go func() {
		mgr.GeneratePreParams()
		close(done)
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, _, bg := mgr.InFlightCounts(); bg == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("generation in progress is not reported as background work")
		}
		time.Sleep(time.Millisecond)
	}
	close(release)
	<-done
	if _, _, bg := mgr.InFlightCounts(); bg != 0 {
		t.Fatalf("background work still reported after generation finished: %d", bg)
	}
}
