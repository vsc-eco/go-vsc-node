package contractinterface

const DirPathDelimiter = "-"
const ObservedContractPrefix = "observed_txs"
const TxSpendRegistryContractKey = "txspdr"
const TxSpendContractPrefix = "txspd" + DirPathDelimiter
const LastHeightContractKey = "lsthgt"

// ContractId is set at startup from the CONTRACT_ID environment variable.
var ContractId string
