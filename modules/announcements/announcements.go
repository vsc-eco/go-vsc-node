package announcements

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
	"vsc-node/lib/dids"
	"vsc-node/lib/hive"
	"vsc-node/lib/vsclog"
	agg "vsc-node/modules/aggregate"
	"vsc-node/modules/common"
	"vsc-node/modules/common/common_types"
	"vsc-node/modules/common/consensusversion"
	systemconfig "vsc-node/modules/common/system-config"
	p2p "vsc-node/modules/p2p"

	"github.com/libp2p/go-libp2p/core/network"
	"github.com/minio/sha256-simd"
	"github.com/multiformats/go-multiaddr"
	"github.com/robfig/cron/v3"
	"github.com/vsc-eco/hivego"

	"github.com/chebyrash/promise"
	ethBls "github.com/protolambda/bls12-381-util"
)

var alog = vsclog.Module("announcements")

// ===== types =====

type AnnouncementsManager struct {
	conf         common.IdentityConfig
	sconf        systemconfig.SystemConfig
	p2pconf      p2p.P2PConfig
	cron         *cron.Cron
	ctx          context.Context
	cancel       context.CancelFunc
	client       HiveRpcClient
	hiveCreator  hive.HiveTransactionCreator
	cronDuration time.Duration
	peerInfo     common_types.PeerInfoGetter
}

type HiveRpcClient interface {
	GetAccount(accountNames []string) ([]hivego.AccountData, error)
	UpdateAccount(
		account string,
		owner *hivego.Auths,
		active *hivego.Auths,
		posting *hivego.Auths,
		jsonMetadata string,
		memoKey string,
		wif *string,
	) (string, error)
}

// ===== interface assertions =====

var _ agg.Plugin = &AnnouncementsManager{}

// ===== constructor =====

func New(
	client HiveRpcClient,
	conf common.IdentityConfig,
	sconf systemconfig.SystemConfig,
	p2pconf p2p.P2PConfig,
	cronDuration time.Duration,
	creator hive.HiveTransactionCreator,
	peerInfo common_types.PeerInfoGetter,
) (*AnnouncementsManager, error) {

	// sanity checks

	if client == nil {
		return nil, fmt.Errorf("client must be provided")
	}
	if cronDuration <= 0 || cronDuration < time.Second {
		return nil, fmt.Errorf(
			"cron duration must be greater than 1 second",
		) // avoid accidental too-frequent announcements
	}

	return &AnnouncementsManager{
		conf:         conf,
		sconf:        sconf,
		p2pconf:      p2pconf,
		cron:         cron.New(),
		client:       client,
		hiveCreator:  creator,
		cronDuration: cronDuration,
		peerInfo:     peerInfo,
	}, nil
}

// ===== implementing plugin interface =====

func (a *AnnouncementsManager) Init() error {
	// inits context and cancel function
	a.ctx, a.cancel = context.WithCancel(context.Background())
	return nil
}

func (a *AnnouncementsManager) Start() *promise.Promise[any] {
	return promise.New(func(resolve func(any), reject func(error)) {
		// run the first announcement immediately
		go func() {
			err := a.announce(a.ctx)
			if err != nil {
				alog.Error("error announcing", "err", err)
			}
		}()

		cronSpec := fmt.Sprintf("@every %ds", int(a.cronDuration.Seconds()))

		// schedule the task to then run every 24 hours
		_, err := a.cron.AddFunc(cronSpec, func() {
			// check if the context is canceled before running the task
			select {
			case <-a.ctx.Done():
				return
			default:
				go func() {
					err := a.announce(a.ctx)
					if err != nil {
						alog.Error("error announcing", "err", err)
					}
				}()
			}
		})
		if err != nil {
			reject(err)
			return
		}
		a.cron.Start() // start the cron scheduler
		resolve(nil)
	})
}

func (a *AnnouncementsManager) Stop() error {
	// cancel the context
	if a.cancel != nil {
		a.cancel()
	}

	// stop the cron scheduler
	a.cron.Stop()
	return nil
}

// ===== announcement types =====

type payload struct {
	DidKeys  []didConsensusKey `json:"did_keys"`
	VscNode  payloadVscNode    `json:"vsc_node"`
	Services []string          `json:"services"`
}

type payloadVscNode struct {
	NetId               string   `json:"net_id"`
	PeerId              string   `json:"peer_id"`
	PeerAddrs           []string `json:"peer_addrs"`
	Ts                  string   `json:"ts"`
	VersionId           string   `json:"version_id"`
	GitCommit           string   `json:"git_commit"`
	VersionMajor        uint64   `json:"version_major"`
	ProtocolVersion     uint64   `json:"protocol_version"`
	VersionNonConsensus uint64   `json:"version_non_consensus"`
	GatewayKey          string   `json:"gateway_key"`
	GatewayKeyPoP       string   `json:"gateway_key_pop"`
	Witness             struct {
		Enabled bool `json:"enabled"`
	} `json:"witness"`
	// DelegationMode is the operator's consensus-delegation policy
	// (delegationmode.{Deactivated,Share,Custom}). Published so the network can
	// (a) reject third-party delegation to a Deactivated node and (b) split
	// pendulum rewards pro-rata to delegators on a Share node. Consensus 0.3.0+;
	// ignored by pre-0.3.0 logic. Always emitted (default Deactivated) so the
	// witness record carries an explicit, authenticated value.
	DelegationMode string `json:"delegation_mode"`
}

type didConsensusKey struct {
	T   string      `json:"t"`
	Ct  string      `json:"ct"`
	Key dids.BlsDID `json:"key"`
	// PoP is a base64 (raw-url) BLS proof-of-possession for Key, bound to the
	// announcing Hive account. Lets ingesters confirm this node holds the
	// secret behind the announced BLS key (defeats rogue-key aggregate forgery).
	PoP string `json:"pop,omitempty"`
}

// ===== git head commit =====
var (
	GitCommit string = "" // Default value if not set during build
	VersionId string = "go-v0.1.0"
)

// parseAnnounceVersionComponent retains the historical numeric-component parser for tests.
// The canonical running version is now a source constant in consensusversion and is read
// via consensusversion.RunningVersion() (no longer parsed from build-time strings).
func parseAnnounceVersionComponent(raw, field string) uint64 {
	return consensusversion.ParseComponent(raw)
}

// ===== announcement impl =====

// example announcement on-chain: https://hivexplorer.com/tx/cad30bcf0891b6b7f9bcf16a05dc084a02acef65
func (a *AnnouncementsManager) announce(ctx context.Context) error {
	select {
	case <-ctx.Done():
		alog.Debug("announce task canceled")
		return nil
	default:
	}

	// get the account's memo key based on their account username
	accounts, err := a.client.GetAccount([]string{a.conf.Get().HiveUsername})
	if err != nil {
		return fmt.Errorf("failed to get account: %w", err)
	}

	// check if the account exists
	if len(accounts) == 0 {
		return fmt.Errorf("account not found")
	}

	// there should only be one account
	if len(accounts) > 1 {
		return fmt.Errorf("more than one account found, this case should not happen")
	}

	// get the account's memo key
	memoKey := accounts[0].MemoKey
	if memoKey == "" {
		return fmt.Errorf("account has no memo key")
	}

	blsPrivKey := dids.BlsPrivKey{}
	var arr [32]byte
	blsPrivSeedHex := a.conf.Get().BlsPrivKeySeed
	blsPrivSeed, err := hex.DecodeString(blsPrivSeedHex)
	if err != nil {
		return fmt.Errorf("failed to decode bls priv seed: %w", err)
	}
	if len(blsPrivSeed) != 32 {
		return fmt.Errorf("bls priv seed must be 32 bytes")
	}

	copy(arr[:], blsPrivSeed)
	if err = blsPrivKey.Deserialize(&arr); err != nil {
		return fmt.Errorf("failed to deserialize bls priv key: %w", err)
	}
	pubKey, err := ethBls.SkToPk(&blsPrivKey)
	if err != nil {
		return fmt.Errorf("failed to get bls pub key: %w", err)
	}

	// gens the BlsDID from the pub key
	blsDid, err := dids.NewBlsDID(pubKey)
	if err != nil {
		return fmt.Errorf("failed to create bls did: %w", err)
	}

	// Proof-of-possession over the BLS pubkey, bound to this Hive account, so
	// ingesting nodes can confirm we actually hold the secret behind the
	// announced BLS key — defeating rogue-key aggregate-signature forgery.
	blsPoP, err := dids.GenerateBlsPoP(&blsPrivKey, a.conf.Get().HiveUsername)
	if err != nil {
		return fmt.Errorf("failed to generate bls proof-of-possession: %w", err)
	}

	salt := []byte("gateway_key")
	gatewayKey := sha256.Sum256(append(blsPrivSeed, salt...))

	gatewayKP := hivego.KeyPairFromBytes(gatewayKey[:])

	// Proof-of-possession over the gateway secp256k1 key, bound to this Hive
	// account, so ingesting nodes can confirm we hold the secret behind the
	// announced gateway key (audit H-6, companion to the BLS PoP above). Closes
	// the duplicate-key griefing vector — a distinct node announcing our public
	// gateway key to wedge gateway rotation cannot produce this signature.
	gatewayPoP, err := dids.GenerateGatewayKeyPoP(gatewayKP, a.conf.Get().HiveUsername)
	if err != nil {
		return fmt.Errorf("failed to generate gateway proof-of-possession: %w", err)
	}

	peerAddrs := make([]string, 0)

	if len(a.p2pconf.Get().AnnounceAddrs) > 0 {
		peerAddrs = append(peerAddrs, a.p2pconf.Get().AnnounceAddrs...)
	} else {
		for _, addr := range a.safePeerAddrs() {
			peerAddrs = append(peerAddrs, addr.String())
		}
	}

	enabled := a.sconf.OnTestnet() || a.sconf.OnDevnet() ||
		int(a.safeStatus()) == int(network.ReachabilityPublic)

	runningVersion := consensusversion.RunningVersion()

	payload := payload{
		Services: []string{"vsc.network"},
		DidKeys: []didConsensusKey{
			{
				T:   "consensus",
				Ct:  "DID-BLS",
				Key: blsDid,
				PoP: blsPoP,
			},
		},
		VscNode: payloadVscNode{
			//Potentially use specific net ID for E2E tests
			NetId:               a.sconf.NetId(),
			PeerId:              a.safePeerId(), //Plz fill in
			PeerAddrs:           peerAddrs,
			Ts:                  time.Now().Format(time.RFC3339),
			GitCommit:           GitCommit,
			VersionId:           VersionId, //Use standard versioning
			VersionMajor:        runningVersion.Major,
			ProtocolVersion:     runningVersion.Consensus,
			VersionNonConsensus: runningVersion.NonConsensus,
			GatewayKey:          *gatewayKP.GetPublicKeyString(),
			GatewayKeyPoP:       gatewayPoP,
			Witness: struct {
				Enabled bool `json:"enabled"`
			}{
				//Put a proper toggle / on chain configuration option
				//Witness should be enabled/disabled by making a transaction on chain.
				Enabled: enabled,
			},
			// Operator's consensus-delegation policy, normalized to a known mode
			// (default Deactivated). Read back from the witness record at
			// consensus 0.3.0+ to gate delegation acceptance and reward sharing.
			DelegationMode: a.conf.DelegationMode(),
		},
	}

	jsonBytes, err := json.Marshal(payload)
	if err != nil {
		alog.Error("error marshaling JSON", "err", err)
	}

	op := a.hiveCreator.UpdateAccount(a.conf.Get().HiveUsername, nil, nil, nil, string(jsonBytes), memoKey)

	tx := a.hiveCreator.MakeTransaction([]hivego.HiveOperation{op})

	a.hiveCreator.PopulateSigningProps(&tx, nil)

	sig, err := a.hiveCreator.Sign(tx)
	if err != nil {
		return fmt.Errorf("failed to update account: %w", err)
	}

	tx.AddSig(sig)

	_, err = a.hiveCreator.Broadcast(tx)

	if err != nil {
		return fmt.Errorf("failed to update account: %w", err)
	}

	//fmt.Println("Updated account TxId", id)

	return nil
}

func (a *AnnouncementsManager) Announce() {
	ctx := context.Background()

	a.announce(ctx)
}

func (a *AnnouncementsManager) PeerConnect() {

}

func (a *AnnouncementsManager) safePeerAddrs() (out []multiaddr.Multiaddr) {
	defer func() {
		if r := recover(); r != nil {
			alog.Warn("peerInfo.GetPeerAddrs panic recovered", "err", r)
			out = []multiaddr.Multiaddr{}
		}
	}()
	return a.peerInfo.GetPeerAddrs()
}

func (a *AnnouncementsManager) safePeerId() (out string) {
	defer func() {
		if r := recover(); r != nil {
			alog.Warn("peerInfo.GetPeerId panic recovered", "err", r)
			out = ""
		}
	}()
	return a.peerInfo.GetPeerId()
}

func (a *AnnouncementsManager) safeStatus() (out network.Reachability) {
	defer func() {
		if r := recover(); r != nil {
			alog.Warn("peerInfo.GetStatus panic recovered", "err", r)
			out = network.ReachabilityUnknown
		}
	}()
	return a.peerInfo.GetStatus()
}
