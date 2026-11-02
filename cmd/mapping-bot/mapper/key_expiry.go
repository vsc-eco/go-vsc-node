package mapper

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"
)

// B8 (EVM bot parity, health.go D7): a TSS key expires after its epochs and,
// once "deprecated", retires about 14 days later and can never sign again; the
// vault's coins then move only by the backup key after its CSV delay. The
// mainnet BTC key expired once (epoch 2102) with nothing warning anyone. The
// bot now warns, once per keyExpiryCheckEvery and on /health, for every key a
// fund-holding generation still uses: expired ("deprecated"), or expiring
// within keyExpiryWarnEpochs (renewKey is the owner's call).

// keyExpiryWarnEpochs: how early to warn (28 epochs, ~7 days; as the EVM bot).
const keyExpiryWarnEpochs = 28

var keyExpiryCheckEvery = time.Hour

type tssKeyExpiry struct {
	status string
	expiry uint64
}

// keyExpiryIssues is the pure D7 rule, sorted by key id.
func keyExpiryIssues(keys map[string]tssKeyExpiry, epoch uint64) []string {
	ids := make([]string, 0, len(keys))
	for id := range keys {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var out []string
	for _, id := range ids {
		k := keys[id]
		switch {
		case k.status == "deprecated":
			out = append(out, fmt.Sprintf("TSS key %s has expired: run renewKey now (it retires ~14 days after expiring and then can never sign)", id))
		case k.status == "active" && k.expiry > 0 && k.expiry <= epoch+keyExpiryWarnEpochs:
			left := k.expiry - min(k.expiry, epoch)
			out = append(out, fmt.Sprintf("TSS key %s expires in %d epochs (~%dh): run renewKey", id, left, 6*left))
		}
	}
	return out
}

// fetchTssKeyExpiry reads a key's status and expiry epoch; found is false when
// the node has no such key.
func (b *Bot) fetchTssKeyExpiry(ctx context.Context, keyId string) (tssKeyExpiry, bool, error) {
	reqBody, err := json.Marshal(map[string]any{
		"query":     `query($id: String!){ getTssKey(keyId: $id){ status expiry_epoch } }`,
		"variables": map[string]any{"id": keyId},
	})
	if err != nil {
		return tssKeyExpiry{}, false, err
	}
	var result struct {
		Data struct {
			GetTssKey *struct {
				Status      string `json:"status"`
				ExpiryEpoch uint64 `json:"expiry_epoch"`
			} `json:"getTssKey"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := b.gqlHTTPPost(ctx, reqBody, func(resp *http.Response) error {
		return json.NewDecoder(resp.Body).Decode(&result)
	}); err != nil {
		return tssKeyExpiry{}, false, err
	}
	if len(result.Errors) > 0 {
		return tssKeyExpiry{}, false, fmt.Errorf("graphql error: %s", result.Errors[0].Message)
	}
	if result.Data.GetTssKey == nil || result.Data.GetTssKey.Status == "" {
		return tssKeyExpiry{}, false, nil
	}
	return tssKeyExpiry{status: result.Data.GetTssKey.Status, expiry: result.Data.GetTssKey.ExpiryEpoch}, true, nil
}

// fetchCurrentEpoch reads the network's current election epoch.
func (b *Bot) fetchCurrentEpoch(ctx context.Context) (uint64, error) {
	reqBody, _ := json.Marshal(map[string]any{"query": `{ electionByBlockHeight { epoch } }`})
	var result struct {
		Data struct {
			ElectionByBlockHeight *struct {
				Epoch uint64 `json:"epoch"`
			} `json:"electionByBlockHeight"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := b.gqlHTTPPost(ctx, reqBody, func(resp *http.Response) error {
		return json.NewDecoder(resp.Body).Decode(&result)
	}); err != nil {
		return 0, err
	}
	if len(result.Errors) > 0 {
		return 0, fmt.Errorf("graphql error: %s", result.Errors[0].Message)
	}
	if result.Data.ElectionByBlockHeight == nil {
		return 0, fmt.Errorf("no election")
	}
	return result.Data.ElectionByBlockHeight.Epoch, nil
}

// KeyExpiryIssues returns the D7 warnings for every key a live generation uses
// (the legacy "main" key plus each generation short of PURGED).
func (b *Bot) KeyExpiryIssues(ctx context.Context) ([]string, error) {
	vaults, err := b.FetchVaultRegistry(ctx)
	if err != nil {
		return nil, fmt.Errorf("read the vault registry: %w", err)
	}
	epoch, err := b.fetchCurrentEpoch(ctx)
	if err != nil {
		return nil, fmt.Errorf("read the current epoch: %w", err)
	}
	keys := map[string]tssKeyExpiry{}
	for _, id := range signatureKeyIds(b.BotConfig.ContractId(), vaults) {
		k, found, err := b.fetchTssKeyExpiry(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("read key %s: %w", id, err)
		}
		if found {
			keys[id] = k
		}
	}
	return keyExpiryIssues(keys, epoch), nil
}

var (
	keyExpiryMu        sync.Mutex
	keyExpiryCheckedAt time.Time
	keyExpiryLast      []string // the last check's warnings, served on /health
)

// CachedKeyExpiryIssues returns the last check's warnings (no node query).
func (b *Bot) CachedKeyExpiryIssues() []string {
	keyExpiryMu.Lock()
	defer keyExpiryMu.Unlock()
	return append([]string(nil), keyExpiryLast...)
}

// WarnKeyExpiry logs the D7 warnings at most once per keyExpiryCheckEvery.
func (b *Bot) WarnKeyExpiry() {
	keyExpiryMu.Lock()
	if !keyExpiryCheckedAt.IsZero() && time.Since(keyExpiryCheckedAt) < keyExpiryCheckEvery {
		keyExpiryMu.Unlock()
		return
	}
	keyExpiryCheckedAt = time.Now()
	keyExpiryMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	issues, err := b.KeyExpiryIssues(ctx)
	if err != nil {
		b.L.Warn("D7: cannot read the vault keys' expiry", "err", err)
		return
	}
	keyExpiryMu.Lock()
	keyExpiryLast = issues
	keyExpiryMu.Unlock()
	for _, msg := range issues {
		b.L.Error("D7: " + msg)
	}
}
