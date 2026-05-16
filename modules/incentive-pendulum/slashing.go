package pendulum

// SlashParams controls the basis-point slashing schedule for oracle participation.
type SlashParams struct {
	// MinBlocksProduced is the L1 block-production threshold per window
	// below which a witness starts accumulating block-production-deficit
	// slashing.
	MinBlocksProduced int
	// MissingBlockStepBps is the per-block-shortfall slashing amount in
	// basis points. Total deficit slashing == (MinBlocksProduced -
	// blocksProduced) * MissingBlockStepBps, floored at 0.
	MissingBlockStepBps int
	// MissingUpdateBps is the flat penalty for failing to refresh a
	// feed_publish op inside the trust window.
	MissingUpdateBps int
	// EquivocationBps is the flat penalty for double-signing or otherwise
	// publishing conflicting price updates inside the same window.
	EquivocationBps int
	// CapBps is the maximum total slashing in basis points per window.
	CapBps int
}

// DefaultSlashParams mirrors the Lean model in MagiLean.Pendulum.Slashing.
func DefaultSlashParams() SlashParams {
	return SlashParams{
		MinBlocksProduced:   4,
		MissingBlockStepBps: 25,
		MissingUpdateBps:    50,
		EquivocationBps:     500,
		CapBps:              1000,
	}
}

// OracleEvidence is per-window participation evidence for one witness.
type OracleEvidence struct {
	// BlocksProduced is the count of L1 (Hive) blocks this witness produced
	// inside the rolling production window.
	BlocksProduced int
	// UpdatedFeed is true iff the witness published or refreshed a price
	// update in the trust window.
	UpdatedFeed bool
	// Equivocated is true iff conflicting price updates from this witness
	// were observed in the same window.
	Equivocated bool
}

// BlockProductionDeficit returns max(0, MinBlocksProduced - BlocksProduced).
func BlockProductionDeficit(p SlashParams, e OracleEvidence) int {
	deficit := p.MinBlocksProduced - e.BlocksProduced
	if deficit < 0 {
		return 0
	}
	return deficit
}

// SlashBpsRaw computes uncapped slashing in basis points.
func SlashBpsRaw(p SlashParams, e OracleEvidence) int {
	raw := BlockProductionDeficit(p, e) * p.MissingBlockStepBps
	if !e.UpdatedFeed {
		raw += p.MissingUpdateBps
	}
	if e.Equivocated {
		raw += p.EquivocationBps
	}
	if raw < 0 {
		return 0
	}
	return raw
}

// SlashBps computes capped slashing in basis points.
func SlashBps(p SlashParams, e OracleEvidence) int {
	raw := SlashBpsRaw(p, e)
	if p.CapBps > 0 && raw > p.CapBps {
		return p.CapBps
	}
	return raw
}
