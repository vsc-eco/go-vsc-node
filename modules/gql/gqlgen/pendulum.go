package gqlgen

import (
	"vsc-node/modules/db/vsc/pendulum_oracle"
	"vsc-node/modules/gql/model"
)

func pendulumSnapshotToGQL(rec pendulum_oracle.SnapshotRecord) PendulumOracleSnapshot {
	var reductions []PendulumWitnessRewardReduction
	if len(rec.WitnessRewardReductions) > 0 {
		reductions = make([]PendulumWitnessRewardReduction, 0, len(rec.WitnessRewardReductions))
		for _, r := range rec.WitnessRewardReductions {
			reductions = append(reductions, PendulumWitnessRewardReduction{
				Witness: r.Witness,
				Bps:     r.Bps,
				Evidence: &PendulumWitnessLivenessEvidence{
					BlockProductionBps:         r.Evidence.BlockProductionBps,
					BlockAttestationBps:        r.Evidence.BlockAttestationBps,
					TssReshareExclusionBps:     r.Evidence.TssReshareExclusionBps,
					TssBlameBps:                r.Evidence.TssBlameBps,
					TssSignNonParticipationBps: r.Evidence.TssSignNonParticipationBps,
				},
			})
		}
	}
	return PendulumOracleSnapshot{
		TickBlockHeight:         model.Uint64(rec.TickBlockHeight),
		TrustedHivePriceBps:     model.Int64(rec.TrustedHivePriceBps),
		TrustedHiveOk:           rec.TrustedHiveOK,
		HiveMovingAvgBps:        model.Int64(rec.HiveMovingAvgBps),
		HiveMovingAvgOk:         rec.HiveMovingAvgOK,
		HbdInterestRateBps:      rec.HBDInterestRateBps,
		HbdInterestRateOk:       rec.HBDInterestRateOK,
		TrustedWitnessGroup:     rec.TrustedWitnessGroup,
		WitnessRewardReductions: reductions,
		GeometryOk:              rec.GeometryOK,
		GeometryVHbd:            model.Int64(rec.GeometryV),
		GeometryPHbd:            model.Int64(rec.GeometryP),
		GeometryEHbd:            model.Int64(rec.GeometryE),
		GeometryTHive:           model.Int64(rec.GeometryT),
		GeometrySBps:            model.Int64(rec.GeometrySBps),
	}
}
