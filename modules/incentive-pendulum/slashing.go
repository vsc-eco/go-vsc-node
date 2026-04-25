package pendulum

// SlashParams controls the basis-point slashing schedule for oracle participation.
type SlashParams struct {
	MinSignatures     int
	MissingSigStepBps int
	MissingUpdateBps  int
	EquivocationBps   int
	CapBps            int
}

// DefaultSlashParams mirrors the Lean model in MagiLean.Pendulum.Slashing.
func DefaultSlashParams() SlashParams {
	return SlashParams{
		MinSignatures:     4,
		MissingSigStepBps: 25,
		MissingUpdateBps:  50,
		EquivocationBps:   500,
		CapBps:            1000,
	}
}

// OracleEvidence is per-window participation evidence for one witness.
type OracleEvidence struct {
	Signatures  int
	UpdatedFeed bool
	Equivocated bool
}

// SignatureDeficit returns max(0, minSignatures-signatures).
func SignatureDeficit(p SlashParams, e OracleEvidence) int {
	deficit := p.MinSignatures - e.Signatures
	if deficit < 0 {
		return 0
	}
	return deficit
}

// SlashBpsRaw computes uncapped slashing in basis points.
func SlashBpsRaw(p SlashParams, e OracleEvidence) int {
	raw := SignatureDeficit(p, e) * p.MissingSigStepBps
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
