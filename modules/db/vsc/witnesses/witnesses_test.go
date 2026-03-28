package witnesses_test

import (
	"encoding/json"
	"fmt"
	"testing"
	"vsc-node/lib/test_utils"
	"vsc-node/modules/aggregate"
	"vsc-node/modules/db"
	"vsc-node/modules/db/vsc"
	"vsc-node/modules/db/vsc/witnesses"

	"github.com/stretchr/testify/assert"
)

func TestWitness(t *testing.T) {
	conf := db.NewDbConfig()
	db := db.New(conf)
	vscDb := vsc.New(db, conf)
	witness := witnesses.New(vscDb)

	agg := aggregate.New([]aggregate.Plugin{
		conf,
		db,
		vscDb,
		witness,
	})

	test_utils.RunPlugin(t, agg)

	data := `{"Metadata":{"vsc_node":{"did":"did:key:z6MkgzrGBrLbEicPUbM4VQd9VjPThcDUVm5piZeeshUQjDC3","unsigned_proof":{"net_id":"testnet/0bf2e474-6b9e-4165-ad4e-a0d78968d20c","ipfs_peer_id":"12D3KooWDAXynd1VZDjAeEQDRp9wVsyhQjUDPG2szeKHuGBYmEAD","ts":"2024-01-04T13:32:42.522Z","git_commit":"1183c1e9864cdea96803b56caa17ee4c87dbc238\n","version_id":"","witness":{"enabled":true,"plugins":["multisig"],"delay_notch":20,"signing_keys":null}}},"did_keys":[{"ct":"DID-BLS","t":"consensus","key":"did:key:z3tEGdy8ZANPehocH7Wiv1FhUtzSQSyJ6BBaSiFcstoJhg5uYq4cAMPiNkgTP3EsnXY5HT"}]},"Account":"manu-node","Height":81622833,"TxId":"f735b166f3225016e45f8e02e710a22170f5f152","BlockId":"04dd773198764acba48258466dc8bf947e7604a4"}`

	info := witnesses.SetWitnessUpdateType{}

	json.Unmarshal([]byte(data), &info)

	assert.NoError(t, witness.SetWitnessUpdate(info))

	testData := map[string]interface{}{
		"Hello": []byte("hello world"),
	}

	bbytes, err := json.Marshal(testData)
	fmt.Println("Serialized", string(bbytes), err)
	mapps := struct {
		Hello []byte
	}{}

	json.Unmarshal(bbytes, &mapps)
	fmt.Println(mapps)
}
