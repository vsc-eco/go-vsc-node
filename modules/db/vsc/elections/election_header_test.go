package elections_test

import (
	"testing"
	"vsc-node/modules/db/vsc/elections"

	"github.com/ipfs/go-cid"
	"github.com/stretchr/testify/assert"
)

func TestElectionHeaderCid(t *testing.T) {
	var validCid = cid.MustParse("bafyreihr6tp3rjdumb3esejyml2p7xlsqfrv3sulbo3rlculxyshursqci")
	var electionDataCid = cid.MustParse("bafyreiflgo3ce4djxsiozskga4o7yjcjedifunx63kmz4h4emxolm54cvm")
	var validElectionHeader = elections.ElectionHeader{}
	validElectionHeader.Epoch = 125
	validElectionHeader.NetId = "testnet/0bf2e474-6b9e-4165-ad4e-a0d78968d20c"
	validElectionHeader.Data = electionDataCid.String()
	cid, err := validElectionHeader.Cid()
	assert.NoError(t, err)
	assert.Equal(t, validCid.String(), cid.String())
}
