package did

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"

	"github.com/btcsuite/btcutil/base58"

	"golang.org/x/crypto/curve25519"
	"golang.org/x/crypto/ed25519"
	"golang.org/x/crypto/nacl/box"
)

var (
	ErrInvalidDID        = errors.New("invalid DID")
	ErrInvalidPubKeySize = errors.New("invalid public key size")
	ErrDecryptionFailed  = errors.New("decryption failed")
	ErrInvalidSignature  = errors.New("invalid signature")
	ErrInvalidEncoding   = errors.New("invalid encoding")
	ErrSerialization     = errors.New("serialization error")
	ErrDeserialization   = errors.New("deserialization error")
)

// decentralized identifier (string wrapper type)
//
// takes the form of "did:key:<base58-encoded ed25519 public key>"
type DID string

// returns the inner string of the DID
func (d DID) String() string {
	return string(d)
}

// a DID provider that uses the ed25519 signature algorithm
type Ed25519Provider struct {
	pubKey  ed25519.PublicKey
	privKey ed25519.PrivateKey // no public "getter" for priv key
	did     DID
}

// getter for the DID
func (p *Ed25519Provider) DID() DID {
	return p.did
}

// getter for the public key
func (p *Ed25519Provider) PublicKey() ed25519.PublicKey {
	return p.pubKey
}

// creates a new DID provider using the seed as the private key
//
// takes a 32-byte high-entropy seed
func NewEd25519Provider(seed [ed25519.SeedSize]byte) (*Ed25519Provider, error) {

	// use a random high-entropy seed to gen a new ed25519 key pair
	pubKey, privKey, err := ed25519.GenerateKey(bytes.NewReader(seed[:]))
	if err != nil {
		return nil, err
	}

	newDid := encodeDID(pubKey)
	if !newDid.IsValid() {
		return nil, ErrInvalidDID
	}

	// return the new provider
	return &Ed25519Provider{
		pubKey:  pubKey,
		privKey: privKey,
		did:     newDid,
	}, nil
}

// encodes the public key into a DID
func encodeDID(pubKey ed25519.PublicKey) DID {
	// indicator bytes are 0xed and 0x01 which say "it's an ed25519 key"
	indicatorBytes := append([]byte{0xed, 0x01}, pubKey...)

	// return the base58 encoded string of indicator bytes and the pub key
	return DID("did:key:" + base58.Encode(indicatorBytes))
}

// checks if the DID is valid
func (d DID) IsValid() bool {
	// should be of form "did:key:0xed0x01<SOME KEY>"
	if len(d) < 8+2 { // 8 for "did:key:" and at least 2 for "0xed0x01"
		return false
	}

	decoded := base58.Decode(string(d)[8:])
	if len(decoded) < 2+32 { // 2 for indicator bytes and 32 for pub key
		return false
	}

	indicatorBytes := decoded[:2]
	pubKey := decoded[2:]

	// check that the indicator bytes are 0xed and 0x01
	if indicatorBytes[0] != 0xed || indicatorBytes[1] != 0x01 {
		return false
	}

	// check that the pub key is 32 bytes
	if len(pubKey) != 32 {
		return false
	}

	return true
}

// gets the public key from the DID
func (d DID) PublicKey() (ed25519.PublicKey, error) {
	if !d.IsValid() {
		return nil, ErrInvalidDID
	}

	// if the DID is valid (above check), these won't have index range errors
	decoded := base58.Decode(string(d)[8:])
	pubKey := decoded[2:]

	return pubKey, nil
}

// creates a JWS using the provider's ed25519 private key
func (p *Ed25519Provider) CreateJWS(payload map[string]interface{}) (string, error) {
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return "", ErrSerialization
	}

	// header
	headerBytes, err := json.Marshal(map[string]string{
		"alg": "EdDSA",
		"kid": p.did.String(),
	})
	if err != nil {
		return "", ErrSerialization
	}

	// encodes the header and payload
	headerEncoded := base64.RawURLEncoding.EncodeToString(headerBytes)
	payloadEncoded := base64.RawURLEncoding.EncodeToString(payloadBytes)

	// creates sig
	toSign := headerEncoded + "." + payloadEncoded

	// signs using the private ed25519 key and encodes the sig
	sig := ed25519.Sign(p.privKey, []byte(toSign))
	sigEncoded := base64.RawURLEncoding.EncodeToString(sig)

	// returns JWS of form "header.payload.sig" (standard JWT-like structure stuff)
	return toSign + "." + sigEncoded, nil
}

// verifies the sig of the JWS using the ed25519 pub key
func (p *Ed25519Provider) VerifyJWS(jws string) (map[string]interface{}, error) {
	JwsComponents := strings.Split(jws, ".")
	if len(JwsComponents) != 3 {
		// uh oh, not of form "header.payload.sig"
		return nil, ErrInvalidSignature
	}

	headerEncoded, payloadEncoded, sigEncoded := JwsComponents[0], JwsComponents[1], JwsComponents[2]

	// decode the sig
	sig, err := base64.RawURLEncoding.DecodeString(sigEncoded)
	if err != nil {
		return nil, ErrInvalidEncoding
	}

	// is the signature valid?
	previouslySigned := headerEncoded + "." + payloadEncoded
	if !ed25519.Verify(p.pubKey, []byte(previouslySigned), sig) {
		return nil, ErrInvalidSignature
	}

	// decode payload
	payloadBytes, err := base64.RawURLEncoding.DecodeString(payloadEncoded)
	if err != nil {
		return nil, ErrInvalidEncoding
	}

	var payload map[string]interface{}
	err = json.Unmarshal(payloadBytes, &payload)
	if err != nil {
		return nil, ErrDeserialization
	}

	return payload, nil
}

// encrypts the payload using key x25519 derived from ed25519 key
//
// uses: https://pkg.go.dev/golang.org/x/crypto@v0.25.0/nacl/box
func (p *Ed25519Provider) CreateJWE(payload map[string]interface{}, audience DID) (string, error) {

	if !audience.IsValid() {
		return "", ErrInvalidDID
	}

	// convert ed25519 private key to x25519 so we can use it for encryption
	// because ed25519 is for signing not encryption
	x25519Priv, err := ed25519ToX25519(p.privKey)
	if err != nil {
		return "", err
	}

	// get the audience's x25519 pub key
	var recipientPub ed25519.PublicKey
	recipientPub, err = audience.PublicKey()
	if err != nil {
		return "", err
	}

	var recipientPubKey [32]byte
	copy(recipientPubKey[:], recipientPub[:])

	// encrypt the payload using a nacl box
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return "", ErrSerialization
	}

	// nonce 24 random bytes because that's what the `Seal` method below expects
	var nonce [24]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", err
	}

	encrypted := box.Seal(nil, payloadBytes, &nonce, &recipientPubKey, &x25519Priv)

	// encode with base64 url encoding all components into a JWE
	jwe := map[string]string{
		"nonce":   base64.RawURLEncoding.EncodeToString(nonce[:]),
		"cipher":  base64.RawURLEncoding.EncodeToString(encrypted),
		"pub_key": base64.RawURLEncoding.EncodeToString(p.pubKey[:]),
	}

	jweBytes, err := json.Marshal(jwe)
	if err != nil {
		return "", ErrSerialization
	}

	return string(jweBytes), nil
}

// decrypts the JWE using the x25519 private key derived from ed25519
//
// uses https://pkg.go.dev/golang.org/x/crypto@v0.25.0/nacl/box
func (p *Ed25519Provider) DecryptJWE(jwe string) (map[string]interface{}, error) {
	var jweObject map[string]string
	err := json.Unmarshal([]byte(jwe), &jweObject)
	if err != nil {
		return nil, ErrDeserialization
	}

	// decode each component from base 64
	decodedPubKey, err := base64.RawURLEncoding.DecodeString(jweObject["pub_key"])
	if err != nil {
		return nil, ErrDecryptionFailed
	}
	decodedNonce, err := base64.RawURLEncoding.DecodeString(jweObject["nonce"])
	if err != nil {
		return nil, ErrDecryptionFailed
	}
	decodedCipherText, err := base64.RawURLEncoding.DecodeString(jweObject["cipher"])
	if err != nil {
		return nil, ErrDecryptionFailed
	}

	// convert our ed25519 key to x25519 for decryption
	x25519Priv, err := ed25519ToX25519(p.privKey)
	if err != nil {
		return nil, err
	}

	// public key from base64 encoding, set to 32-byte fixed-size array
	var pubKey [32]byte
	copy(pubKey[:], decodedPubKey)

	// for nacl box, we set `nonce` to 24-byte fixed-size array
	// the reason for us "copying" these is to convert variable length slices to fixed size arrays
	var nonceArray [24]byte
	copy(nonceArray[:], decodedNonce)

	decrypted, ok := box.Open(nil, decodedCipherText, &nonceArray, &pubKey, &x25519Priv)
	if !ok {
		return nil, ErrDecryptionFailed
	}

	// decode the decrypted payload
	var payload map[string]interface{}
	err = json.Unmarshal(decrypted, &payload)
	if err != nil {
		return nil, ErrDeserialization
	}

	// return valid payload
	return payload, nil
}

// converts an Ed25519 private key to an X25519 private key for encryption
//
// references this: https://github.com/bcgit/bc-csharp/issues/514#issuecomment-1930590773
func ed25519ToX25519(privKey ed25519.PrivateKey) ([32]byte, error) {
	var x25519Priv [32]byte
	seed := privKey.Seed()

	seed[0] &= 248
	seed[31] &= 127
	seed[31] |= 64

	// seeds the x25519 priv key with the first 32 bytes of the seed
	// computes the x25519 priv key using basepoint
	//
	// returns result as slice of 32 bytes, so we copy it into a fixed size array
	x25519PrivBytes, err := curve25519.X25519(seed[:32], curve25519.Basepoint)
	if err != nil {
		return [32]byte{}, err
	}

	// copies the slice into a fixed size array of 32 bytes
	copy(x25519Priv[:], x25519PrivBytes)

	return x25519Priv, nil
}
