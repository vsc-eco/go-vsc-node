package btcrelay

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestFetchChain(t *testing.T) {
	bcr := New()

	headBlock, err := bcr.fetchChain()
	assert.NoError(t, err)

	jsonBytes, _ := json.MarshalIndent(headBlock, "", "  ")
	t.Log(string(jsonBytes))
}
