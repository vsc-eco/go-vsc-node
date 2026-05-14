package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"strconv"
	"strings"
)

// feedPublishOperation implements hivego.HiveOperation for Hive's feed_publish
// (op id 7). Duplicated from cmd/devnet-setup/ops.go — both binaries are
// package main so they can't share via import.
type feedPublishOperation struct {
	Publisher    string `json:"publisher"`
	ExchangeRate struct {
		Base  string `json:"base"`
		Quote string `json:"quote"`
	} `json:"exchange_rate"`
}

func (o feedPublishOperation) OpName() string { return "feed_publish" }

func (o feedPublishOperation) SerializeOp() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte(7) // feed_publish op id
	feedWriteVString(o.Publisher, &buf)
	if err := feedWriteAsset(o.ExchangeRate.Base, &buf); err != nil {
		return nil, errors.New("feed_publish base: " + err.Error())
	}
	if err := feedWriteAsset(o.ExchangeRate.Quote, &buf); err != nil {
		return nil, errors.New("feed_publish quote: " + err.Error())
	}
	return buf.Bytes(), nil
}

func feedWriteVString(s string, b *bytes.Buffer) {
	vbuf := make([]byte, 5)
	n := binary.PutUvarint(vbuf, uint64(len(s)))
	b.Write(vbuf[:n])
	b.WriteString(s)
}

// feedWriteAsset serialises a Hive asset string using the NAIAP format that
// hivego's appendVAsset uses: int64 LE amount || uint32 LE nai (precision
// embedded in the low bits of the NAI per the AppBase encoding).
func feedWriteAsset(asset string, b *bytes.Buffer) error {
	parts := strings.SplitN(asset, " ", 2)
	if len(parts) != 2 {
		return errors.New("invalid asset: " + asset)
	}
	sym := strings.ToUpper(strings.TrimSpace(parts[1]))

	precision := 3
	var nai uint32
	switch sym {
	case "HIVE", "TESTS":
		nai = ((99999999 + 2) << 5) | 3
	case "HBD", "TBD":
		nai = ((99999999 + 1) << 5) | 3
	case "VESTS":
		precision = 6
		nai = ((99999999 + 3) << 5) | 6
	default:
		return errors.New("unsupported asset symbol: " + sym)
	}

	dotParts := strings.SplitN(parts[0], ".", 2)
	dec := ""
	if len(dotParts) == 2 {
		dec = dotParts[1]
	}
	if len(dec) > precision {
		dec = dec[:precision]
	} else {
		dec += strings.Repeat("0", precision-len(dec))
	}

	amount, err := strconv.ParseInt(dotParts[0]+dec, 10, 64)
	if err != nil {
		return errors.New("invalid amount: " + parts[0])
	}

	binary.Write(b, binary.LittleEndian, amount)
	naiBytes := make([]byte, 4)
	binary.LittleEndian.PutUint32(naiBytes, nai)
	b.Write(naiBytes)
	return nil
}
