package streamer_test

import (
	"fmt"
	"testing"
	"time"
	"vsc-node/modules/db"
	"vsc-node/modules/db/vsc"
	"vsc-node/modules/db/vsc/hive_blocks"
	"vsc-node/modules/hive/streamer"

	"github.com/stretchr/testify/assert"
)

func TestStream(t *testing.T) {
	// db conn
	d := db.New()
	assert.NoError(t, d.Init())
	assert.NoError(t, d.Start())
	defer func() {
		assert.NoError(t, d.Stop())
	}()

	// vsc db
	vscDb := vsc.New(d)
	assert.NoError(t, vscDb.Init())
	assert.NoError(t, vscDb.Start())
	defer func() {
		assert.NoError(t, vscDb.Stop())
	}()

	// hive blocks
	hiveBlockDbManager := hive_blocks.New(vscDb)
	// assert.NoError(t, hiveBlockDbManager.ClearBlocks())

	filter1 := func(op hive_blocks.Op) bool {
		return true
		// return op.Type == "transfer_operation"
	}

	filter2 := func(op hive_blocks.Op) bool {
		return true
		// return op.Type == "custom_json_operation"
	}

	// streamer props
	filters := []streamer.FilterFunc{filter1, filter2}
	process := func(block hive_blocks.HiveBlock) error {
		fmt.Printf("processed block: %+v\n", block)
		return nil
	}

	// create streamer
	// startBlock := 5000
	s := streamer.New(hiveBlockDbManager, filters, process, nil)
	assert.NoError(t, s.Init())
	assert.NoError(t, s.Start())
	defer func() {
		assert.NoError(t, s.Stop())
	}()

	go func() {
		<-time.After(10 * time.Second)
		s.Pause()
	}()

	go func() {
		<-time.After(20 * time.Second)
		s.Resume()
	}()

	select {}
}
