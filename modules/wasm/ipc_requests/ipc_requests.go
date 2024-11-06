package ipc_requests

import (
	"github.com/JustinKnueppel/go-result"
	"github.com/moznion/go-optional"
)

type ProcessedMessage[Result any] struct {
	Result   optional.Option[Result]
	Response optional.Option[Message[Result]]
}

type Message[Result any] interface {
	Process() result.Result[ProcessedMessage[Result]]
}
