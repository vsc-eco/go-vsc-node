package announcements

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"time"
	"vsc-node/lib/dids"
	agg "vsc-node/modules/aggregate"
	"vsc-node/modules/config"

	"github.com/robfig/cron/v3"
	"github.com/vsc-eco/hivego"

	"github.com/chebyrash/promise"
	ethBls "github.com/protolambda/bls12-381-util"
)

// ===== types =====

type announcementsManager struct {
	conf         *config.Config[announcementsConfig]
	cron         *cron.Cron
	ctx          context.Context
	cancel       context.CancelFunc
	client       HiveRpcClient
	cronDuration time.Duration
}

type HiveRpcClient interface {
	GetAccount(accountNames []string) ([]hivego.AccountData, error)
	UpdateAccount(account string, owner *hivego.Auths, active *hivego.Auths, posting *hivego.Auths, jsonMetadata string, memoKey string, wif *string) (string, error)
}

// ===== interface assertions =====

var _ agg.Plugin = &announcementsManager{}

// ===== constructor =====

func New(client HiveRpcClient, conf *config.Config[announcementsConfig], cronDuration time.Duration) (*announcementsManager, error) {

	// sanity checks
	if conf == nil {
		return nil, fmt.Errorf("config must be provided")
	}
	if client == nil {
		return nil, fmt.Errorf("client must be provided")
	}
	if cronDuration <= 0 || cronDuration < time.Second {
		return nil, fmt.Errorf("cron duration must be greater than 1 second") // avoid accidental too-frequent announcements
	}

	return &announcementsManager{
		cron:         cron.New(),
		conf:         conf,
		client:       client,
		cronDuration: cronDuration,
	}, nil
}

// ===== implementing plugin interface =====

func (a *announcementsManager) Init() error {
	// inits context and cancel function
	a.ctx, a.cancel = context.WithCancel(context.Background())
	return nil
}

func (a *announcementsManager) Start() *promise.Promise[any] {
	return promise.New(func(resolve func(any), reject func(error)) {
		// run the first announcement immediately
		go func() {
			err := a.announce(a.ctx)
			if err != nil {
				log.Println("error announcing:", err)
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
						log.Println("error announcing:", err)
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

func (a *announcementsManager) Stop() error {
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
	DidKeys []didConsensusKey `json:"did_keys"`
}

type didConsensusKey struct {
	T   string      `json:"t"`
	Ct  string      ` json:"ct"`
	Key dids.BlsDID `json:"key"`
}

// ===== announcement impl =====

// example announcement on-chain: https://hivexplorer.com/tx/cad30bcf0891b6b7f9bcf16a05dc084a02acef65
func (a *announcementsManager) announce(ctx context.Context) error {
	select {
	case <-ctx.Done():
		log.Println("announce task canceled")
		return nil
	default:
		log.Println("announcing")
	}

	// get the account's memo key based on their account username
	accounts, err := a.client.GetAccount([]string{a.conf.Get().Username})
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

	payload := payload{
		DidKeys: []didConsensusKey{
			{
				T:   "consensus",
				Ct:  "DID-BLS",
				Key: blsDid,
			},
		},
	}

	jsonBytes, err := json.Marshal(payload)
	if err != nil {
		log.Println("error marshaling JSON:", err)
	}

	wif := a.conf.Get().AnnouncementPrivateWif
	_, err = a.client.UpdateAccount(a.conf.Get().Username, nil, nil, nil, string(jsonBytes), memoKey, &wif)
	if err != nil {
		return fmt.Errorf("failed to update account: %w", err)
	}

	return nil
}
