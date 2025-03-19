package logger

import "fmt"

//Future note: logging should support smart contract debug logs which can be stored or processed by the developer

type Logger interface {
	Debug(log ...any)
	Error(log ...any)
}

type PrefixedLogger struct {
	Prefix string
}

func (pl PrefixedLogger) Debug(log ...any) {
	fmt.Println("[Prefix: "+pl.Prefix+"] Debug:", log)
}

func (pl PrefixedLogger) Error(log ...any) {
	fmt.Println("[Prefix: "+pl.Prefix+"] Error: ", log)
}

var _ Logger = &PrefixedLogger{}
