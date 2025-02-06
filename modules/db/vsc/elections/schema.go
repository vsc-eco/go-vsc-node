package elections

type electionCommonInfo struct {
	Epoch uint64 `json:"epoch" bson:"epoch"`
	NetId string `json:"net_id" bson:"net_id"`
}

type electionHeaderInfo struct {
	Data string `json:"data" bson:"data"`
}

type ElectionHeader struct {
	electionCommonInfo
	electionHeaderInfo
}

type electionDataInfo struct {
	Members         []ElectionMember `json:"members" bson:"members"`
	Weights         []uint64         `json:"weights" bson:"weights"`
	ProtocolVersion uint64           `json:"protocol_version" bson:"protocol_version"`
}
type ElectionData struct {
	electionCommonInfo
	electionDataInfo
}

type ElectionResult struct {
	electionCommonInfo
	electionHeaderInfo
	electionDataInfo

	TotalWeight uint64 `json:"total_weight" bson:"total_weight"`
	BlockHeight uint64 `json:"block_height" bson:"block_height"`
	Proposer    string `json:"proposer" bson:"proposer"`
}

type ElectionMember struct {
	Key     string `json:"key"`
	Account string `json:"account"`
}
