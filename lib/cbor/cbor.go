// https://github.com/brianolson/cbor_go/blob/master/cbor.go#L166
package cbor

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"math/big"
	"reflect"
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

// TODO: honor encoding.BinaryMarshaler interface and encapsulate blob returned from that.

// Load one object into v
func Loads(blob []byte, v interface{}) error {
	dec := NewDecoder(bytes.NewReader(blob))
	return dec.Decode(v)
}

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

	// Extra processing for CBOR TAG objects.
	TagDecoders map[uint64]TagDecoder
}

func NewDecoder(r io.Reader) *Decoder {
	return &Decoder{
		r,
		make([]byte, 1),
		make([]byte, 8),
		make(map[uint64]TagDecoder),
	}
}
func (dec *Decoder) Decode(v interface{}) error {
	return dec.reflectDecode()
}
func (dec *Decoder) reflectDecode() error {
	var didread int
	var err error

	didread, err = io.ReadFull(dec.rin, dec.c)

	if didread == 1 {
		//  log.Printf("got one %x\n", dec.c[0])
	}

	if err != nil {
		return err
	}

	return dec.innerDecodeC(dec.c[0])
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

func (dec *Decoder) innerDecodeC(c byte) error {
	cborType := c & typeMask
	cborInfo := c & infoBits

	aux, err := dec.handleInfoBits(cborInfo)
	if err != nil {
		log.Printf("error in handleInfoBits: %v", err)
		return err
	}
	//log.Printf("cborType %x cborInfo %d aux %x", cborType, cborInfo, aux)

	if cborType == cborUint {
		// value = aux
		return nil
	} else if cborType == cborNegint {
		if aux > 0x7fffffffffffffff {
			bigU := &big.Int{}
			bigU.SetUint64(aux)
			minusOne := big.NewInt(-1)
			bn := &big.Int{}
			bn.Sub(minusOne, bigU)
			// value = bn
			return nil
		}
		// value = -1-int64(aux)
		return nil
	} else if cborType == cborBytes {
		//log.Printf("cborType %x bytes cborInfo %d aux %x", cborType, cborInfo, aux)
		if cborInfo == varFollows {
			parts := make([][]byte, 0, 1)
			allsize := 0
			subc = []byte{0}
			for true {
				_, err = io.ReadFull(dec.rin, subc)
				if err != nil {
					log.Printf("error reading next byte for bar bytes")
					return err
				}
				if subc[0] == 0xff {
					// done
					var out []byte = nil
					if len(parts) == 0 {
						out = make([]byte, 0)
					} else {
						pos := 0
						out = make([]byte, allsize)
						for _, p := range parts {
							pos += copy(out[pos:], p)
						}
					}
					// value = out
					return nil
				} else {
					var subb []byte = nil
					if (subc[0] & typeMask) != cborBytes {
						return fmt.Errorf("sub of var bytes is type %x, wanted %x", subc[0], cborBytes)
					}
					err = dec.innerDecodeC(subc[0]) // TODO get result
					if err != nil {
						log.Printf("error decoding sub bytes")
						return err
					}
					allsize += len(subb)
					parts = append(parts, subb)
				}
			}
		} else {
			val := make([]byte, aux)
			_, err = io.ReadFull(dec.rin, val)
			if err != nil {
				return err
			}
			// value = val
			return nil
		}
	} else if cborType == cborText {
		return dec.decodeText(cborInfo, aux)
	} else if cborType == cborArray {
		// TODO start here
		return dec.decodeArray(rv, cborInfo, aux)
	} else if cborType == cborMap {
		return dec.decodeMap(rv, cborInfo, aux)
		// TODO finish here
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
			// value = bn
			return nil
		} else if aux == tagNegBignum {
			bn, err := dec.decodeBignum(ic[0])
			if err != nil {
				return err
			}
			minusOne := big.NewInt(-1)
			bnOut := &big.Int{}
			bnOut.Sub(minusOne, bn)
			// value = bnOut
			return nil
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
				// value = math.Ldexp(float64(mant+1024), int(exp-25))
				return nil
			} else if mant == 0 {
				// value = math.Inf(-1)
				return nil
			} else {
				// value = math.NaN()
				return nil
			}
			if (aux & 0x08000) != 0 {
				// value = value * -1
				return nil
			}
			return setFloat64(rv, val)
		} else if cborInfo == int32Follows {
			// value = math.Float32frombits(uint32(aux))
			return nil
		} else if cborInfo == int64Follows {
			// value = math.Float64frombits(aux)
			return nil
		} else if cborInfo == cborFalse {
			// value = false
			return nil
		} else if cborInfo == cborTrue {
			// value = true
			return nil
		} else if cborInfo == cborNull {
			// value = nil
			// ABC
			return nil
		}
	}

	return err
}

func (dec *Decoder) decodeText(cborInfo byte, aux uint64) error {
	var err error
	if cborInfo == varFollows {
		parts := make([]string, 0, 1)
		subc := []byte{0}
		for {
			_, err = io.ReadFull(dec.rin, subc)
			if err != nil {
				log.Printf("error reading next byte for var text")
				return err
			}
			if subc[0] == 0xff {
				// done
				joined := strings.Join(parts, "")
				// value = joined
				return nil
			} else {
				var subtext interface{}
				err = dec.innerDecodeC(subc[0]) // TODO get result
				if err != nil {
					log.Printf("error decoding subtext")
					return err
				}
				st, ok := subtext.(string)
				if ok {
					parts = append(parts, st)
				} else {
					return fmt.Errorf("var text sub element not text but %T", subtext)
				}
			}
		}
	} else {
		raw := make([]byte, aux)
		_, err = io.ReadFull(dec.rin, raw)
		xs := string(raw)
		// value = xs
		return nil
	}
}

type mapAssignable interface {
	ReflectValueForKey(key interface{}) (*reflect.Value, bool)
	SetReflectValueForKey(key interface{}, value reflect.Value) error
}

type mapReflectValue struct {
	reflect.Value
}

func (irv *mapReflectValue) ReflectValueForKey(key interface{}) (*reflect.Value, bool) {
	//var x interface{}
	//rv := reflect.ValueOf(&x)
	rv := reflect.New(irv.Type().Elem())
	return &rv, true
}
func (irv *mapReflectValue) SetReflectValueForKey(key interface{}, value reflect.Value) error {
	//log.Printf("k T %T v%#v, v T %s v %#v", key, key, value.Type().String(), value.Interface())
	krv := reflect.Indirect(reflect.ValueOf(key))
	vrv := reflect.Indirect(value)
	//log.Printf("irv T %s v %#v", irv.Type().String(), irv.Interface())
	//log.Printf("k T %s v %#v, v T %s v %#v", krv.Type().String(), krv.Interface(), vrv.Type().String(), vrv.Interface())
	if krv.Kind() == reflect.Interface {
		krv = krv.Elem()
		//log.Printf("ke T %s v %#v", krv.Type().String(), krv.Interface())
	}
	if (krv.Kind() == reflect.Slice) || (krv.Kind() == reflect.Array) {
		//log.Printf("key is slice or array")
		if krv.Type().Elem().Kind() == reflect.Uint8 {
			//log.Printf("key is []uint8")
			ks := string(krv.Bytes())
			krv = reflect.ValueOf(ks)
		}
	}
	irv.SetMapIndex(krv, vrv)
	return nil
}

type structAssigner struct {
	Srv reflect.Value

	//keyType reflect.Type
}

func (sa *structAssigner) ReflectValueForKey(key interface{}) (*reflect.Value, bool) {
	var skey string
	switch tkey := key.(type) {
	case string:
		skey = tkey
	case *string:
		skey = *tkey
	default:
		log.Printf("rvfk key is not string, got %T", key)
		return nil, false
	}

	ft := sa.Srv.Type()
	numFields := ft.NumField()
	for i := 0; i < numFields; i++ {
		sf := ft.Field(i)
		fieldname, ok := fieldname(sf)
		if !ok {
			continue
		}
		if (fieldname == skey) || strings.EqualFold(fieldname, skey) {
			fieldVal := sa.Srv.FieldByName(sf.Name)
			if !fieldVal.CanSet() {
				log.Printf("cannot set field %s for key %s", sf.Name, skey)
				return nil, false
			}
			return &fieldVal, true
		}
	}
	return nil, false
}
func (sa *structAssigner) SetReflectValueForKey(key interface{}, value reflect.Value) error {
	return nil
}

func (dec *Decoder) setMapKV(krv reflect.Value, ma mapAssignable) error {
	var err error
	val, ok := ma.ReflectValueForKey(krv.Interface())
	if !ok {
		var throwaway interface{}
		err = dec.Decode(&throwaway)
		if err != nil {
			return err
		}
		return nil
	}
	err = dec.reflectDecode(*val)
	if err != nil {
		log.Printf("error decoding map val: T %T v %#v v.T %s", val, val, val.Type().String())
		return err
	}
	err = ma.SetReflectValueForKey(krv.Interface(), *val)
	if err != nil {
		log.Printf("error setting value")
		return err
	}

	return nil
}

func (dec *Decoder) decodeMap(rv reflect.Value, cborInfo byte, aux uint64) error {
	//log.Print("decode map into   ", rv.Type().String())
	// dereferenced reflect value
	var drv reflect.Value
	if rv.Kind() == reflect.Ptr {
		drv = reflect.Indirect(rv)
	} else {
		drv = rv
	}
	//log.Print("decode map into d ", drv.Type().String())

	// inner reflect value
	var irv reflect.Value
	var ma mapAssignable

	var keyType reflect.Type

	switch drv.Kind() {
	case reflect.Interface:
		//log.Print("decode map into interface ", drv.Type().String())
		// TODO: maybe I should make this map[string]interface{}
		nob := make(map[interface{}]interface{})
		irv = reflect.ValueOf(nob)
		ma = &mapReflectValue{irv}
		keyType = irv.Type().Key()
	case reflect.Struct:
		//log.Print("decode map into struct ", drv.Type().String())
		ma = &structAssigner{drv}
		keyType = reflect.TypeOf("")
	case reflect.Map:
		//log.Print("decode map into map ", drv.Type().String())
		if drv.IsNil() {
			if drv.CanSet() {
				drv.Set(reflect.MakeMap(drv.Type()))
			} else {
				return fmt.Errorf("target map is nil and not settable")
			}
		}
		keyType = drv.Type().Key()
		ma = &mapReflectValue{drv}
	default:
		return fmt.Errorf("can't read map into %s", rv.Type().String())
	}

	var err error

	if cborInfo == varFollows {
		subc := []byte{0}
		for true {
			_, err = io.ReadFull(dec.rin, subc)
			if err != nil {
				log.Printf("error reading next byte for var text")
				return err
			}
			if subc[0] == 0xff {
				// Done
				break
			} else {
				//var key interface{}
				krv := reflect.New(keyType)
				//var val interface{}
				err = dec.innerDecodeC(krv, subc[0])
				if err != nil {
					log.Printf("error decoding map key V, %s", err)
					return err
				}

				err = dec.setMapKV(krv, ma)
				if err != nil {
					return err
				}
			}
		}
	} else {
		var i uint64
		for i = 0; i < aux; i++ {
			//var key interface{}
			krv := reflect.New(keyType)
			//var val interface{}
			//err = dec.Decode(&key)
			err = dec.reflectDecode(krv)
			if err != nil {
				log.Printf("error decoding map key #, %s", err)
				return err
			}
			err = dec.setMapKV(krv, ma)
			if err != nil {
				return err
			}
		}
	}

	if drv.Kind() == reflect.Interface {
		drv.Set(irv)
	}
	return nil
}

func (dec *Decoder) decodeArray(rv reflect.Value, cborInfo byte, aux uint64) error {
	if rv.Kind() == reflect.Ptr {
		rv = reflect.Indirect(rv)
	}

	var makeLength int = 0
	if cborInfo == varFollows {
		// no special capacity to allocate the slice to
	} else {
		makeLength = int(aux)
	}

	// inner reflect value
	var irv reflect.Value
	var elemType reflect.Type

	switch rv.Kind() {
	case reflect.Interface:
		// make a slice
		nob := make([]interface{}, 0, makeLength)
		irv = reflect.ValueOf(nob)
		elemType = irv.Type().Elem()
	case reflect.Slice:
		// we have a slice
		irv = rv
		elemType = irv.Type().Elem()
	case reflect.Array:
		// no irv, no elemType
	default:
		return fmt.Errorf("can't read array into %s", rv.Type().String())
	}

	var err error

	if cborInfo == varFollows {
		var arrayPos int = 0
		//log.Printf("var array")
		subc := []byte{0}
		for true {
			_, err = io.ReadFull(dec.rin, subc)
			if err != nil {
				log.Printf("error reading next byte for var text")
				return err
			}
			if subc[0] == 0xff {
				// Done
				break
			} else if rv.Kind() == reflect.Array {
				err := dec.innerDecodeC(rv.Index(arrayPos), subc[0])
				if err != nil {
					log.Printf("error decoding array subob")
					return err
				}
				arrayPos++
			} else {
				subrv := reflect.New(elemType)
				err := dec.innerDecodeC(subrv, subc[0])
				if err != nil {
					log.Printf("error decoding array subob")
					return err
				}
				irv = reflect.Append(irv, reflect.Indirect(subrv))
			}
		}
	} else {
		var i uint64
		for i = 0; i < aux; i++ {
			if rv.Kind() == reflect.Array {
				err := dec.reflectDecode(rv.Index(int(i)))
				if err != nil {
					log.Printf("error decoding array subob")
					return err
				}
			} else {
				subrv := reflect.New(elemType)
				err := dec.reflectDecode(subrv)
				if err != nil {
					log.Printf("error decoding array subob")
					return err
				}
				irv = reflect.Append(irv, reflect.Indirect(subrv))
			}
		}
	}

	if rv.Kind() != reflect.Array {
		rv.Set(irv)
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
	default:
		return fmt.Errorf("cannot assign []byte into Kind=%s Type=%s %#v", rv.Kind().String(), "" /*rv.Type().String()*/, rv)
	}
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

type Encoder struct {
	out io.Writer

	scratch []byte
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

// Write out an object to an io.Writer
func Encode(out io.Writer, ob interface{}) error {
	return NewEncoder(out).Encode(ob)
}

// Write out an object to a new byte slice
func Dumps(ob interface{}) ([]byte, error) {
	writeTarget := &bytes.Buffer{}
	writeTarget.Grow(20000)
	err := Encode(writeTarget, ob)
	if err != nil {
		return nil, err
	}
	return writeTarget.Bytes(), nil
}

// Return new Encoder object for writing to supplied io.Writer.
//
// TODO: set options on Encoder object.
func NewEncoder(out io.Writer) *Encoder {
	return &Encoder{out, make([]byte, 9)}
}

func (enc *Encoder) Encode(ob interface{}) error {
	switch x := ob.(type) {
	case int:
		return enc.writeInt(int64(x))
	case int8:
		return enc.writeInt(int64(x))
	case int16:
		return enc.writeInt(int64(x))
	case int32:
		return enc.writeInt(int64(x))
	case int64:
		return enc.writeInt(x)
	case uint:
		return enc.tagAuxOut(cborUint, uint64(x))
	case uint8: /* aka byte */
		return enc.tagAuxOut(cborUint, uint64(x))
	case uint16:
		return enc.tagAuxOut(cborUint, uint64(x))
	case uint32:
		return enc.tagAuxOut(cborUint, uint64(x))
	case uint64:
		return enc.tagAuxOut(cborUint, x)
	case float32:
		return enc.writeFloat(float64(x))
	case float64:
		return enc.writeFloat(x)
	case string:
		return enc.writeText(x)
	case []byte:
		return enc.writeBytes(x)
	case bool:
		return enc.writeBool(x)
	case nil:
		return enc.tagAuxOut(cbor7, uint64(cborNull))
	case big.Int:
		return fmt.Errorf("TODO: encode big.Int")
	}

	// If none of the simple types work, try reflection
	return enc.writeReflection(reflect.ValueOf(ob))
}

func (enc *Encoder) writeReflection(rv reflect.Value) error {
	var err error
	switch rv.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return enc.writeInt(rv.Int())
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return enc.tagAuxOut(cborUint, rv.Uint())
	case reflect.Float32, reflect.Float64:
		return enc.writeFloat(rv.Float())
	case reflect.Bool:
		return enc.writeBool(rv.Bool())
	case reflect.String:
		return enc.writeText(rv.String())
	case reflect.Slice, reflect.Array:
		elemType := rv.Type().Elem()
		if elemType.Kind() == reflect.Uint8 {
			// special case, write out []byte
			return enc.writeBytes(rv.Bytes())
		}
		alen := rv.Len()
		err = enc.tagAuxOut(cborArray, uint64(alen))
		for i := 0; i < alen; i++ {
			err = enc.writeReflection(rv.Index(i))
			if err != nil {
				log.Printf("error at array elem %d", i)
				return err
			}
		}
		return nil
	case reflect.Map:
		err = enc.tagAuxOut(cborMap, uint64(rv.Len()))
		keys := rv.MapKeys()
		for _, krv := range keys {
			vrv := rv.MapIndex(krv)
			err = enc.writeReflection(krv)
			if err != nil {
				log.Printf("error encoding map key")
				return err
			}
			err = enc.writeReflection(vrv)
			if err != nil {
				log.Printf("error encoding map val")
				return err
			}
		}
		return nil
	case reflect.Struct:
		// TODO: check for big.Int ?
		numfields := rv.NumField()
		structType := rv.Type()
		usableFields := 0
		for i := 0; i < numfields; i++ {
			fieldinfo := structType.Field(i)
			_, ok := fieldname(fieldinfo)
			if !ok {
				continue
			}
			usableFields++
		}
		err = enc.tagAuxOut(cborMap, uint64(usableFields))
		if err != nil {
			return err
		}
		for i := 0; i < numfields; i++ {
			fieldinfo := structType.Field(i)
			fieldname, ok := fieldname(fieldinfo)
			if !ok {
				continue
			}
			err = enc.writeText(fieldname)
			if err != nil {
				return err
			}
			err = enc.writeReflection(rv.Field(i))
			if err != nil {
				return err
			}
		}
		return nil
	case reflect.Interface:
		//return fmt.Errorf("TODO: serialize interface{} k=%s T=%s", rv.Kind().String(), rv.Type().String())
		return enc.Encode(rv.Interface())
	case reflect.Ptr:
		if rv.IsNil() {
			return enc.tagAuxOut(cbor7, uint64(cborNull))
		}
		return enc.writeReflection(reflect.Indirect(rv))
	}

	return fmt.Errorf("don't know how to CBOR serialize k=%s t=%s", rv.Kind().String(), rv.Type().String())
}

func (enc *Encoder) writeInt(x int64) error {
	if x < 0 {
		return enc.tagAuxOut(cborNegint, uint64(-1-x))
	}
	return enc.tagAuxOut(cborUint, uint64(x))
}

func (enc *Encoder) tagAuxOut(tag byte, x uint64) error {
	var err error
	if x <= 23 {
		// tiny literal
		enc.scratch[0] = tag | byte(x)
		_, err = enc.out.Write(enc.scratch[:1])
	} else if x < 0x0ff {
		enc.scratch[0] = tag | int8Follows
		enc.scratch[1] = byte(x & 0x0ff)
		_, err = enc.out.Write(enc.scratch[:2])
	} else if x < 0x0ffff {
		enc.scratch[0] = tag | int16Follows
		enc.scratch[1] = byte((x >> 8) & 0x0ff)
		enc.scratch[2] = byte(x & 0x0ff)
		_, err = enc.out.Write(enc.scratch[:3])
	} else if x < 0x0ffffffff {
		enc.scratch[0] = tag | int32Follows
		enc.scratch[1] = byte((x >> 24) & 0x0ff)
		enc.scratch[2] = byte((x >> 16) & 0x0ff)
		enc.scratch[3] = byte((x >> 8) & 0x0ff)
		enc.scratch[4] = byte(x & 0x0ff)
		_, err = enc.out.Write(enc.scratch[:5])
	} else {
		err = enc.tagAux64(tag, x)
	}
	return err
}
func (enc *Encoder) tagAux64(tag byte, x uint64) error {
	enc.scratch[0] = tag | int64Follows
	enc.scratch[1] = byte((x >> 56) & 0x0ff)
	enc.scratch[2] = byte((x >> 48) & 0x0ff)
	enc.scratch[3] = byte((x >> 40) & 0x0ff)
	enc.scratch[4] = byte((x >> 32) & 0x0ff)
	enc.scratch[5] = byte((x >> 24) & 0x0ff)
	enc.scratch[6] = byte((x >> 16) & 0x0ff)
	enc.scratch[7] = byte((x >> 8) & 0x0ff)
	enc.scratch[8] = byte(x & 0x0ff)
	_, err := enc.out.Write(enc.scratch[:9])
	return err
}

func (enc *Encoder) writeText(x string) error {
	enc.tagAuxOut(cborText, uint64(len(x)))
	_, err := io.WriteString(enc.out, x)
	return err
}

func (enc *Encoder) writeBytes(x []byte) error {
	enc.tagAuxOut(cborBytes, uint64(len(x)))
	_, err := enc.out.Write(x)
	return err
}

func (enc *Encoder) writeFloat(x float64) error {
	return enc.tagAux64(cbor7, math.Float64bits(x))
}

func (enc *Encoder) writeBool(x bool) error {
	if x {
		return enc.tagAuxOut(cbor7, uint64(cborTrue))
	} else {
		return enc.tagAuxOut(cbor7, uint64(cborFalse))
	}
}
