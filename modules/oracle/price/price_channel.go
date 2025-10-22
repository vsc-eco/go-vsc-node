package price

import "errors"

var (
	errPriceChannelFull = errors.New("price channel full")
	errPriceChannelNil  = errors.New("price channel not initialized")
)

type PriceChannel struct {
	c chan map[string]PricePoint
}

func makePriceChannel() *PriceChannel {
	return &PriceChannel{
		c: nil,
	}
}

func (p *PriceChannel) Open() {
	p.c = make(chan map[string]PricePoint, 128)
}

func (p *PriceChannel) Close() {
	close(p.c)
	p.c = nil
}

func (p *PriceChannel) Receive(data map[string]PricePoint) error {
	if p.c == nil {
		return errPriceChannelNil
	}

	select {
	case p.c <- data:
		return nil

	default:
		return errPriceChannelFull
	}
}
