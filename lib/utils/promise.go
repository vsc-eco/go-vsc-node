package utils

import (
	"time"

	"github.com/chebyrash/promise"
)

func Sleep(ts time.Duration) *promise.Promise[struct{}] {
	return promise.New(func(resolve func(struct{}), reject func(error)) {
		time.AfterFunc(ts, func() {
			resolve(struct{}{})
		})
	})
}

func PromiseResolve[T any](val T) *promise.Promise[T] {
	return promise.New(func(resolve func(T), reject func(error)) {
		resolve(val)
	})
}

func PromiseReject[T any](val error) *promise.Promise[T] {
	return promise.New(func(resolve func(T), reject func(error)) {
		reject(val)
	})
}
