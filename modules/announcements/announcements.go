package announcements

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log"
	"vsc-node/lib/dids"
	agg "vsc-node/modules/aggregate"
	"vsc-node/modules/config"

	"github.com/mattrltrent/hivego"
	"github.com/robfig/cron/v3"
	blst "github.com/supranational/blst/bindings/go"

	"github.com/chebyrash/promise"
)

// ===== types =====

type announcementsManager struct {
	conf *config.Config[announcementsConfig]
	cron *cron.Cron
	stop chan struct{}
}

// ===== intetface assertions =====

var _ agg.Plugin = &announcementsManager{}

// ===== constructor =====

func New(conf *config.Config[announcementsConfig]) *announcementsManager {
	return &announcementsManager{
		cron: cron.New(),
		conf: conf,
		stop: make(chan struct{}),
	}
}

// ===== implementing plugin interface =====

func (a *announcementsManager) Init() error {
	return nil
}

func (a *announcementsManager) Start() *promise.Promise[any] {
	return promise.New(func(resolve func(any), reject func(error)) {
		// create a ctx that cancels when the stop chan is closed
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			<-a.stop
			cancel()
		}()

		// run task immediately
		go a.announce(ctx)

		// schedule the task to run every 24 hours starting from now
		_, err := a.cron.AddFunc("@every 24h", func() {
			// check if stop signal has been received before running the task
			select {
			case <-a.stop:
				return
			default:
				go a.announce(ctx)
			}
		})
		if err != nil {
			reject(err)
			return
		}
		a.cron.Start()
		resolve(nil)
	})
}

func (a *announcementsManager) Stop() error {
	// safely close the stop channel
	select {
	case <-a.stop:
		// do nothing, already stopped
	default:
		close(a.stop)
	}
	// stop cron scheduler
	a.cron.Stop()
	return nil
}

// ===== announcement impl =====

type payload struct {
	did_keys []didConsensusKey `json:"did_keys"`
	vsc_node vscNode           `json:"vsc_node"`
}

type didConsensusKey struct {
	t   string      `json:"t"`
	ct  string      `json:"ct"`
	key dids.BlsDID `json:"key"`
}

type signature struct {
	protected string `json:"protected"`
	signature string `json:"signature"`
}

type signedProof struct {
	payload    string      `json:"payload"`
	signatures []signature `json:"signatures"`
}

type unsignedProof struct{} // todo

type vscNode struct {
	Did           dids.DID[ed25519.PublicKey] `json:"did"`
	signedProof   signedProof                 `json:"signed_proof"`
	unsignedProof unsignedProof               `json:"unsigned_proof"`
}

type accountUpdateOperation struct {
	// key fields
	Account      string `json:"account"`
	JSONMetadata string `json:"json_metadata"`
	MemoKey      string `json:"memo_key"`
	// metadata fields
	// Owner        string   `json:"owner,omitempty"`
	// Active       string   `json:"active,omitempty"`
	// Posting      string   `json:"posting,omitempty"`
	Extensions []string `json:"extensions"`
	opText     string
}

func (o accountUpdateOperation) OpName() string {
	return "account_update"
}

func appendVString(s string, b *bytes.Buffer) *bytes.Buffer {
	vBuf := make([]byte, 5)
	vLen := binary.PutUvarint(vBuf, uint64(len(s)))
	b.Write(vBuf[0:vLen])

	b.WriteString(s)
	return b
}

func appendVStringArray(a []string, b *bytes.Buffer) *bytes.Buffer {
	b.Write([]byte{byte(len(a))})
	for _, s := range a {
		appendVString(s, b)
	}
	return b
}

func (o accountUpdateOperation) SerializeOp() ([]byte, error) {
	var buf bytes.Buffer
	buf.Write([]byte{byte(10)})
	appendVString(o.Account, &buf)
	appendVString(o.JSONMetadata, &buf)
	appendVString(o.MemoKey, &buf)
	// appendVString(o.Owner, &buf)
	// appendVString(o.Active, &buf)
	// appendVString(o.Posting, &buf)
	// appendVStringArray(o.Extensions, &buf)
	buf.WriteByte(0) // Extensions (empty)
	appendVString(o.opText, &buf)

	fmt.Println("finished SerializeOp", buf)
	return buf.Bytes(), nil
}

func (a *announcementsManager) updateAccount(hrpc *hivego.HiveRpcNode) (string, error) {

	blsPrivKey := a.conf.Get().BlsPrivKey
	fmt.Println("BLS PRIV KEY", blsPrivKey)
	blsDid, err := dids.NewBlsDID(new(blst.P1Affine).From(&blsPrivKey))
	if err != nil {
		return "", err
	}

	keyPubKey := a.conf.Get().ConsensusKey.Public().(ed25519.PublicKey)
	keyDid, err := dids.NewKeyDID(keyPubKey)
	if err != nil {
		return "", err
	}

	fmt.Println("GOT HERE!!!!!!!!!!!!!!!!!!")

	// Example setup for payload struct
	unsignedProof := unsignedProof{} // Populate as needed
	signedProof := signedProof{
		payload: "example_payload",
		signatures: []signature{
			{
				protected: "example_protected",
				signature: "example_signature",
			},
		},
	}

	vscNode := vscNode{
		Did:           keyDid,
		unsignedProof: unsignedProof,
		signedProof:   signedProof,
	}

	payload := payload{
		did_keys: []didConsensusKey{
			{
				t:   "consensus",
				ct:  "DID-BLS",
				key: blsDid,
			},
		},
		vsc_node: vscNode,
	}

	// Serialize payload into JSON
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		log.Fatalf("Failed to serialize payload: %v", err)
	}

	// Prepare account update operation
	op := accountUpdateOperation{
		Account:      "mattrltrent",
		JSONMetadata: string(payloadJSON),
		MemoKey:      "STM7DGfyBPBRuq4f426M7jgLsbAPDqf82R6wdahWcQFUgSi3DJHiT",
		Extensions:   []string{},
		opText:       "account_update",
	}

	// todo: WIF private key
	wif := ""

	// Broadcast the operation
	return hrpc.Broadcast([]hivego.HiveOperation{op}, &wif)
}

func (a *announcementsManager) announce(ctx context.Context) {
	log.Println("announcing")
	hrpc := hivego.NewHiveRpc("https://api.hive.blog")
	log.Println(a.updateAccount(hrpc))
}
