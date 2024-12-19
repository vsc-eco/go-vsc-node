package elections

type ElectionResult struct {
	Epoch       int              `json:"epoch" bson:"epoch"`
	NetId       string           `json:"net_id" bson:"net_id"`
	BlockHeight int              `json:"block_height" bson:"block_height"`
	Data        string           `json:"data" bson:"data"`
	Members     []ElectionMember `json:"members" bson:"members"`
	Proposer    string           `json:"proposer" bson:"proposer"`
	Weights     []int64          `json:"weights" bson:"weights"`
	WeightTotal int64            `json:"weight_total" bson:"weight_total`
}

type ElectionMember struct {
	Key     string `json:"key"`
	Account string `json:"account"`
}
