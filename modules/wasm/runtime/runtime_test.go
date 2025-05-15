package wasm_runtime_test

import (
	"encoding/json"
	"testing"
	wasm_runtime "vsc-node/modules/wasm/runtime"

	"github.com/stretchr/testify/assert"
	"go.mongodb.org/mongo-driver/bson"
)

func TestJSON(t *testing.T) {
	b, err := json.Marshal(wasm_runtime.AssemblyScript)
	assert.NoError(t, err)
	assert.Equal(t, "\"assembly-script\"", string(b))

	var r wasm_runtime.Runtime
	err = json.Unmarshal(b, &r)
	assert.NoError(t, err)
	assert.Equal(t, wasm_runtime.AssemblyScript, r)
}

func TestBSON(t *testing.T) {
	type Container struct {
		Runtime wasm_runtime.Runtime
	}
	c := Container{
		wasm_runtime.AssemblyScript,
	}
	b, err := bson.Marshal(c)
	assert.NoError(t, err)
	assert.Contains(t, string(b), "assembly-script")

	var r Container
	err = bson.Unmarshal(b, &r)
	assert.NoError(t, err)
	assert.Equal(t, wasm_runtime.AssemblyScript, r.Runtime)
}
