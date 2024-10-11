// https://github.com/brianolson/cbor_go/blob/master/cbor.go#L166
package cbor

import (
	"fmt"
	"io"
	"log"
	"math"
	"math/big"
	"reflect"
	"slices"
	"strings"
)

var typeMask byte = 0xE0
var infoBits byte = 0x1F

/* type values */
var cborUint byte = 0x00
var cborNegint byte = 0x20
var cborBytes byte = 0x40
var cborText byte = 0x60
var cborArray byte = 0x80
var cborMap byte = 0xA0
var cborTag byte = 0xC0
var cbor7 byte = 0xE0

/* cbor7 values */
const (
	cborFalse byte = 20
	cborTrue  byte = 21
	cborNull  byte = 22
)

/* info bits */
var int8Follows byte = 24
var int16Follows byte = 25
var int32Follows byte = 26
var int64Follows byte = 27
var varFollows byte = 31

/* tag values */
var tagBignum uint64 = 2
var tagNegBignum uint64 = 3
var tagDecimal uint64 = 4
var tagBigfloat uint64 = 5

type TagDecoder interface {
	// Handle things which match this.
	//
	// Setup like this:
	// var dec Decoder
	// var myTagDec TagDecoder
	// dec.TagDecoders[myTagDec.GetTag()] = myTagDec
	GetTag() uint64

	// Sub-object will be decoded onto the returned object.
	DecodeTarget() interface{}

	// Run after decode onto DecodeTarget has happened.
	// The return value from this is returned in place of the
	// raw decoded object.
	PostDecode(interface{}) (interface{}, error)
}

type Decoder struct {
	rin io.Reader

	// tag byte
	c []byte

	// many values fit within the next 8 bytes
	b8 []byte

	internalVisitors []Visitor
	userVisitor      Visitor

	// Extra processing for CBOR TAG objects.
	TagDecoders map[uint64]TagDecoder
}

// {tx: {op: "myOp"}} -> []string{"tx", "op"}
type Visitor struct {
	IntVisitor     func(path []string, val *big.Int) error
	NilVisitor     func(path []string) error
	BoolVisitor    func(path []string, val bool) error
	BytesVisitor   func(path []string, val []byte) error
	StringVisitor  func(path []string, val string) error
	Float32Visitor func(path []string, val float32) error
	Float64Visitor func(path []string, val float64) error
}

func NewStringCollector(collected func(path []string, val string) error) Visitor {
	var currentPath []string = nil
	buf := ""
	return Visitor{
		StringVisitor: func(path []string, val string) error {
			if currentPath != nil && !slices.Equal(path, currentPath) {
				collected(currentPath, buf)
				currentPath = nil
				buf = val
			} else {
				buf += val
				currentPath = path
			}
			return nil
		},
	}
}

func NewBytesCollector(collected func(path []string, val []byte) error) Visitor {
	var currentPath []string = nil
	buf := make([]byte, 0)
	return Visitor{
		BytesVisitor: func(path []string, val []byte) error {
			if currentPath != nil && !slices.Equal(path, currentPath) {
				collected(currentPath, buf)
				currentPath = nil
				buf = val
			} else {
				buf = append(buf, val...)
				currentPath = path
			}
			return nil
		},
	}
}

func JoinVisitors(visitors ...Visitor) Visitor {
	return JoinVisitorsWithSlice(visitors)
}

func JoinVisitorsWithSlice(visitors []Visitor) Visitor {
	return Visitor{
		IntVisitor: func(path []string, val *big.Int) error {
			for _, v := range visitors {
				if v.IntVisitor == nil {
					continue
				}
				err := v.IntVisitor(path, val)
				if err != nil {
					return err
				}
			}
			return nil
		},
		NilVisitor: func(path []string) error {
			for _, v := range visitors {
				if v.NilVisitor == nil {
					continue
				}
				err := v.NilVisitor(path)
				if err != nil {
					return err
				}
			}
			return nil
		},
		BoolVisitor: func(path []string, val bool) error {
			for _, v := range visitors {
				if v.BoolVisitor == nil {
					continue
				}
				err := v.BoolVisitor(path, val)
				if err != nil {
					return err
				}
			}
			return nil
		},
		BytesVisitor: func(path []string, val []byte) error {
			for _, v := range visitors {
				if v.BytesVisitor == nil {
					continue
				}
				err := v.BytesVisitor(path, val)
				if err != nil {
					return err
				}
			}
			return nil
		},
		StringVisitor: func(path []string, val string) error {
			for _, v := range visitors {
				if v.StringVisitor == nil {
					continue
				}
				err := v.StringVisitor(path, val)
				if err != nil {
					return err
				}
			}
			return nil
		},
		Float32Visitor: func(path []string, val float32) error {
			for _, v := range visitors {
				if v.Float32Visitor == nil {
					continue
				}
				err := v.Float32Visitor(path, val)
				if err != nil {
					return err
				}
			}
			return nil
		},
		Float64Visitor: func(path []string, val float64) error {
			for _, v := range visitors {
				if v.Float64Visitor == nil {
					continue
				}
				err := v.Float64Visitor(path, val)
				if err != nil {
					return err
				}
			}
			return nil
		},
	}
}

var pathRoot = []string{}

func NewDecoder(r io.Reader) *Decoder {
	return &Decoder{
		r,
		make([]byte, 1),
		make([]byte, 8),
		make([]Visitor, 0),
		Visitor{},
		make(map[uint64]TagDecoder),
	}
}
func (dec *Decoder) Decode(v Visitor) error {
	visitorBak := dec.userVisitor
	dec.userVisitor = v
	res := dec.reflectDecode(pathRoot)
	dec.userVisitor = visitorBak
	return res
}
func (dec *Decoder) reflectDecode(path []string) error {
	var didread int
	var err error

	didread, err = io.ReadFull(dec.rin, dec.c)

	if didread == 1 {
		//  log.Printf("got one %x\n", dec.c[0])
	}

	if err != nil {
		return err
	}

	return dec.innerDecodeC(dec.c[0], path)
}

func (dec *Decoder) groupVisitors() Visitor {
	return JoinVisitorsWithSlice(append(dec.internalVisitors, dec.userVisitor))
}

func (dec *Decoder) handleInfoBits(cborInfo byte) (uint64, error) {
	var aux uint64

	if cborInfo <= 23 {
		aux = uint64(cborInfo)
		return aux, nil
	} else if cborInfo == int8Follows {
		didread, err := io.ReadFull(dec.rin, dec.b8[:1])
		if didread == 1 {
			aux = uint64(dec.b8[0])
		}
		return aux, err
	} else if cborInfo == int16Follows {
		didread, err := io.ReadFull(dec.rin, dec.b8[:2])
		if didread == 2 {
			aux = (uint64(dec.b8[0]) << 8) | uint64(dec.b8[1])
		}
		return aux, err
	} else if cborInfo == int32Follows {
		didread, err := io.ReadFull(dec.rin, dec.b8[:4])
		if didread == 4 {
			aux = (uint64(dec.b8[0]) << 24) |
				(uint64(dec.b8[1]) << 16) |
				(uint64(dec.b8[2]) << 8) |
				uint64(dec.b8[3])
		}
		return aux, err
	} else if cborInfo == int64Follows {
		didread, err := io.ReadFull(dec.rin, dec.b8)
		if didread == 8 {
			var shift uint = 56
			i := 0
			aux = uint64(dec.b8[i]) << shift
			for i < 7 {
				i += 1
				shift -= 8
				aux |= uint64(dec.b8[i]) << shift
			}
		}
		return aux, err
	}
	return 0, nil
}

func (dec *Decoder) innerDecodeC(c byte, path []string) error {
	cborType := c & typeMask
	cborInfo := c & infoBits

	aux, err := dec.handleInfoBits(cborInfo)
	if err != nil {
		log.Printf("error in handleInfoBits: %v", err)
		return err
	}
	//log.Printf("cborType %x cborInfo %d aux %x", cborType, cborInfo, aux)

	if cborType == cborUint {
		return dec.groupVisitors().IntVisitor(path, new(big.Int).SetUint64(aux))
	} else if cborType == cborNegint {
		bigU := &big.Int{}
		bigU.SetUint64(aux)
		minusOne := big.NewInt(-1)
		bn := &big.Int{}
		bn.Sub(minusOne, bigU)
		return dec.groupVisitors().IntVisitor(path, bn)
	} else if cborType == cborBytes {
		//log.Printf("cborType %x bytes cborInfo %d aux %x", cborType, cborInfo, aux)
		if cborInfo == varFollows {
			subc := []byte{0}
			for {
				_, err = io.ReadFull(dec.rin, subc)
				if err != nil {
					log.Printf("error reading next byte for bar bytes")
					return err
				}
				if subc[0] == 0xff {
					// done
					return dec.groupVisitors().BytesVisitor([]string{"[bytes-end]"}, []byte{})
				} else {
					if (subc[0] & typeMask) != cborBytes {
						return fmt.Errorf("sub of var bytes is type %x, wanted %x", subc[0], cborBytes)
					}
					err = dec.innerDecodeC(subc[0], path) // TODO get result [probably not here]
					if err != nil {
						log.Printf("error decoding sub bytes")
						return err
					}
				}
			}
		} else {
			val := make([]byte, aux)
			_, err = io.ReadFull(dec.rin, val)
			if err != nil {
				return err
			}
			err = dec.groupVisitors().BytesVisitor(path, val)
			if err != nil {
				return err
			}
			return dec.groupVisitors().BytesVisitor([]string{"[bytes-end]"}, []byte{})
		}
	} else if cborType == cborText {
		return dec.decodeText(cborInfo, aux, path)
	} else if cborType == cborArray {
		return dec.decodeArray(cborInfo, aux, path)
	} else if cborType == cborMap {
		return dec.decodeMap(cborInfo, aux, path)
	} else if cborType == cborTag {
		/*var innerOb interface{}*/
		ic := []byte{0}
		_, err = io.ReadFull(dec.rin, ic)
		if err != nil {
			return err
		}
		if aux == tagBignum {
			bn, err := dec.decodeBignum(ic[0])
			if err != nil {
				return err
			}
			return dec.groupVisitors().IntVisitor(path, bn)
		} else if aux == tagNegBignum {
			bn, err := dec.decodeBignum(ic[0])
			if err != nil {
				return err
			}
			minusOne := big.NewInt(-1)
			bnOut := &big.Int{}
			bnOut.Sub(minusOne, bn)
			return dec.groupVisitors().IntVisitor(path, bnOut)
		} else if aux == tagDecimal {
			log.Printf("TODO: directly read bytes into decimal")
		} else if aux == tagBigfloat {
			log.Printf("TODO: directly read bytes into bigfloat")
		} else { // TODO dubious we need this fix if needed
			decoder, ok := dec.TagDecoders[aux]
			if ok {
				target := decoder.DecodeTarget()
				err = dec.innerDecodeC(ic[0], path) // TODO get result
				if err != nil {
					return err
				}
				target, err = decoder.PostDecode(target)
				if err != nil {
					return err
				}
				// value = target
				return nil
			} else {
				target := CBORTag{}
				target.Tag = aux
				err = dec.innerDecodeC(ic[0], path) // TODO get result
				if err != nil {
					return err
				}
				// value = target
				return nil
			}
		}
		return nil
	} else if cborType == cbor7 {
		if cborInfo == int16Follows {
			exp := (aux >> 10) & 0x01f
			mant := aux & 0x03ff
			var val float64
			if exp == 0 {
				val = math.Ldexp(float64(mant), -24)
			} else if exp != 31 {
				val = math.Ldexp(float64(mant+1024), int(exp-25))
			} else if mant == 0 {
				val = math.Inf(1)
			} else {
				val = math.NaN()
			}
			if (aux & 0x08000) != 0 {
				val = -val
			}
			return dec.groupVisitors().Float64Visitor(path, val)
		} else if cborInfo == int32Follows {
			value := math.Float32frombits(uint32(aux))
			return dec.groupVisitors().Float32Visitor(path, value)
		} else if cborInfo == int64Follows {
			value := math.Float64frombits(aux)
			return dec.groupVisitors().Float64Visitor(path, value)
		} else if cborInfo == cborFalse {
			return dec.groupVisitors().BoolVisitor(path, false)
		} else if cborInfo == cborTrue {
			return dec.groupVisitors().BoolVisitor(path, true)
		} else if cborInfo == cborNull {
			return dec.groupVisitors().NilVisitor(path)
		}
	}

	return err
}

func (dec *Decoder) decodeText(cborInfo byte, aux uint64, path []string) error {
	var err error
	if cborInfo == varFollows {
		subc := []byte{0}
		for {
			_, err = io.ReadFull(dec.rin, subc)
			if err != nil {
				log.Printf("error reading next byte for var text")
				return err
			}
			if subc[0] == 0xff {
				// done
				return dec.groupVisitors().StringVisitor([]string{"[string-end]"}, "")
			} else {
				err = dec.innerDecodeC(subc[0], path) // TODO get result
				if err != nil {
					log.Printf("error decoding subtext")
					return err
				}
			}
		}
	} else {
		raw := make([]byte, aux)
		_, err = io.ReadFull(dec.rin, raw)
		if err != nil {
			return err
		}
		xs := string(raw)
		err = dec.groupVisitors().StringVisitor(path, xs)
		if err != nil {
			return err
		}
		return dec.groupVisitors().StringVisitor([]string{"[string-end]"}, "")
	}
}

func (dec *Decoder) setMapKV(key string, path []string) error {
	// parses map value for key
	err := dec.reflectDecode(append(path, key)) // TODO get result
	if err != nil {
		return err
	}

	return nil
}

func (dec *Decoder) decodeMap(cborInfo byte, aux uint64, path []string) error {
	var err error

	if cborInfo == varFollows {
		subc := []byte{0}
		for {
			_, err = io.ReadFull(dec.rin, subc)
			if err != nil {
				log.Printf("error reading next byte for var text")
				return err
			}
			if subc[0] == 0xff {
				// Done
				return nil
			} else {
				var key string
				vBak := dec.internalVisitors
				uBak := dec.userVisitor
				dec.userVisitor = Visitor{}
				dec.internalVisitors = append(dec.internalVisitors, NewStringCollector(func(foundPath []string, val string) error {
					if !slices.Equal(path, foundPath) {
						return fmt.Errorf("not same paths")
					}
					key = val
					return nil
				}))
				// parses a map key
				err = dec.innerDecodeC(subc[0], path) // TODO get result
				dec.internalVisitors = vBak
				dec.userVisitor = uBak
				if err != nil {
					log.Printf("error decoding map key V, %s", err)
					return err
				}

				err = dec.setMapKV(key, path)
				if err != nil {
					return err
				}
			}
		}
	} else {
		var i uint64
		for i = 0; i < aux; i++ {
			var key string
			vBak := dec.internalVisitors
			uBak := dec.userVisitor
			dec.userVisitor = Visitor{}
			dec.internalVisitors = append(dec.internalVisitors, NewStringCollector(func(foundPath []string, val string) error {
				if !slices.Equal(path, foundPath) {
					return fmt.Errorf("not same paths")
				}
				key = val
				return nil
			}))
			// parses a map key
			err = dec.reflectDecode(path) // TODO get result
			dec.internalVisitors = vBak
			dec.userVisitor = uBak
			if err != nil {
				log.Printf("error decoding map key #, %s", err)
				return err
			}
			err = dec.setMapKV(key, path)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

func (dec *Decoder) decodeArray(cborInfo byte, aux uint64, path []string) error {
	var err error

	if cborInfo == varFollows {
		var arrayPos int = 0
		//log.Printf("var array")
		subc := []byte{0}
		for {
			_, err = io.ReadFull(dec.rin, subc)
			if err != nil {
				log.Printf("error reading next byte for var text")
				return err
			}
			if subc[0] == 0xff {
				// Done
				break
			} else {
				err := dec.innerDecodeC(subc[0], append(path, fmt.Sprintf("[%d]", arrayPos))) // TODO get result
				if err != nil {
					log.Printf("error decoding array subob")
					return err
				}
				arrayPos++
			}
		}
	} else {
		var i uint64
		for i = 0; i < aux; i++ {
			err := dec.reflectDecode(append(path, fmt.Sprintf("[%d]", i))) // TODO get result
			if err != nil {
				log.Printf("error decoding array subob")
				return err
			}
		}
	}

	return nil
}

func (dec *Decoder) decodeBignum(c byte) (*big.Int, error) {
	cborType := c & typeMask
	cborInfo := c & infoBits

	aux, err := dec.handleInfoBits(cborInfo)
	if err != nil {
		log.Printf("error in bignum handleInfoBits: %v", err)
		return nil, err
	}
	//log.Printf("bignum cborType %x cborInfo %d aux %x", cborType, cborInfo, aux)

	if cborType != cborBytes {
		return nil, fmt.Errorf("attempting to decode bignum but sub object is not bytes but type %x", cborType)
	}

	rawbytes := make([]byte, aux)
	_, err = io.ReadFull(dec.rin, rawbytes)
	if err != nil {
		return nil, err
	}

	bn := big.NewInt(0)
	littleBig := &big.Int{}
	d := &big.Int{}
	for _, bv := range rawbytes {
		d.Lsh(bn, 8)
		littleBig.SetUint64(uint64(bv))
		bn.Or(d, littleBig)
	}

	return bn, nil
}

// copied from encoding/json/decode.go
// An InvalidUnmarshalError describes an invalid argument passed to Unmarshal.
// (The argument to Unmarshal must be a non-nil pointer.)
type InvalidUnmarshalError struct {
	Type reflect.Type
}

func (e *InvalidUnmarshalError) Error() string {
	if e.Type == nil {
		return "json: Unmarshal(nil)"
	}

	if e.Type.Kind() != reflect.Ptr {
		return "json: Unmarshal(non-pointer " + e.Type.String() + ")"
	}
	return "json: Unmarshal(nil " + e.Type.String() + ")"
}

type CBORTag struct {
	Tag           uint64
	WrappedObject interface{}
}

// parse StructField.Tag.Get("json" or "cbor")
func fieldTagName(xinfo string) (string, bool) {
	if len(xinfo) != 0 {
		// e.g. `json:"field_name,omitempty"`, or same for cbor
		// TODO: honor 'omitempty' option
		jiparts := strings.Split(xinfo, ",")
		if len(jiparts) > 0 {
			fieldName := jiparts[0]
			if len(fieldName) > 0 {
				return fieldName, true
			}
		}
	}
	return "", false
}

// Return fieldname, bool; if bool is false, don't use this field
func fieldname(fieldinfo reflect.StructField) (string, bool) {
	if fieldinfo.PkgPath != "" {
		// has path to private package. don't export
		return "", false
	}
	fieldname, ok := fieldTagName(fieldinfo.Tag.Get("cbor"))
	if !ok {
		fieldname, ok = fieldTagName(fieldinfo.Tag.Get("json"))
	}
	if ok {
		if fieldname == "" {
			return fieldinfo.Name, true
		}
		if fieldname == "-" {
			return "", false
		}
		return fieldname, true
	}
	return fieldinfo.Name, true
}
