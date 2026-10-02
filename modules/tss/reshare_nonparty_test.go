package tss

import (
	"fmt"
	"testing"
	"time"

	tss_helpers "vsc-node/modules/tss/helpers"

	"github.com/stretchr/testify/require"
)

// A node in neither committee of a reshare (it held back its readiness, or is
// blamed or banned) must not start the session: it has no party to run, so the
// session could only end as an empty-culprit timeout that no participant's
// result matches. Start must refuse at once, before any key or network access,
// exactly as keygen and signing refuse a node outside their committee.
func TestReshareStart_RefusesNodeInNeitherCommittee(t *testing.T) {
	for _, prev := range []string{"keygen", "reshare"} {
		t.Run(prev, func(t *testing.T) {
			members := []string{"magi.test1", "magi.test3", "magi.test5"}
			old := make([]Participant, len(members))
			next := make([]Participant, len(members))
			for i, a := range members {
				old[i] = Participant{Account: a}
				next[i] = Participant{Account: a}
			}
			d := &ReshareDispatcher{
				BaseDispatcher: BaseDispatcher{
					tssMgr:       newTestTssManager(t, "magi.test2"),
					algo:         tss_helpers.SigningAlgoEcdsa,
					participants: old,
					sessionId:    "reshare-2600-0-tn-key-04",
					keyId:        "tn-key-04",
					epoch:        2,
					blockHeight:  2600,
					done:         make(chan struct{}, 1),
				},
				newParticipants:    next,
				newEpoch:           3,
				origOldSize:        len(members),
				origNewSize:        len(members),
				prevCommitmentType: prev,
			}

			errc := make(chan error, 1)
			go func() {
				defer func() {
					if r := recover(); r != nil {
						errc <- fmt.Errorf("Start panicked: %v", r)
					}
				}()
				errc <- d.Start()
			}()
			select {
			case err := <-errc:
				require.EqualError(t, err, "node not part of reshare committees")
			case <-time.After(5 * time.Second):
				t.Fatal("Start did not return for a node in neither committee")
			}
		})
	}
}
