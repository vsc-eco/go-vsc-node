package tss

import (
	"encoding/base64"
	"math/big"
	"sort"
	"testing"
	"vsc-node/lib/test_utils"
	"vsc-node/modules/common"
	"vsc-node/modules/db/vsc/elections"

	"github.com/stretchr/testify/require"
)

// makeElection creates a minimal ElectionResult with no weights, suitable for
// tests that only need member lookup (serialize, blame epoch, BLS collection).
func makeElection(epoch uint64, accounts []string) *elections.ElectionResult {
	members := make([]elections.ElectionMember, len(accounts))
	for i, a := range accounts {
		members[i] = elections.ElectionMember{Account: a}
	}
	return &elections.ElectionResult{
		ElectionCommonInfo: elections.ElectionCommonInfo{Epoch: epoch},
		ElectionDataInfo:   elections.ElectionDataInfo{Members: members},
	}
}

// decodeBlameBitset decodes a base64 blame bitset against an election member list,
// exactly as RunActions does at tss.go:707-712.
func decodeBlameBitset(encoded string, members []elections.ElectionMember) []string {
	blameBytes, _ := base64.RawURLEncoding.DecodeString(encoded)
	bits := new(big.Int).SetBytes(blameBytes)
	var names []string
	for idx, m := range members {
		if bits.Bit(idx) == 1 {
			names = append(names, m.Account)
		}
	}
	sort.Strings(names)
	return names
}

// makeElectionResult creates an ElectionResult with equal weights, suitable for
// tests that exercise blame scoring (which uses Weights).
func makeElectionResult(epoch uint64, accounts []string) elections.ElectionResult {
	members := make([]elections.ElectionMember, len(accounts))
	weights := make([]uint64, len(accounts))
	for i, a := range accounts {
		members[i] = elections.ElectionMember{Account: a, Key: "key-" + a}
		weights[i] = 10
	}
	return elections.ElectionResult{
		ElectionCommonInfo: elections.ElectionCommonInfo{Epoch: epoch},
		ElectionDataInfo:   elections.ElectionDataInfo{Members: members, Weights: weights},
	}
}

// newTestTssManager creates a minimal TssManager suitable for unit tests
// that exercise RPC handlers, BLS collection, and RunActions control flow.
func newTestTssManager(t *testing.T, myAccount string) *TssManager {
	t.Helper()
	path := t.TempDir()
	identity := common.NewIdentityConfig(path)
	err := identity.SetUsername(myAccount)
	require.NoError(t, err)

	mgr := &TssManager{
		config:         identity,
		actionMap:      make(map[string]Dispatcher),
		sessionMap:     make(map[string]sessionInfo),
		sessionResults: make(map[string]sessionResultEntry),
		sigChannels:    make(map[string]chan sigMsg),
		messageBuffer:  newSessionBuffer(),
		witnessDb:      &test_utils.MockWitnessDb{},
		metrics:        &Metrics{BlameCount: make(map[string]int64)},
	}
	mgr.lastBlockHeight.Store(1000) // reasonable current height
	return mgr
}

