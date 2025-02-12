package elections

type electionCommonInfo struct {
	Epoch uint64 `json:"epoch" refmt:"epoch" bson:"epoch"`
	NetId string `json:"net_id" refmt:"net_id" bson:"net_id"`
}

type electionHeaderInfo struct {
	Data string `json:"data" refmt:"data" bson:"data"`
}

type ElectionHeader struct {
	electionCommonInfo
	electionHeaderInfo
}

type electionDataInfo struct {
	Members         []ElectionMember `json:"members" refmt:"members" bson:"members"`
	Weights         []uint64         `json:"weights" refmt:"weights" bson:"weights"`
	ProtocolVersion uint64           `json:"protocol_version" refmt:"protocol_version" bson:"protocol_version"`
}
type ElectionData struct {
	electionCommonInfo
	electionDataInfo
}

type ElectionResult struct {
	electionCommonInfo
	electionHeaderInfo
	electionDataInfo

	TotalWeight uint64 `json:"total_weight" refmt:"total_weight" bson:"total_weight"`
	BlockHeight uint64 `json:"block_height" refmt:"block_height" bson:"block_height"`
	Proposer    string `json:"proposer" refmt:"proposer" bson:"proposer"`
}

type ElectionResultRecord struct {
	Epoch           uint64           `json:"epoch" refmt:"epoch" bson:"epoch"`
	NetId           string           `json:"net_id" refmt:"net_id" bson:"net_id"`
	Data            string           `json:"data" refmt:"data" bson:"data"`
	Members         []ElectionMember `json:"members" refmt:"members" bson:"members"`
	Weights         []uint64         `json:"weights" refmt:"weights" bson:"weights"`
	ProtocolVersion uint64           `json:"protocol_version" refmt:"protocol_version" bson:"protocol_version"`
	TotalWeight     uint64           `json:"total_weight" refmt:"total_weight" bson:"total_weight"`
	BlockHeight     uint64           `json:"block_height" refmt:"block_height" bson:"block_height"`
	Proposer        string           `json:"proposer" refmt:"proposer" bson:"proposer"`
}

type ElectionMember struct {
	Key     string `json:"key" refmt:"key"`
	Account string `json:"account" refmt:"account"`
}
