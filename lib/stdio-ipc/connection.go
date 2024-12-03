package stdio_ipc

import "io"

type Connection[IncomingMessage any, OutgoingMessage any] interface {
	Send(OutgoingMessage) error
	Receive(*IncomingMessage) error
	Finished() bool
	io.Closer
}
