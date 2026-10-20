package tss

// Devnet-only pre-generated TSS pre-parameters.
//
// Safe-prime generation (GeneratePreParams) costs from 15s to minutes of full CPU
// per set, every node refills its pool after each ceremony, and on a devnet all
// the nodes do it at once on one machine. That spike is what limits a box to one
// devnet at a time. With VSC_DEVNET_PREPARAMS_POOL set on vsc-devnet, a node takes
// ready-made sets from a shared directory instead (one JSON LocalPreParams per
// *.json file). A set is claimed by renaming its file, which is atomic on the
// shared volume, so no two nodes ever hold the same set and no set is used twice.
// A set that fails ValidateDevnetPreParams is renamed invalid-* (the harness
// fails the run on any) and skipped. An empty pool falls back to live generation.

import (
	"encoding/json"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"time"

	systemconfig "vsc-node/modules/common/system-config"

	ecKeyGen "github.com/bnb-chain/tss-lib/v3/ecdsa/keygen"
)

const devnetPreParamsPoolEnv = "VSC_DEVNET_PREPARAMS_POOL"

// devnetPreParamsSource returns a genPreParams replacement backed by the pool, or
// nil (live generation) off devnet or when the pool is not configured.
func devnetPreParamsSource(sconf systemconfig.SystemConfig) func(time.Duration) (*ecKeyGen.LocalPreParams, error) {
	if sconf == nil || sconf.NetId() != "vsc-devnet" {
		return nil
	}
	dir := os.Getenv(devnetPreParamsPoolEnv)
	if dir == "" {
		return nil
	}
	return func(timeout time.Duration) (*ecKeyGen.LocalPreParams, error) {
		if p := claimDevnetPreParams(dir); p != nil {
			return p, nil
		}
		log.Warn("devnet pre-params pool exhausted, generating live", "dir", dir)
		return ecKeyGen.GeneratePreParams(timeout)
	}
}

// claimDevnetPreParams takes the first unclaimed, valid set from dir.
func claimDevnetPreParams(dir string) *ecKeyGen.LocalPreParams {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	host, _ := os.Hostname()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") ||
			strings.HasPrefix(name, "claimed-") || strings.HasPrefix(name, "invalid-") {
			continue
		}
		claimed := filepath.Join(dir, "claimed-"+host+"-"+name)
		if err := os.Rename(filepath.Join(dir, name), claimed); err != nil {
			continue // another node claimed it first
		}
		var p ecKeyGen.LocalPreParams
		raw, err := os.ReadFile(claimed)
		if err == nil {
			err = json.Unmarshal(raw, &p)
		}
		if err == nil {
			err = ValidateDevnetPreParams(&p)
		}
		if err != nil {
			log.Error("invalid devnet pre-params set, skipping", "file", name, "err", err)
			_ = os.Rename(claimed, filepath.Join(dir, "invalid-"+host+"-"+name))
			continue
		}
		log.Info("claimed devnet pre-params set", "file", name)
		return &p
	}
	return nil
}

// ValidateDevnetPreParams checks that a pre-params set is internally consistent
// with how tss-lib builds one (ecdsa/keygen/prepare.go), so a damaged fixture is
// rejected instead of surfacing later as an unexplained keygen or proof failure.
func ValidateDevnetPreParams(p *ecKeyGen.LocalPreParams) error {
	if p == nil || !p.ValidateWithProof() {
		return errors.New("incomplete pre-params")
	}
	one, two := big.NewInt(1), big.NewInt(2)

	sk := p.PaillierSK
	if sk.N == nil || sk.PhiN == nil || sk.LambdaN == nil {
		return errors.New("incomplete Paillier key")
	}
	if new(big.Int).Mul(sk.P, sk.Q).Cmp(sk.N) != 0 {
		return errors.New("Paillier N != P*Q")
	}
	pm1, qm1 := new(big.Int).Sub(sk.P, one), new(big.Int).Sub(sk.Q, one)
	if new(big.Int).Mul(pm1, qm1).Cmp(sk.PhiN) != 0 {
		return errors.New("Paillier PhiN != (P-1)(Q-1)")
	}
	gcd := new(big.Int).GCD(nil, nil, pm1, qm1)
	if new(big.Int).Div(sk.PhiN, gcd).Cmp(sk.LambdaN) != 0 {
		return errors.New("Paillier LambdaN != lcm(P-1, Q-1)")
	}
	if !sk.P.ProbablyPrime(20) || !sk.Q.ProbablyPrime(20) {
		return errors.New("Paillier factor is not prime")
	}

	// NTildei = (2p+1)(2q+1) for Sophie Germain primes p, q.
	safeP := new(big.Int).Add(new(big.Int).Mul(two, p.P), one)
	safeQ := new(big.Int).Add(new(big.Int).Mul(two, p.Q), one)
	if new(big.Int).Mul(safeP, safeQ).Cmp(p.NTildei) != 0 {
		return errors.New("NTildei != (2p+1)(2q+1)")
	}
	for _, x := range []*big.Int{p.P, p.Q, safeP, safeQ} {
		if !x.ProbablyPrime(20) {
			return errors.New("safe-prime component is not prime")
		}
	}
	// Beta = Alpha^-1 mod p*q, H2 = H1^Alpha mod NTildei.
	pq := new(big.Int).Mul(p.P, p.Q)
	if new(big.Int).Mod(new(big.Int).Mul(p.Alpha, p.Beta), pq).Cmp(one) != 0 {
		return errors.New("Alpha*Beta != 1 mod p*q")
	}
	if p.H1i.Cmp(one) <= 0 || p.H1i.Cmp(p.NTildei) >= 0 {
		return errors.New("H1i out of range")
	}
	if new(big.Int).Exp(p.H1i, p.Alpha, p.NTildei).Cmp(p.H2i) != 0 {
		return errors.New("H2i != H1i^Alpha mod NTildei")
	}
	return nil
}
