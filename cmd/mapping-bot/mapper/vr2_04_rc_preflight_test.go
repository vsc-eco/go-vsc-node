package mapper

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// VR2-04: the bot must refuse a rotation op it cannot pay for, BEFORE submitting.
//
// A rotation cycle costs roughly 28,000 RC across map, topUpFeeReserve,
// migrateVault and confirmSpend, and there was no pre-flight at all. Running out
// partway is the damaging case: it leaves a generation half-swept with an
// in-flight spend the same account can no longer finish, which needs manual
// recovery. A testnet confirmSpend aborted at RC 83 exactly this way. Failing
// before the first op is merely an operator inconvenience.
func TestVR204_RefusesWhenRcIsBelowTheOpCost(t *testing.T) {
	gql := &mockGraphQL{}
	bot, did := buildBotForL2Test(t, gql)

	rcLimit := bot.rcLimitFor("migrateVault")
	gql.accountRC = map[string]int64{did.String(): int64(rcLimit) - 1}

	_, err := bot.callContractL2(context.Background(), []byte(`{}`), "migrateVault")
	if err == nil {
		t.Fatal("expected a refusal when available RC is below the op's own cost")
	}
	if !strings.Contains(err.Error(), "insufficient RC") {
		t.Errorf("the refusal must name the cause; got %q", err)
	}
	// The point of a pre-flight is that nothing was broadcast.
	for _, c := range gql.calls {
		if c.Method == "SubmitTransactionV1" {
			t.Fatal("a refused op must not be submitted — that is the whole point of " +
				"checking before rather than after")
		}
	}
}

// Sufficient credits must not be obstructed. A pre-flight that blocks legitimate
// rotations would be worse than the stall it prevents.
func TestVR204_AllowsWhenRcCoversTheOp(t *testing.T) {
	gql := &mockGraphQL{}
	bot, did := buildBotForL2Test(t, gql)

	gql.accountRC = map[string]int64{did.String(): int64(bot.rcLimitFor("migrateVault")) * 10}

	if _, err := bot.callContractL2(context.Background(), []byte(`{}`), "migrateVault"); err != nil {
		if strings.Contains(err.Error(), "insufficient RC") {
			t.Fatalf("ample RC was refused: %v", err)
		}
	}
}

// An unreadable RC balance must FAIL OPEN. A monitoring outage must not become a
// rotation outage — and the node still rejects the op itself if the credits
// really are missing, so nothing is lost by proceeding.
func TestVR204_UnreadableRcDoesNotBlockTheOp(t *testing.T) {
	for name, gql := range map[string]*mockGraphQL{
		"query failed":     {rcErr: errors.New("node unreachable")},
		"no record at all": {},
	} {
		bot, _ := buildBotForL2Test(t, gql)
		_, err := bot.callContractL2(context.Background(), []byte(`{}`), "migrateVault")
		if err != nil && strings.Contains(err.Error(), "insufficient RC") {
			t.Errorf("%s: must not refuse on an unreadable balance; got %v", name, err)
		}
	}
}
