package EventEngine

import "github.com/ipfs/boxo/blockservice"

type VEvent struct {
	op string
	lo string
}

type EventEngine struct {
	BlockService  blockservice.BlockService
	EventDispatch *chan VEvent
	Events        []VEvent
}

func (ee *EventEngine) ProcessBlock() {

}
