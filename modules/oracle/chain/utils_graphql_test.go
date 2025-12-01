package chain

import (
	"fmt"
	"log"
	"testing"
)

func TestNonceQuery(t *testing.T) {
	nonce, err := getAccountNonce("btc")
	if err != nil {
		log.Fatalln(err.Error())
	}
	fmt.Println(nonce)
}
