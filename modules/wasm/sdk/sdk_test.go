package sdk

import (
	"context"
	"fmt"
	"testing"
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
