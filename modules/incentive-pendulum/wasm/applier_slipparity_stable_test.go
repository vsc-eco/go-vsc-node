package pendulumwasm

import (
	"math/big"
	"math/rand"
	"testing"

	"vsc-node/lib/intmath"
	"vsc-node/modules/common/consensusversion"
	pendulum "vsc-node/modules/incentive-pendulum"
)

// THORChain's swap fee on a StableSwap pool: the slip fee is measured on the
// pool's own curve (stableGross), so the rule charges the curve's real price
// impact, or the protocol tier when that is larger. It never charges the
// constant-product slip x/(x+X) a stable pool does not have.

func parityStableApplier() *Applier {
	cfg := DefaultConfig()
	cfg.SlipParityHeight = 1
	cfg.StablePoolAt = func(id string, _ uint64) (StableListing, bool) {
		if id == stablePoolID {
			return hbdUsdcListing, true
		}
		return StableListing{}, false
	}
	return New(
		&stubGeometry{out: balancedGeometry()},
		func(uint64) []string { return []string{stablePoolID, "vsc1Other"} },
		func(uint64) consensusversion.Version { return consensusversion.V0_13_0 },
		cfg,
	)
}

func stableRates(t *testing.T, in string) (rIn, rOut *big.Int) {
	t.Helper()
	decIn, decOut := uint8(3), uint8(6)
	if in == "usdc" {
		decIn, decOut = 6, 3
	}
	var err error
	if rIn, err = pendulum.StableRate(decIn); err != nil {
		t.Fatal(err)
	}
	if rOut, err = pendulum.StableRate(decOut); err != nil {
		t.Fatal(err)
	}
	return rIn, rOut
}

func TestSlipParity_StablePoolPaysItsCurveSlip(t *testing.T) {
	a := parityStableApplier()
	for _, in := range []string{"hbd", "usdc"} {
		// 10% of the reserve: x/(x+X) = 9.09% on a constant product.
		x := int64(10_000_000)
		if in == "usdc" {
			x = 10_000_000_000
		}
		args := stableArgs(in, x)
		st := mustOK(t, a, stablePoolID, args)
		if st.Curve != "stable" {
			t.Fatalf("%s in: curve %q", in, st.Curve)
		}
		rIn, rOut := stableRates(t, in)
		gross, curveSlip, err := stableGross(&stableTerms{amp: 100, rIn: rIn, rOut: rOut}, big.NewInt(x), big.NewInt(args.XReserve), big.NewInt(args.YReserve))
		if err != nil {
			t.Fatal(err)
		}
		baseProtocol := intmath.MulDivFloor(gross, big.NewInt(pendulum.ProtocolFeeRateBps), big.NewInt(pendulum.BpsScale))
		protocol := pendulum.ApplyMultiplierBps(baseProtocol, st.MultiplierBps)
		if protocol.Cmp(baseProtocol) < 0 {
			protocol = baseProtocol
		}
		want := curveSlip
		if protocol.Cmp(want) > 0 {
			want = protocol
		}
		fee := st.LpShareOutput + st.NodeShareOutput + st.NetworkCreditOutput
		if fee != want.Int64() {
			t.Fatalf("%s in: fee %d, want max(curve slip %s, protocol tier %s)", in, fee, curveSlip, protocol)
		}
		if gross.Int64()-st.UserOutput != fee {
			t.Fatalf("%s in: gross %s - user %d != fee %d", in, gross, st.UserOutput, fee)
		}
		// The curve's slip on a balanced A=100 pool is far below 1%.
		if fee*10_000 > gross.Int64()*30 {
			t.Fatalf("%s in: stable fee %d is over 0.3%% of gross %s", in, fee, gross)
		}
		// The same swap on a constant-product pool pays the full x/(x+X).
		cp := mustOK(t, a, "vsc1Other", cpmmOnly(args))
		cpGross, cpSlip, _ := baseFees(cpmmOnly(args))
		cpFee := cp.LpShareOutput + cp.NodeShareOutput + cp.NetworkCreditOutput
		if cpFee != cpSlip.Int64() || cpFee*10_000 < cpGross.Int64()*900 {
			t.Fatalf("%s in: constant-product fee %d, slip %s, gross %s", in, cpFee, cpSlip, cpGross)
		}
		t.Logf("%s in: stable fee %d (%.3f%%), constant product %d (%.2f%%)", in, fee,
			100*float64(fee)/float64(gross.Int64()), cpFee, 100*float64(cpFee)/float64(cpGross.Int64()))
	}
}

// Conservation and the pool contract's stable floor (stored reserves on or
// above the old level set) under THORChain's larger fee, on balanced and
// lopsided pools up to 30:1, both directions, swaps up to half the reserve.
func TestSlipParity_StablePoolConservesAndPassesTheContractFloor(t *testing.T) {
	a := parityStableApplier()
	rng := rand.New(rand.NewSource(11))
	checked := 0
	for i := 0; i < 1000; i++ {
		in := "hbd"
		if i%2 == 1 {
			in = "usdc"
		}
		args := stableArgs(in, 1)
		args.XReserve = args.XReserve/30 + rng.Int63n(args.XReserve*30)
		args.YReserve = args.YReserve/30 + rng.Int63n(args.YReserve*30)
		args.X = rng.Int63n(args.XReserve/2) + 1
		acc := &recordingAccrual{}
		res := a.ApplySwapFees(stablePoolID, "tx", 100, args, acc.fn)
		if res.IsErr() {
			continue // dust that rounds to nothing
		}
		out := res.Unwrap()
		var bucket int64
		for _, c := range acc.calls {
			bucket += c
		}
		if bucket != out.NodeBucketCreditedHBD {
			t.Fatalf("case %d: bucket %d vs reported %d", i, bucket, out.NodeBucketCreditedHBD)
		}
		if in == "hbd" { // USDC out: node share sold back for HBD
			if args.YReserve-out.NewYReserve != out.UserOutput {
				t.Fatalf("case %d usdc out: reserve drop %d != user %d", i, args.YReserve-out.NewYReserve, out.UserOutput)
			}
			if args.XReserve+args.X-out.NewXReserve != out.NodeBucketCreditedHBD {
				t.Fatalf("case %d usdc out: hbd drop %d != bucket %d", i, args.XReserve+args.X-out.NewXReserve, out.NodeBucketCreditedHBD)
			}
			if out.NewXReserve < args.XReserve {
				t.Fatalf("case %d usdc out: input reserve fell %d -> %d (pool contract refuses)", i, args.XReserve, out.NewXReserve)
			}
		} else { // HBD out: node share leaves Y as HBD
			if args.YReserve-out.NewYReserve != out.UserOutput+out.NodeShareOutput {
				t.Fatalf("case %d hbd out: reserve drop %d != user %d + node %d", i, args.YReserve-out.NewYReserve, out.UserOutput, out.NodeShareOutput)
			}
			if out.NewXReserve != args.XReserve+args.X {
				t.Fatalf("case %d hbd out: input reserve %d != %d", i, out.NewXReserve, args.XReserve+args.X)
			}
		}
		if out.UserOutput <= 0 || out.LpShareOutput < 0 || out.NodeShareOutput < 0 || out.NetworkCreditOutput < 0 {
			t.Fatalf("case %d: negative leg or no output: %+v", i, out)
		}
		rIn, rOut := stableRates(t, in)
		storedX := big.NewInt(out.NewXReserve)
		storedY := big.NewInt(out.NewYReserve - out.NetworkCreditOutput)
		floor, err := pendulum.StableFloorY(storedX, big.NewInt(args.XReserve), big.NewInt(args.YReserve), rIn, rOut, 100)
		if err != nil {
			t.Fatal(err)
		}
		if new(big.Int).Mul(storedY, rOut).Cmp(floor) < 0 {
			t.Fatalf("case %d (%s in, X=%d Y=%d x=%d): stored %s·%s below floor %s", i, in, args.XReserve, args.YReserve, args.X, storedY, rOut, floor)
		}
		checked++
	}
	if checked < 900 {
		t.Fatalf("only %d of 1000 swaps priced", checked)
	}
}
