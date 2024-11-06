package protocol

import "github.com/JustinKnueppel/go-result"

type Protocol struct {
	ProposeBlock func(id string) (res result.Result[any])
	SignMultiSig func()
}
