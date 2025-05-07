package hive_mock_network

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"
	"vsc-node/lib/utils"

	"github.com/chebyrash/promise"

	"github.com/creachadair/jrpc2/handler"
	"github.com/creachadair/jrpc2/jhttp"
)

type Accounts struct {
	Accounts []string
}

type RcAccounts struct {
	RcAccounts []RcAccount `json:"rc_accounts"`
}

type RcAccount struct {
	RcManabar `json:"rc_manabar"`
	MaxRc     Mana `json:"max_rc"`
}

type RcManabar struct {
	LastUpdateTime time.Time `json:"last_update_time"`
	CurrentMana    Mana      `json:"current_mana"`
}

type Mana int

type Account struct {
	Account AccountInfo `json:"account"`
}

type AccountInfo struct {
	DelegatedVestingShares VestingShares `json:"delegated_vesting_shares"`
	ReceivedVestingShares  VestingShares `json:"received_vesting_shares"`
	RewardVestingBalance   VestingShares `json:"reward_vesting_balancev"`
	RewardHBDBalance       Currency      `json:"reward_hbd_balance"`
	RewardHIVEBalance      Currency      `json:"reward_hive_balance"`
}

type VestingShares int

type Currency int

func Network() (func(context.Context) error, *promise.Promise[any]) {
	mux := http.NewServeMux()
	server := &http.Server{Addr: ":7779", Handler: mux}
	// rpcServer := rpc.NewServer()
	b := jhttp.NewBridge(handler.Map{
		"condenser_api.get_accounts": handler.New(func(ctx context.Context, ss [][]string) []Account {
			return utils.Map(ss[0], func(account string) Account {
				return Account{
					AccountInfo{
						VestingShares(20),
						VestingShares(30),
						VestingShares(35),
						Currency(40),
						Currency(50),
					},
				}
			})
		}),
		"condenser_api.get_dynamic_global_properties": handler.New(func(ctx context.Context, ss []string) string {
			return strings.Join(ss, " ")
		}),
		"condenser_api.get_current_median_history_price": handler.New(func(ctx context.Context, ss []string) string {
			return strings.Join(ss, " ")
		}),
		"condenser_api.get_reward_fund": handler.New(func(ctx context.Context, ss []string) string {
			return strings.Join(ss, " ")
		}),
		"rc_api.find_rc_accounts": handler.New(func(ctx context.Context, ss Accounts) RcAccounts {
			return RcAccounts{
				utils.Map(ss.Accounts, func(account string) RcAccount {
					return RcAccount{
						RcManabar{},
						Mana(10),
					}
				}),
			}
		}),
		"condenser_api.get_withdraw_routes": handler.New(func(ctx context.Context, ss []string) string {
			return strings.Join(ss, " ")
		}),
		"condenser_api.get_open_orders": handler.New(func(ctx context.Context, ss []string) []string {
			return ss
		}),
		"condenser_api.get_conversion_requests": handler.New(func(ctx context.Context, ss []string) string {
			return strings.Join(ss, " ")
		}),
		"condenser_api.get_collateralized_conversion_requests": handler.New(func(ctx context.Context, ss []string) []string {
			return ss
		}),
	}, nil)
	mux.Handle("POST /", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Println("got req")
		b.ServeHTTP(w, r)
		// conn, _, err := w.(http.Hijacker).Hijack()
		// if err != nil {
		// 	log.Print("rpc hijacking ", r.RemoteAddr, ": ", err.Error())
		// 	return
		// }
		// var connected = "200 Connected to Go RPC"
		// io.WriteString(conn, "HTTP/1.0 "+connected+"\n\n")
		// rpcServer.ServeCodec(jsonrpc.NewServerCodec(conn))
		// rpcServer.ServeHTTP(w, r)
	}))
	mux.Handle("GET /health", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte{})
	}))
	return func(ctx context.Context) error {
			return server.Shutdown(ctx)
		}, promise.New(func(resolve func(any), reject func(error)) {
			defer b.Close()
			err := server.ListenAndServe()
			if err != nil {
				reject(err)
			}
			resolve(nil)
		})
}
