package state_engine_test

import (
	"testing"

	consensusversion "vsc-node/modules/common/consensusversion"

	"github.com/stretchr/testify/assert"
)

// SELF-INJECTION-1: the trusted-Self rebuild is enforced from 0.14.0 exactly.
// The decode itself is covered through ProcessBlock in selfinject_ingest_test.go.
func TestSelfInjectGateVersions(t *testing.T) {
	v := func(c uint64) consensusversion.Version {
		return consensusversion.Version{Major: 0, Consensus: c, NonConsensus: 0}
	}
	assert.False(t, consensusversion.SelfInjectionTrustedSelfActive(v(12)), "0.12 must not enforce")
	assert.False(t, consensusversion.SelfInjectionTrustedSelfActive(v(13)), "0.13 must not enforce (one version early)")
	assert.True(t, consensusversion.SelfInjectionTrustedSelfActive(v(14)), "0.14 must enforce")
	assert.True(t, consensusversion.SelfInjectionTrustedSelfActive(v(15)), "above 0.14 must enforce")
}
