package common_test

import (
	"testing"
	"vsc-node/modules/common"

	"github.com/stretchr/testify/assert"
)

func TestContractId(t *testing.T) {
	contractId := common.ContractId("bef70add6d21cd812cf68da2caee72da05de48b4", 0)
	assert.Equal(t, contractId, "vsc1Bem8RnoLgGPP7E2MBN52ekrdVqy2LNpSqF")
}
