package libp2p

import (
	"context"
	"fmt"
	"io"
	"time"
	start_status "vsc-node/modules/start-status"

	"github.com/chebyrash/promise"
	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"github.com/libp2p/go-libp2p/core/peer"
)

type SendFunc[Msg any] func(msg Msg) error

type PubSubServiceParams[Msg any] interface {
	Topic() string

	ValidateMessage(ctx context.Context, from peer.ID, msg *pubsub.Message, parsedMsg Msg) bool
	HandleMessage(ctx context.Context, from peer.ID, msg Msg, send SendFunc[Msg]) error
	HandleRawMessage(ctx context.Context, rawMsg *pubsub.Message, send SendFunc[Msg]) error

	ParseMessage(data []byte) (Msg, error)
	SerializeMessage(msg Msg) []byte
}

type PubSubService[Msg any] = *pubSubService[Msg]

type pubSubService[Msg any] struct {
	topic       *pubsub.Topic
	cancelRelay pubsub.RelayCancelFunc
	sub         *pubsub.Subscription

	params PubSubServiceParams[Msg]

	ctx       context.Context
	cancelCtx context.CancelFunc

	startStatus start_status.StartStatus
}

var _ io.Closer = &pubSubService[any]{}
var _ start_status.Starter = &pubSubService[any]{}

// Started implements io.Closer.
func (p *pubSubService[Msg]) Close() error {
	p.cancelRelay()
	p.sub.Cancel()
	p.cancelCtx()
	return p.topic.Close()
}

func (p *pubSubService[Msg]) Send(msg Msg) error {
	b := p.params.SerializeMessage(msg)
	return p.topic.Publish(p.ctx, b)
}

func (p *pubSubService[Msg]) Context() context.Context {
	return p.ctx
}

// Started implements start_status.Starter.
func (p *pubSubService[Msg]) Started() *promise.Promise[any] {
	return p.startStatus.Started()
}

func NewPubSubService[Msg any](p2p *P2PServer, service PubSubServiceParams[Msg]) (PubSubService[Msg], error) {
	topic, err := p2p.pubsub.Join(service.Topic())
	if err != nil {
		return nil, err
	}

	cancelRelay, err := topic.Relay()
	if err != nil {
		return nil, err
	}

	// Invalid messages are not relayed nor processed by subscribers
	err = p2p.pubsub.RegisterTopicValidator(service.Topic(), func(ctx context.Context, from peer.ID, msg *pubsub.Message) bool {
		parsedMsg, err := service.ParseMessage(msg.GetData())
		if err != nil {
			return false
		}
		return service.ValidateMessage(ctx, from, msg, parsedMsg)
	})
	if err != nil {
		return nil, err
	}

	sub, err := topic.Subscribe(pubsub.WithBufferSize(32)) //TODO might need to increase buffer size if messages are dropped
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(context.TODO())

	startStatus := start_status.New()

	res := &pubSubService[Msg]{
		topic,
		cancelRelay,
		sub,
		service,
		ctx,
		cancel,
		startStatus,
	}

	go func() {
		_, err := promise.All(ctx,
			p2p.Started(),
			promise.New(func(resolve func(any), reject func(error)) {
				ticker := time.NewTicker(time.Second)
				defer ticker.Stop()
				for {
					select {
					case <-ctx.Done():
						return
					case <-ticker.C:
						// peers := topic.ListPeers()
						resolve(nil)
						// fmt.Println(p2p.PeerInfo().GetPeerId(), "pubsub", service.Topic(), "peers:", len(peers))
						// if len(peers) > 0 {
						// 	return
						// }
					}
				}
			}),
		).Await(ctx)
		if err != nil {
			startStatus.TriggerStartFailure(err)
			return
		}
		startStatus.TriggerStart()
	}()

	go func() {
		for {
			msg, err := sub.Next(ctx)
			if err != nil {
				fmt.Println("Error in subscription:", err)
				res.Close()
				return
			}

			// TODO isn't this already run internally by libp2p?
			// if !service.ValidateMessage(ctx, msg.GetFrom(), msg) {
			// 	continue
			// }

			go func() {
				parsedMsg, err := service.ParseMessage(msg.GetData())
				if err != nil {
					//TODO handle error
					return
				}

				go func() {
					err := service.HandleRawMessage(ctx, msg, res.Send)
					if err != nil {
						//TODO handle error
						return
					}
				}()

				go func() {
					err := service.HandleMessage(ctx, msg.GetFrom(), parsedMsg, res.Send)
					if err != nil {
						//TODO handle error
						fmt.Println("pubsub handling error:", err)
						return
					}
				}()
			}()
		}
	}()

	return res, nil
}
