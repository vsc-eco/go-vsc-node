package devnet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"
)

// blockAPIRestarts is how many times waitForBlockAPI restarts hafah-postgrest
// before failing the boot.
const blockAPIRestarts = 2

// waitForBlockAPI checks that drone serves block_api.get_block_range end to end
// (drone, hafah-postgrest, HAF) before any node starts. PostgREST can come up
// with a schema cache that lacks hafah_endpoints.home (PGRST202); drone then
// answers every range with a null result and the nodes' streamer retries it
// forever at a log level the tests do not print, so no node ever processes a
// block. That is the boot hang the H-37 magi restart cannot clear. Restarting
// hafah-postgrest rebuilds its schema cache.
func (d *Devnet) waitForBlockAPI(ctx context.Context) error {
	client := &http.Client{Timeout: 5 * time.Second}
	var err error
	for restart := 0; restart <= blockAPIRestarts; restart++ {
		if restart > 0 {
			log.Printf("[devnet] drone serves no blocks (%v); restarting hafah-postgrest to reload its schema cache (%d/%d)",
				err, restart, blockAPIRestarts)
			if rerr := d.compose(ctx, "restart", "hafah-postgrest"); rerr != nil {
				return fmt.Errorf("restarting hafah-postgrest: %w", rerr)
			}
			if werr := d.waitForService(ctx, "hafah-postgrest", 2*time.Minute); werr != nil {
				return werr
			}
		}
		deadline := time.Now().Add(30 * time.Second) // wall: boot, before any node starts
		for {
			if err = blockAPIServes(ctx, client, d.DroneEndpoint()); err == nil {
				return nil
			}
			if time.Now().After(deadline) { // wall: boot, before any node starts
				break
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Second): // wall: boot, before any node starts
			}
		}
	}
	return fmt.Errorf("drone does not serve block_api.get_block_range after %d hafah-postgrest restarts: %w",
		blockAPIRestarts, err)
}

// blockAPIServes asks drone for block 1 and requires it back.
func blockAPIServes(ctx context.Context, client *http.Client, url string) error {
	var res struct {
		Blocks []json.RawMessage `json:"blocks"`
	}
	body := `{"jsonrpc":"2.0","id":1,"method":"block_api.get_block_range","params":{"starting_block_num":1,"count":1}}`
	if err := pmRPC(ctx, client, url, body, &res); err != nil {
		return err
	}
	if len(res.Blocks) == 0 {
		return errors.New("block_api.get_block_range returned no blocks")
	}
	return nil
}
