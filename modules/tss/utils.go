package tss

import (
	"encoding/base64"
	"strconv"
	"strings"

	"github.com/davidlazar/go-crypto/encoding/base32"
	"github.com/ipfs/go-datastore"
)

func makeKey(t string, id string, epochTag ...int) datastore.Key {
	keyId := t + "-" + base64.RawURLEncoding.EncodeToString([]byte(id))
	if len(epochTag) > 0 {
		keyId = keyId + strconv.Itoa(epochTag[0])
	}
	k1 := base32.EncodeToString([]byte(keyId))
	k := datastore.NewKey(strings.ToUpper(k1))

	return k
}

func makeEpochIdx(epoch int) int {
	epochIdx := epoch % 100
	if epochIdx == 0 {
		epochIdx++
	}
	return epochIdx
}
