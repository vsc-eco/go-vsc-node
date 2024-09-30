package witnesses

type Witness struct {
	NodeId string
	Key    Key
}

type Key interface {
	Bytes() []byte
}
