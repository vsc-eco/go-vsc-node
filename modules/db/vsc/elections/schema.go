package elections

type ElectionCommonInfo struct {
	Epoch uint64 `json:"epoch" graphql:"epoch" refmt:"epoch" bson:"epoch"`
	NetId string `json:"net_id" graphql:"net_id" refmt:"net_id" bson:"net_id"`
	Type  string `json:"type" graphql:"type" refmt:"type" bson:"type"`
}

type ElectionHeaderInfo struct {
	Data string `json:"data" graphql:"data" refmt:"data" bson:"data"`
}

type ElectionHeader struct {
	ElectionCommonInfo
	ElectionHeaderInfo
}

type ElectionDataInfo struct {
	Members         []ElectionMember `json:"members" graphql:"members" refmt:"members" bson:"members"`
	Weights         []uint64         `json:"weights" graphql:"weights" refmt:"weights" bson:"weights"`
	ProtocolVersion uint64           `json:"protocol_version" graphql:"protocol_version" refmt:"protocol_version" bson:"protocol_version"`
}
type ElectionData struct {
	ElectionCommonInfo
	ElectionDataInfo
}

type ElectionResult struct {
	ElectionCommonInfo
	ElectionHeaderInfo
	ElectionDataInfo

	TotalWeight uint64 `json:"total_weight" graphql:"total_weight" refmt:"total_weight" bson:"total_weight"`
	BlockHeight uint64 `json:"block_height" graphql:"block_height" refmt:"block_height" bson:"block_height"`
	Proposer    string `json:"proposer" graphql:"proposer" refmt:"proposer" bson:"proposer"`
	TxId        string `json:"tx_id" graphql:"tx_id" refmt:"tx_id" bson:"tx_id"`
}

type ElectionResultRecord struct {
	Epoch           uint64           `json:"epoch" graphql:"epoch" refmt:"epoch" bson:"epoch"`
	NetId           string           `json:"net_id" graphql:"net_id" refmt:"net_id" bson:"net_id"`
	Data            string           `json:"data" graphql:"data" refmt:"data" bson:"data"`
	Members         []ElectionMember `json:"members" graphql:"members" refmt:"members" bson:"members"`
	Weights         []uint64         `json:"weights" graphql:"weights" refmt:"weights" bson:"weights"`
	ProtocolVersion uint64           `json:"protocol_version" graphql:"protocol_version" refmt:"protocol_version" bson:"protocol_version"`
	TotalWeight     uint64           `json:"total_weight" graphql:"total_weight" refmt:"total_weight" bson:"total_weight"`
	BlockHeight     uint64           `json:"block_height" graphql:"block_height" refmt:"block_height" bson:"block_height"`
	Proposer        string           `json:"proposer" graphql:"proposer" refmt:"proposer" bson:"proposer"`
	Type            string           `json:"type" graphql:"type" refmt:"type" bson:"type"`
	TxId            string           `json:"tx_id" graphql:"tx_id" refmt:"tx_id" bson:"tx_id"`
}

type ElectionMember struct {
	Key     string `json:"key" graphql:"key" refmt:"key"`
	Account string `json:"account" graphql:"account" refmt:"account"`
}
