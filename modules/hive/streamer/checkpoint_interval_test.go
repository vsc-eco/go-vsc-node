package streamer_test

import (
	"testing"
	"time"

	"vsc-node/lib/test_utils"
	"vsc-node/modules/db/vsc/hive_blocks"
	"vsc-node/modules/hive/streamer"

	"github.com/stretchr/testify/assert"
)

// The persisted last-processed block follows processing at CheckpointInterval:
// the devnet harness waits on it, and once a second trails fast blocks by ten.
func TestCheckpointFollowsCheckpointInterval(t *testing.T) {
	prev := streamer.CheckpointInterval
	t.Cleanup(func() { streamer.CheckpointInterval = prev })
	streamer.CheckpointInterval = 20 * time.Millisecond

	db := &test_utils.MockHiveBlockDb{}
	seedBlockData(t, db, 10) // under the 100-block forced checkpoint
	sr := streamer.NewStreamReader(db, func(hive_blocks.HiveBlock, *uint64) {}, nil, 0)
	assert.NoError(t, sr.Init())
	sr.Start()
	t.Cleanup(func() { sr.Stop() })

	var got uint64
	for deadline := time.Now().Add(150 * time.Millisecond); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if got, _ = db.GetLastProcessedBlock(); got == 10 {
			return
		}
	}
	t.Fatalf("persisted last-processed block %d 150ms after processing 10 blocks with a 20ms checkpoint interval, want 10", got)
}
