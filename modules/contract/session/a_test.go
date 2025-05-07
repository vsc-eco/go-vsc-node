package contract_session_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestA(t *testing.T) {
	var a []byte
	assert.Nil(t, a)
	a = make([]byte, 0)
	assert.NotNil(t, a)
}
