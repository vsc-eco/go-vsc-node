package p2p

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"slices"
	"time"

	"github.com/go-playground/validator/v10"
)

const (
	MsgBtcChainRelay MsgCode = iota
	MsgPriceBroadcast
	MsgPriceBlock
	MsgPriceSignature
)

var priceValidator = validator.New(validator.WithRequiredStructEnabled())

type MsgCode int

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
	Price         float64 `json:"average_price"             validate:"required,gt=0.0"`
	Volume        float64 `json:"average_volume"            validate:"required,gt=0.0"`
	UnixTimeStamp int64   `json:"unix_time_stamp,omitempty" validate:"required,gt=0"`
}

func MakeAveragePricePoint(
	price, volume float64,
) AveragePricePoint {
	now := time.Now().UTC().Unix()
	return AveragePricePoint{
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
	Hash       string    `json:"hash,omitempty"       validate:"hexadecimal"`
	Height     int64     `json:"height,omitempty"`
	PrevBlock  string    `json:"prev_block,omitempty" validate:"hexadecimal"`
	MerkleRoot string    `json:"mrkl_root,omitempty"  validate:"hexadecimal"`
	Timestamp  time.Time `json:"time,omitempty"`
	Fees       int64     `json:"fees,omitempty"`
}

type OracleBlock struct {
	ID            string          `json:"id"             validate:"hexadecimal"`
	BlockProducer string          `json:"block_producer"`
	Signatures    []string        `json:"signatures"`
	Data          json.RawMessage `json:"data"`
	TimeStamp     time.Time       `json:"timestamp"`
}

// VSCBlock.ID construction:
// hex(timestamp + sha256.Sum256(username + activeKey + json(data))).
// results in 40 byte ID (80 char hex string).
func MakeVscBlock(username, activeKey string, data any) (*OracleBlock, error) {
	timestamp := time.Now().UTC()

	tsBuf := [8]byte{}
	binary.BigEndian.PutUint64(tsBuf[:], uint64(timestamp.UnixMilli()))

	buf := bytes.NewBuffer(
		slices.Concat(
			[]byte(username),
			[]byte(activeKey),
		),
	)
	if err := json.NewEncoder(buf).Encode(data); err != nil {
		return nil, err
	}

	idParts := sha256.Sum256(buf.Bytes())
	idBytes := slices.Concat(tsBuf[:], idParts[:])

	jbytes, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}

	block := &OracleBlock{
		ID:            hex.EncodeToString(idBytes),
		BlockProducer: username,
		Signatures:    []string{},
		Data:          jbytes,
		TimeStamp:     timestamp,
	}

	return block, nil
}
