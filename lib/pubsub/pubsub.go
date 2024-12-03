package pubsub

type PubSub[Peer any] interface {
	Subscribe(topic string, handler func([]byte))
	Unsubscribe(topic string, handler func([]byte))

	SendToAll(topic string, message []byte)
	SendTo(topic string, message []byte, recipients []Peer)

	Peers() []Peer
}
