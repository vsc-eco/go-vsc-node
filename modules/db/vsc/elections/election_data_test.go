package elections_test

import (
	"encoding/json"
	"testing"
	"vsc-node/modules/db/vsc/elections"
	settlement "vsc-node/modules/incentive-pendulum/settlement"

	"github.com/ipfs/go-cid"
	cbornode "github.com/ipfs/go-ipld-cbor"
	"github.com/multiformats/go-multihash"
	"github.com/stretchr/testify/assert"
)

func TestElectionDataCid(t *testing.T) {
	// Pinned CID for the canonical-form ElectionData. The per-member and
	// election-level consensus-version fields (refmt-encoded, no omitempty) are
	// always present in the body, so this pin changed from the pre-versioning
	// format. The Settlement pointer is still OMITTED when nil — the non-nil
	// sanity check below guards that it is carried when set.
	var validCid = cid.MustParse("bafyreiflgo3ce4djxsiozskga4o7yjcjedifunx63kmz4h4emxolm54cvm")
	var validElectionData = elections.ElectionData{}
	validElectionData.Epoch = 125
	validElectionData.NetId = "testnet/0bf2e474-6b9e-4165-ad4e-a0d78968d20c"
	validElectionData.ProtocolVersion = 0
	validElectionData.Weights = []uint64{
		6,
		10,
		13,
		13,
		13,
		6,
		13,
		6,
		13,
		13,
		17,
		17,
		1,
		13,
		1,
		6,
		13,
		13,
		13,
		6,
		17,
		13,
		6,
		13,
		13,
		13,
		6,
		6,
		13,
		10,
		17,
		17,
		17,
		17,
		6,
	}
	validElectionData.Members = []elections.ElectionMember{
		{
			Key: "did:key:z3tEF5NxSjPf93zbwQCxVW4x6g6kZ9ttbqcVJDL8wzjtBLJ1eTy6tYXS9WVDYNseisVNTp",
			Account: "actifit.vsc",
		},
		{
			Key: "did:key:z3tEFJrgSYG8ypEDaGDrZY1yooSqmQvgPMdH9PQ8kiMYhYuMzRMDUAUkSKLBnDyWMuvoqm",
			Account: "annadang",
		},
		{
			Key: "did:key:z3tEG5Jj5rFQG1pBJTxXHyhMmnRNq2vWU7G2DJ3gkL4NxVkFcUhPPpCGD6mXvchQm2u7pc",
			Account: "asgarth.vsc",
		},
		{
			Key: "did:key:z3tEGiumPhsgaGjq997DbadLP8YhbNVWzvzaiKXajqqaZ9KRs1o4xmfHX9SEAZiLixPx1y",
			Account: "avalonreport",
		},
		{
			Key: "did:key:z3tEGGk86KRWQv3s6KV4iMo9ew1zgKG3CvzjLGevrwjpbbbD87HpxXNGH9U9pw5TBC6ePv",
			Account: "bala.vsc",
		},
		{
			Key: "did:key:z3tEGRuCuGyxnbEyjAiJKYqt4pdGtsYiikJ7WmH8qECWCGpKXrFAFykhRqKN3w3HzsnHnx",
			Account: "bradleyarrow.vsc",
		},
		{
			Key: "did:key:z3tEFzX6HusCWzwjdf3LM1uAny8KX6KuereChqF8eoEkENvKX6H96DBe45avev6XrQQMMs",
			Account: "condeas.vsc",
		},
		{
			Key: "did:key:z3tEFngLxehHCQspkeSVoHfWA8v3pxJUtXR2SZRTkrxU3eKfyerJrCJbV3LhqDzwjagxto",
			Account: "cuongphamq",
		},
		{
			Key: "did:key:z3tEFz3LYFSg6XhjTxXvPnrtQyEtAfsx3fD6pUB1KSs434XsLeNTFkTVyPUYwz4MvpbRFf",
			Account: "danggia",
		},
		{
			Key: "did:key:z3tEGcSWJNbUw6GxijVdRG8nQtB32BRGbGSkEusGEFKfmPPDz2bxR4ktyyfV8Pn1WeCMCj",
			Account: "dragonz47",
		},
		{
			Key: "did:key:z3tEEhfRU4aAYyb2wW2Qi5bDQX357isq1agUsdGtEdv6zT2DHofrCMNRQaTr9KaxUW4JLP",
			Account: "geo52rey.dev",
		},
		{
			Key: "did:key:z3tEFLNacMc9WHFHCw7kyC81SBdeHavfbdBkQYNzr2UACWudvEZPF3F8qZcrKxF6NXxjhM",
			Account: "geo52rey.vsc",
		},
		{
			Key: "did:key:z3tEFCVn45mZ3p8dDXFs3MSvbD61kHeAu9FEYbXLh3wYsaQVQfBd3waybdU8g5cjrpdgtV",
			Account: "geo52rey.vsc2",
		},
		{
			Key: "did:key:z3tEGiyG5fc4FXEuxdWu7YE2e9hKQJZrVmnfsa5fXcD7RnXoKJ4AMpyd2y7rxiLgnkibeT",
			Account: "good-karma.vsc",
		},
		{
			Key: "did:key:z3tEG7gEcDk66xrxf5F6mxyWrBvVa7joUCv1AG7Gr76rNWupWHvnywwuoykrkL3ax3vEvt",
			Account: "hotelcalifornia",
		},
		{
			Key: "did:key:z3tEGfHXLLjLecrJPBhBW1chg6WMWzRaaKK11dYSMnWvjP55CFFcTmUZ2qKHJZqE4xcudJ",
			Account: "i-am-the-flash",
		},
		{
			Key: "did:key:z3tEGVGccHDMMVC6TZnFMXEQQRskLcCzRWFkBzVVQZjFf3w4DAzXKpfQkWKpVC2Ffed88d",
			Account: "jeffbuilds.vsc",
		},
		{
			Key: "did:key:z3tEFJMGRpMsmQWB7GkxV2zYQ4YRft8jsiUuvqRvy5PG4PwxDmnqpLHC9bPJb9TxoeYXpL",
			Account: "kenz47",
		},
		{
			Key: "did:key:z3tEEgRcn2b8v5S5i5ABGcvRRA9zJy94S64K28LSrR4pkn6DVVd9MtnaBZpu9wE323nULo",
			Account: "kill.allnode",
		},
		{
			Key: "did:key:z3tEGKWdYE7AwmDjT9Nuq9pJUMvZ3FXToHQ1swJv3xNgPPpisBLXHRfyymoDVRB3AvpJPu",
			Account: "ladytime",
		},
		{
			Key: "did:key:z3tEEbuU1ttovNKtug4VYFaW9ZJW4Dt9RqVEUFNsHa2TofFYy9tVu9eKkZPsXKRDN6Jeru",
			Account: "lassecashwitness",
		},
		{
			Key: "did:key:z3tEEktbbmF59iFWRU534Pd56uMUztB7aTHKL4WFQsgqhsvhqYS4qAVGxj1dMCJydXDd6f",
			Account: "mahdiyari.vsc",
		},
		{
			Key: "did:key:z3tEFEL2wsJMFU9djvzF4dJrE1j42vmj1xhJzu86nXeRyPha2hs1guK6CBxg6jJJVnxdtN",
			Account: "noel18082018",
		},
		{
			Key: "did:key:z3tEGfLZHrot4T4pjGf1sR1VQ3NDvLrfvoANh1CN41i9Qqembs1RkBBtm23kffzVijLaEh",
			Account: "p2pnodetop",
		},
		{
			Key: "did:key:z3tEGRQ25oAHpsE2qKMxymybE6gQtWTJ9pogVfCMKo84SMKQKFb9rw1qCebE6kgTLZ1ubc",
			Account: "podping.vsc",
		},
		{
			Key: "did:key:z3tEFrkdDcfB2ekzDCpFT7pAp95ooWzLCx1XeMLT272tcCQwH9mCr2g61ysPAjqL2sYUpL",
			Account: "possibly.vsc",
		},
		{
			Key: "did:key:z3tEFVF1BeYPp5PU7erY4Bi1kHQFJVpKqrwJ83qSAgzTTStNV8SFT1QmmBxpVyPoKKRX3q",
			Account: "rhemagames",
		},
		{
			Key: "did:key:z3tEFXunHAWZvxGRyQbXuCutZ3xebRgVZML7N7t1kMzyppEjBwpyrFUSn8bNE3gmc1y7Qp",
			Account: "skiptvads.vsc2",
		},
		{
			Key: "did:key:z3tEGYbX4TiY7rmQa6fPNH6bxXXBfc1Kz1eQytrUVMkjWEXbofnFyG5GMwJWQWP9J3E8C4",
			Account: "stonemac65",
		},
		{
			Key: "did:key:z3tEGBeBSiykgD3H23S98eMDzsn2PxEiv5t8KUGVrL4jAq8P4BHu9HwTnGsBCqUCmgNpe8",
			Account: "sudokurious",
		},
		{
			Key: "did:key:z3tEFDjwc8cdS3fXqPPZs2r5Pe5NrzUzt13SGbFW7G5pSLbu5ZboqLfNCEBF9eYjvGpkQJ",
			Account: "v4vapp.vsc",
		},
		{
			Key: "did:key:z3tEGFbLo8toPnWJgLaAto6McP9S3XUbfZEavZsgfLsZnbC5nK7zRETJSwMjFJZVmNyjC3",
			Account: "vaultec-scc",
		},
		{
			Key: "did:key:z3tEFUsQiNndov8ysxyzXEZggBe7PyBdP1DswJ2nuJcCFtxpgE9NfZ2ek61SJEszE2Nvmw",
			Account: "vsc.node1",
		},
		{
			Key: "did:key:z3tEG4yj2v1vMpooaa686DNE3BcPyQumfze4v9poY4ucx2kYLGJuPYtyiWQjhayk4oU761",
			Account: "vsc.node2",
		},
		{
			Key: "did:key:z3tEGS5crqb9h1XErUmeqR51AmrEGbF4yMUgegXxUiyPg7ZuvyPEvw2SS5wuHfdFv9nyFP",
			Account: "xautraikhoaibe",
		},
	}
	cid, err := validElectionData.Cid()
	assert.NoError(t, err)
	assert.Equal(t, validCid.String(), cid.String())

	// Sanity: a non-nil Settlement must change the CID — otherwise the field
	// is silently being dropped during encoding and on-chain settlement would
	// never be carried by the election body. Pair with TestElectionDataCid
	// above: nil → unchanged CID, non-nil → different CID.
	validElectionData.Settlement = &settlement.SettlementRecord{
		Epoch:               124,
		PrevEpoch:           123,
		SnapshotRangeFrom:   1000,
		SnapshotRangeTo:     2000,
		BucketBalanceHBD:    0,
		TotalDistributedHBD: 0,
		ResidualHBD:         0,
		RewardReductions:    []settlement.RewardReductionEntry{},
		Distributions:       []settlement.DistributionEntry{},
	}
	cidWithSettlement, err := validElectionData.Cid()
	assert.NoError(t, err)
	assert.NotEqual(t, validCid.String(), cidWithSettlement.String(),
		"Settlement field must be included in CID; if equal, refmt is dropping the non-nil pointer")
}

// TestElectionDataSettlementRoundTrip pins the encode → decode contract that
// TxElectionResult.ExecuteTx relies on at apply time: dag-CBOR encode the
// ElectionData body, then dag-CBOR decode + json.Unmarshal it back into an
// ElectionResult, and assert the embedded Settlement round-trips bit-for-bit.
// If this breaks, the state engine cannot read settlements out of election
// bodies on chain and the whole inline-settlement design is dead.
func TestElectionDataSettlementRoundTrip(t *testing.T) {
	original := elections.ElectionData{}
	original.Epoch = 42
	original.NetId = "test-net-id"
	original.Type = "staked"
	original.ProtocolVersion = 1
	original.Members = []elections.ElectionMember{
		{Key: "did:key:abc", Account: "alice"},
		{Key: "did:key:def", Account: "bob"},
	}
	original.Weights = []uint64{100, 200}
	original.Settlement = &settlement.SettlementRecord{
		Epoch:               41,
		PrevEpoch:           40,
		SnapshotRangeFrom:   1000,
		SnapshotRangeTo:     2000,
		BucketBalanceHBD:    5000,
		TotalDistributedHBD: 4500,
		ResidualHBD:         500,
		RewardReductions: []settlement.RewardReductionEntry{
			{Account: "alice", Bps: 100},
			{Account: "bob", Bps: 50},
		},
		Distributions: []settlement.DistributionEntry{
			{Account: "alice", HBDAmt: 3000},
			{Account: "bob", HBDAmt: 1500},
		},
	}

	// Encode via the same Node() path the election proposer uses.
	cborNode, err := original.Node()
	assert.NoError(t, err)

	// Decode via the same dagCbor + json.Unmarshal path
	// TxElectionResult.ExecuteTx uses to recover the body on chain.
	dagNode, err := cbornode.Decode(cborNode.RawData(), multihash.SHA2_256, -1)
	assert.NoError(t, err)
	bbytes, err := dagNode.MarshalJSON()
	assert.NoError(t, err)

	var recovered elections.ElectionResult
	err = json.Unmarshal(bbytes, &recovered)
	assert.NoError(t, err)

	assert.NotNil(t, recovered.Settlement, "Settlement field lost during round-trip")
	if recovered.Settlement != nil {
		assert.Equal(t, original.Settlement.Epoch, recovered.Settlement.Epoch)
		assert.Equal(t, original.Settlement.PrevEpoch, recovered.Settlement.PrevEpoch)
		assert.Equal(t, original.Settlement.BucketBalanceHBD, recovered.Settlement.BucketBalanceHBD)
		assert.Equal(t, original.Settlement.TotalDistributedHBD, recovered.Settlement.TotalDistributedHBD)
		assert.Equal(t, original.Settlement.ResidualHBD, recovered.Settlement.ResidualHBD)
		assert.Equal(t, len(original.Settlement.Distributions), len(recovered.Settlement.Distributions))
		assert.Equal(t, len(original.Settlement.RewardReductions), len(recovered.Settlement.RewardReductions))
		if len(recovered.Settlement.Distributions) >= 2 {
			assert.Equal(t, "alice", recovered.Settlement.Distributions[0].Account)
			assert.Equal(t, int64(3000), recovered.Settlement.Distributions[0].HBDAmt)
			assert.Equal(t, "bob", recovered.Settlement.Distributions[1].Account)
			assert.Equal(t, int64(1500), recovered.Settlement.Distributions[1].HBDAmt)
		}
	}
}
