package sdk

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"vsc-node/modules/db/vsc/contracts"
)

func TestVerifyAddress(t *testing.T) {
	// Change this to the address you want to test
	addr := "contract:vsc1BkWohDf5fPcwn7V9B9ar6TyiWc3A2ZGJ4t"

	fn := SdkNamespaces["system"]["verify_address"].(func(context.Context, any) SdkResult)
	res := fn(context.Background(), addr)

	if res.IsErr() {
		t.Fatalf("verify_address returned error: %v", res.UnwrapErr())
	}
	val := res.Unwrap()
	fmt.Printf("address: %q\nresult:  %q\ngas:     %d\n", addr, val.Result, val.Gas)
}

func TestSystemCallArityMismatchFailsClosed(t *testing.T) {
	fn := SdkNamespaces["system"]["call"].(func(context.Context, any, any) SdkResult)
	res := fn(context.Background(), "hive.transfer", `{"arg0":"anything"}`)
	if res.IsOk() {
		t.Fatalf("expected ARITY_MISMATCH error for non-unary target")
	}
	errText := res.UnwrapErr().Error()
	if !strings.Contains(errText, contracts.SDK_ERROR) || !strings.Contains(errText, "ARITY_MISMATCH") {
		t.Fatalf("unexpected error for arity mismatch: %s", errText)
	}
}

func TestSystemCallRecoversTargetPanic(t *testing.T) {
	prev, had := SdkNamespaces["panic_test"]
	defer func() {
		if had {
			SdkNamespaces["panic_test"] = prev
		} else {
			delete(SdkNamespaces, "panic_test")
		}
	}()

	SdkNamespaces["panic_test"] = map[string]sdkFunc{
		"boom": func(context.Context, any) SdkResult {
			panic("boom")
		},
	}

	fn := SdkNamespaces["system"]["call"].(func(context.Context, any, any) SdkResult)
	res := fn(context.Background(), "panic_test.boom", `{"arg0":"x"}`)
	if res.IsOk() {
		t.Fatalf("expected SYSTEM_CALL_PANIC error, got success")
	}
	errText := res.UnwrapErr().Error()
	if !strings.Contains(errText, contracts.SDK_ERROR) || !strings.Contains(errText, "SYSTEM_CALL_PANIC") {
		t.Fatalf("unexpected error for recovered panic: %s", errText)
	}
}
