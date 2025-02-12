package elections_test

import (
	"testing"
	"vsc-node/modules/db/vsc/elections"

	"github.com/ipfs/go-cid"
	"github.com/stretchr/testify/assert"
)

func TestElectionHeaderCid(t *testing.T) {
	var validCid = cid.MustParse("bafyreiazhbfk5agojh2chgxai23eydg6eee4djflohc5r4pkyqxmo543ym")
	var electionDataCid = cid.MustParse("bafyreifiekarc6umywsvbug7z27wqa54edmymoks3okkptowjznhbc65jm")
	var validElectionHeader = elections.ElectionHeader{}
	validElectionHeader.Epoch = 125
	validElectionHeader.NetId = "testnet/0bf2e474-6b9e-4165-ad4e-a0d78968d20c"
	validElectionHeader.Data = electionDataCid.String()
	cid, err := validElectionHeader.Cid()
	assert.NoError(t, err)
	assert.Equal(t, validCid.String(), cid.String())
}
