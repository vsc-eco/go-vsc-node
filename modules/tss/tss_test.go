package tss_test

import (
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"
	"vsc-node/lib/test_utils"
	"vsc-node/modules/aggregate"
	"vsc-node/modules/common"
	"vsc-node/modules/db"
	"vsc-node/modules/db/vsc"
	"vsc-node/modules/db/vsc/elections"
	"vsc-node/modules/db/vsc/hive_blocks"
	tss_db "vsc-node/modules/db/vsc/tss"
	"vsc-node/modules/db/vsc/witnesses"
	libp2p "vsc-node/modules/p2p"
	stateEngine "vsc-node/modules/state-processing"
	vtss "vsc-node/modules/tss"
	tss_helpers "vsc-node/modules/tss/helpers"
	"vsc-node/modules/vstream"

	flatfs "github.com/ipfs/go-ds-flatfs"
	"github.com/libp2p/go-libp2p/core/peer"
	// "vsc-node/modules/tss"
)

type MockElectionSystem struct {
	ActiveWitnesses map[string]stateEngine.Witness
}

func (mes *MockElectionSystem) GetSchedule(blockHeight uint64) []stateEngine.WitnessSlot {
	witnesses := make([]stateEngine.Witness, 0)
	for key := range mes.ActiveWitnesses {
		witnesses = append(witnesses, stateEngine.Witness{
			Account: key,
		})
	}

	list := make([]stateEngine.WitnessSlot, 0)
	for x := 0; x < 5; x++ {
		modl := (int(blockHeight) + x) % len(witnesses)
		dl := (blockHeight % 10)
		dx := blockHeight - dl
		list = append(list, stateEngine.WitnessSlot{
			Account:    witnesses[modl].Account,
			SlotHeight: uint64(int(dx) + x*10),
		})
	}
	return list
}

func MakeNode(index int, mes *MockElectionSystem) (*aggregate.Aggregate, vstream.VStream, witnesses.Witnesses, *libp2p.P2PServer, elections.Elections) {
	path := "data-dir-" + strconv.Itoa(index)

	os.Mkdir(path, os.ModePerm)
	identity := common.NewIdentityConfig(path)
	identity.Init()
	identity.SetUsername("e2e-" + strconv.Itoa(index))
	dbConf := db.NewDbConfig()
	csonf := common.SystemConfig{
		Network: "mocknet",
	}

	db := db.New(dbConf)
	vscDb := vsc.New(db, "vsc-tss-test-"+strconv.Itoa(index))
	tssKeys := tss_db.NewKeys(vscDb)
	tssRequests := tss_db.NewRequests(vscDb)
	tssCommitments := tss_db.NewCommitments(vscDb)
	electionDb := elections.New(vscDb)
	witnesses := witnesses.New(vscDb)
	vstream := vstream.New(nil)

	p2p := libp2p.New(witnesses, identity, csonf, 22222+index)

	keystore, err := flatfs.CreateOrOpen(path+"/keys", flatfs.Prefix(1), false)

	if err != nil {
		panic(err)
	}
	tssMgr := vtss.New(p2p, tssKeys, tssRequests, tssCommitments, witnesses, electionDb, vstream, mes, identity, keystore)

	agg := aggregate.New([]aggregate.Plugin{
		identity,
		dbConf,
		db,
		vscDb,
		tssKeys,
		tssRequests,
		tssCommitments,
		electionDb,
		witnesses,

		p2p,

		tssMgr,
	})

	go func() {
		keyId := "test-key"
		tssMgr.KeyGen(keyId, tss_helpers.SigningAlgoSecp256k1)

		time.Sleep(2 * time.Minute)
		msg, _ := hex.DecodeString("89d7d1a68f8edd0cc1f961dce816422055d1ab69a0623954b834c95c1cdd7ed0")

		fmt.Println("msg hex is", hex.EncodeToString(msg))

		// tssMgr.KeySign(msg, keyId, tss_helpers.SigningAlgoEd25519)

		tssMgr.KeyReshare(keyId, tss_helpers.SigningAlgoSecp256k1)
	}()

	return agg, *vstream, witnesses, p2p, electionDb
}

func TestVtss(t *testing.T) {

	mes := &MockElectionSystem{
		ActiveWitnesses: map[string]stateEngine.Witness{
			"e2e-1": stateEngine.Witness{},
			"e2e-2": stateEngine.Witness{},
			"e2e-3": stateEngine.Witness{},
		},
	}

	vstrs := make([]vstream.VStream, 0)
	wts := make([]witnesses.Witnesses, 0)
	ets := make([]elections.Elections, 0)
	pts := make([]*libp2p.P2PServer, 0)
	for x := 0; x < 3; x++ {
		agg, vstr, witness, p2p, et := MakeNode(x, mes)
		vstrs = append(vstrs, vstr)
		wts = append(wts, witness)
		pts = append(pts, p2p)
		ets = append(ets, et)
		go test_utils.RunPlugin(t, agg)
	}

	time.Sleep(5 * time.Second)
	for _, w := range wts {
		for i, n := range pts {
			w.SetWitnessUpdate(witnesses.SetWitnessUpdateType{
				Account: "e2e-" + strconv.Itoa(i),
				Metadata: witnesses.PostingJsonMetadata{
					VscNode: witnesses.PostingJsonMetadataVscNode{
						PeerId: n.ID().String(),
					},
				},
			})
			// fmt.Println("err", err)
			for ix, nx := range pts {
				if ix != i {
					for _, addr := range nx.Addrs() {
						addr := addr.String() + "/p2p/" + nx.ID().String()

						addrInfo, _ := peer.AddrInfoFromString(addr)
						err := n.Connect(context.Background(), *addrInfo)
						if err == nil {
							break
						}
					}
				}
			}
		}
	}

	for _, e := range ets {
		e.StoreElection(elections.ElectionResult{
			BlockHeight: 1,
			ElectionDataInfo: elections.ElectionDataInfo{
				Members: []elections.ElectionMember{
					{
						Account: "e2e-0",
					},
					{
						Account: "e2e-1",
					},
					{
						Account: "e2e-2",
					},
				},
			},
		})
	}

	time.Sleep(5 * time.Second)
	go func() {
		bh := uint64(0)
		for {
			for _, vstr := range vstrs {
				vstr.ProcessBlock(hive_blocks.HiveBlock{
					Transactions: []hive_blocks.Tx{},
					BlockNumber:  bh,
				}, &bh)
			}
			time.Sleep(3 * time.Second)
			bh = bh + 1
		}
	}()

	select {}
}

// func TestP2p(t *testing.T) {
// 	fmt.Println("P2P Test started")
// 	dbConfig := db.NewDbConfig()
// 	db := db.New(dbConfig)
// 	vscDb := vsc.New(db, "vsc-tss")
// 	witnessDb := witnesses.New(vscDb)
// 	identityConfig := vcommon.NewIdentityConfig("data-tss" + "/config")

// 	identityConfig.Init()

// 	systemConfig := vcommon.SystemConfig{}

// 	p2pServer := libp2p.New(witnessDb, identityConfig, systemConfig, 10722)
// 	err := p2pServer.Init()
// 	if err != nil {
// 		t.Fatalf("Failed to initialize P2P server: %v", err)
// 	}

// 	fmt.Println("P2P Test completed")
// 	tssMgr := vtss.New(p2pServer)

// 	agg := []aggregate.Plugin{
// 		dbConfig,
// 		db,
// 		vscDb,
// 		witnessDb,
// 		identityConfig,
// 		p2pServer,
// 		tssMgr,
// 	}

// 	test_utils.RunPlugin(t, aggregate.New(agg))
// }
