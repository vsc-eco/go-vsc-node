package p2p

import (
	"encoding/json"

	"github.com/go-playground/validator/v10"
)

const (
	MsgBtcChainRelay MsgType = iota
	MsgPriceOracleBroadcast
	MsgPriceOracleNewBlock
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
	// length: range from 1-9 chars.
	// format: uppercase letters, may include numbers.
	Symbol string  `json:"symbol"         validate:"required,min=1,max=9,alphanum"` // no need to validate
	Price  float64 `json:"average_price"  validate:"required,gt=0.0"`
	// MedianPrice   float64 `json:"median_price"              validate:"required,gt=0.0"`
	Volume float64 `json:"average_volume" validate:"required,gt=0.0"`
	// UnixTimeStamp int64   `json:"unix_time_stamp,omitempty" validate:"required,gt=0"`
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
	Signatures []string `json:"signatures"`
	Data       string   `json:"data"       validate:"json"`
}
