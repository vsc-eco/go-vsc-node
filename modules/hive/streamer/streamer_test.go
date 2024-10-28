package streamer_test

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"testing"
	"time"
	"vsc-node/modules/db"
	"vsc-node/modules/db/vsc"
	"vsc-node/modules/db/vsc/hive_blocks"
	"vsc-node/modules/hive/streamer"

	"github.com/stretchr/testify/assert"
	"github.com/vsc-eco/hivego"
)

func setupAndCleanUpDataDir(t *testing.T) {

	t.Cleanup(func() {
		os.RemoveAll("data")
	})

	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, os.Interrupt, syscall.SIGTERM)

	go func() {
		<-signalChan
		os.RemoveAll("data")
		os.Exit(1)
	}()
}

func TestStream(t *testing.T) {
	// setupAndCleanUpDataDir(t)

	time.Sleep(1 * time.Second)

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

	// chmod 755 /data
	os.Chmod("data", 0755)

	// hive blocks
	hiveBlockDbManager := hive_blocks.New(vscDb)
	// assert.NoError(t, hiveBlockDbManager.ClearBlocks())

	filter1 := func(op hivego.Operation) bool {
		return true
		// return op.Type == "transfer_operation"
	}

	filter2 := func(op hivego.Operation) bool {
		return true
		// return op.Type == "custom_json_operation"
	}

	// streamer props
	filters := []streamer.FilterFunc{filter1, filter2}
	process := func(block hive_blocks.HiveBlock) error {
		fmt.Printf("processed block: %+v\n", block)
		fmt.Printf("it had txs: %+v\n", block.Transactions)
		return nil
	}

	// create streamer
	startBlock := 5000
	s := streamer.New(hiveBlockDbManager, filters, process, &startBlock)
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

	// todo: remove
	select {}
}
