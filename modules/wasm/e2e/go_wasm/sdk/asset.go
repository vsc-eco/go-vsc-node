package sdk

type Asset string

const (
	AssetHive       Asset = "hive"
	AssetHiveCons   Asset = "hive_consensus"
	AssetHbd        Asset = "hbd"
	AssetHbdSavings Asset = "hbd_savings"
)

func (a Asset) String() string {
	return string(a)
}
