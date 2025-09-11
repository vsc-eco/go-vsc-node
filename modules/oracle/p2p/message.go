package p2p

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"io"
	"strings"
	"time"

	"github.com/go-playground/validator/v10"
)

const (
	MsgBtcChainRelay MsgType = iota
	MsgPriceOracleBroadcast
	MsgPriceOracleNewBlock
	MsgPriceOracleSignature
	MsgPriceOracleSignedBlock
)

var priceValidator = validator.New(validator.WithRequiredStructEnabled())

type MsgType int

type ObservePricePoint struct {
	Symbol string  `json:"symbol,omitempty"`
	Price  float64 `json:"price,omitempty"`
	Volume float64 `json:"volume,omitempty"`
}

func (o *ObservePricePoint) String() string {
	jbytes, _ := json.MarshalIndent(o, "", "  ")
	return string(jbytes)
}

type AveragePricePoint struct {
	Symbol        string  `json:"symbol"                    validate:"required,min=1,max=9,alphanum"` // no need to validate
	Price         float64 `json:"average_price"             validate:"required,gt=0.0"`
	Volume        float64 `json:"average_volume"            validate:"required,gt=0.0"`
	UnixTimeStamp int64   `json:"unix_time_stamp,omitempty" validate:"required,gt=0"`
}

func MakeAveragePricePoint(
	symbol string,
	price, volume float64,
) AveragePricePoint {
	now := time.Now().UTC().Unix()
	return AveragePricePoint{
		Symbol:        strings.ToUpper(symbol),
		Price:         price,
		Volume:        volume,
		UnixTimeStamp: now,
	}
}

// UnmarshalJSON implements json.Unmarshaler
func (p *AveragePricePoint) UnmarshalJSON(data []byte) error {
	type alias *AveragePricePoint
	buf := (alias)(p)

	if err := json.Unmarshal(data, buf); err != nil {
		return err
	}

	return priceValidator.Struct(p)
}

// https://www.blockcypher.com/dev/bitcoin/#block
type BlockRelay struct {
	Hash       string `json:"hash,omitempty"       validate:"hexadecimal"`
	Height     uint32 `json:"height,omitempty"`
	PrevBlock  string `json:"prev_block,omitempty" validate:"hexadecimal"`
	MerkleRoot string `json:"mrkl_root,omitempty"  validate:"hexadecimal"`
	Timestamp  string `json:"time,omitempty"`
	Fees       uint32 `json:"fees,omitempty"`
}

type VSCBlock struct {
	ID         string   `json:"id"         validate:"hexadecimal"`
	Signatures []string `json:"signatures"`
	Data       any      `json:"data"`
	// in ms
	TimeStamp int64 `json:"timestamp"`
}

func MakeVscBlock(data any) (*VSCBlock, error) {
	// block id: 8 byte timestamp for UTC ms, followed 8 random bytes
	idBuf := [16]byte{}

	ts := uint64(time.Now().UTC().UnixMilli())
	binary.BigEndian.PutUint64(idBuf[:8], ts)

	_, err := io.ReadFull(rand.Reader, idBuf[8:])
	if err != nil {
		return nil, err
	}

	block := &VSCBlock{
		ID:         hex.EncodeToString(idBuf[:]),
		Signatures: []string{},
		Data:       data,
		TimeStamp:  time.Now().UTC().UnixMilli(),
	}

	return block, nil
}
