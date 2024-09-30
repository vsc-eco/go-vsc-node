package dids

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"filippo.io/edwards25519"
	"github.com/multiformats/go-multibase"
	"golang.org/x/crypto/nacl/box"
)

var (
	ErrInvalidPrivateKeySize = errors.New("invalid private key size")
	ErrInvalidDID            = errors.New("invalid DID format")
	ErrInvalidSignature      = errors.New("invalid signature")
	ErrSerialization         = errors.New("serialization error")
	ErrDeserialization       = errors.New("deserialization error")
	ErrInvalidEncoding       = errors.New("invalid base64 encoding")
	ErrDecryptionFailed      = errors.New("decryption failed")
)

type KeyDID string

func NewDID(pubKey ed25519.PublicKey) (DID, error) {
	// prefix the pub key with the indicator bytes for ed25519 keys
	data := append([]byte{0xED, 0x01}, pubKey...)

	// base 58 encode key
	encodedKey, err := multibase.Encode(multibase.Base58BTC, data)
	if err != nil {
		return nil, err
	}

	// prepend the did:key: prefix
	did := KeyDID("did:key:" + encodedKey)

	return did, nil
}

func PubKeyFromKeyDID(did string) ([]byte, error) {
	// remove did prefix
	if !strings.HasPrefix(string(did), "did:key:") {
		return nil, fmt.Errorf("invalid DID format: missing did:key: prefix")
	}

	// remove 'did:key:' (8 chars)
	encodedKey := string(did)[8:]

	// decode base 58 string including the z prefix
	_, decodedBytes, err := multibase.Decode(encodedKey)
	if err != nil {
		return nil, fmt.Errorf("failed to decode multibase string: %w", err)
	}

	// remove the bytes and confirm they are there and the ed25519 indicator bytes
	if len(decodedBytes) < 2 || !bytes.Equal(decodedBytes[:2], []byte{0xED, 0x01}) {
		return nil, fmt.Errorf("invalid multicodec prefix: expected 0xED01 for Ed25519")
	}
	pubKey := decodedBytes[2:]

	return pubKey, nil
}

func (d KeyDID) FullString() string {
	return string(d)
}

// key provider for did ed25519 keys
type KeyProvider struct {
	pubKey  ed25519.PublicKey
	privKey ed25519.PrivateKey
	did     DID
}

func (p *KeyProvider) DID() DID {
	return p.did
}

// create new key provider
func NewKeyProvider(privKey ed25519.PrivateKey) (*KeyProvider, error) {
	// Get the pub key from priv
	pubKey := privKey.Public().(ed25519.PublicKey)

	// create a new DID
	did, err := NewDID(pubKey)
	if err != nil {
		return nil, fmt.Errorf("failed to create DID: %w", err)
	}

	// validate the DID by attempting to extract the public key
	extractedPubKey, err := PubKeyFromKeyDID(did.FullString())
	if err != nil {
		return nil, fmt.Errorf("failed to validate DID: %w", err)
	}

	// ensure the extracted public key matches the original
	if !bytes.Equal(pubKey, extractedPubKey) {
		return nil, fmt.Errorf("extracted public key does not match original")
	}

	// return new provider for keys
	return &KeyProvider{
		pubKey:  pubKey,
		privKey: privKey,
		did:     did,
	}, nil
}

// creates a JWS using the priv key
func (p *KeyProvider) Sign(payload interface{}) (string, error) {

	// assert payload of type map[string]interface{}
	payloadMap, ok := payload.(map[string]interface{})
	if !ok {
		return "", fmt.Errorf("payload must be of type map[string]interface{}")
	}

	// write the header
	header := map[string]interface{}{
		"alg": "EdDSA",
		"typ": "JWT",
		"kid": p.did.FullString(), // set kid as full DID like did:key:z123...
	}

	// encode header and payload
	headerJSON, err := json.Marshal(header)
	if err != nil {
		return "", fmt.Errorf("failed to marshal header: %w", err)
	}

	payloadJSON, err := json.Marshal(payloadMap)
	if err != nil {
		return "", fmt.Errorf("failed to marshal payload: %w", err)
	}

	encodedHeader := base64.RawURLEncoding.EncodeToString(headerJSON)
	encodedPayload := base64.RawURLEncoding.EncodeToString(payloadJSON)

	// create input for signing and sign
	signingInput := encodedHeader + "." + encodedPayload
	signature := ed25519.Sign(p.privKey, []byte(signingInput))

	// encode the sig
	encodedSignature := base64.RawURLEncoding.EncodeToString(signature)

	// aggregate all parts to form JWS
	return signingInput + "." + encodedSignature, nil
}

// verifies the JWS sig and returns the payload if it's valid
func VerifyJWS(pubKey ed25519.PublicKey, jws string) (map[string]interface{}, error) {
	// split JWS into components
	parts := strings.Split(jws, ".")
	if len(parts) != 3 {
		return nil, ErrInvalidSignature
	}

	// header, payload, sig
	encodedHeader, encodedPayload, encodedSignature := parts[0], parts[1], parts[2]

	// decode the sig
	signature, err := base64.RawURLEncoding.DecodeString(encodedSignature)
	if err != nil {
		return nil, fmt.Errorf("failed to decode signature: %w", err)
	}

	// verify the sig
	signingInput := encodedHeader + "." + encodedPayload
	if !ed25519.Verify(pubKey, []byte(signingInput), signature) {
		return nil, ErrInvalidSignature
	}

	// decode and parse the payload
	payloadJSON, err := base64.RawURLEncoding.DecodeString(encodedPayload)
	if err != nil {
		return nil, fmt.Errorf("failed to decode payload: %w", err)
	}

	var payload map[string]interface{}
	err = json.Unmarshal(payloadJSON, &payload)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal payload: %w", err)
	}

	return payload, nil
}

// encrypts the payload using an X25519 key derived from the edd25519 one and NaCl box encryption
func (p *KeyProvider) CreateJWE(payload map[string]interface{}, to DID) (string, error) {
	// get the "who we're sending to"'s pub key
	sendingToPubKey, err := PubKeyFromKeyDID(to.FullString())
	if err != nil {
		return "", fmt.Errorf("failed to get sending to's pub key: %w", err)
	}

	// convert ed25519 keys to curve25519
	senderPrivKey, err := ed25519PrivateKeyToCurve25519(p.privKey)
	if err != nil {
		return "", fmt.Errorf("failed to convert sender's private key: %w", err)
	}
	recipientCurvePubKey, err := ed25519PublicKeyToCurve25519(sendingToPubKey)
	if err != nil {
		return "", fmt.Errorf("failed to convert recipient's pub key: %w", err)
	}

	// gen a random nonce, 24 bytes as required
	var nonce [24]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", fmt.Errorf("failed to generate nonce: %w", err)
	}

	// encode payload
	plaintext, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("failed to marshal payload: %w", err)
	}

	// encrypt the payload via box seal
	encrypted := box.Seal(nil, plaintext, &nonce, &recipientCurvePubKey, &senderPrivKey)

	// create the protected header
	protectedHeader := map[string]interface{}{
		"alg":  "X25519",
		"enc":  "XSalsa20-Poly1305",
		"skid": p.did.FullString(), // sender's full DID
	}

	// encoded protected header
	protectedHeaderJSON, err := json.Marshal(protectedHeader)
	if err != nil {
		return "", fmt.Errorf("failed to marshal protected header: %w", err)
	}
	protectedHeaderEncoded := base64.RawURLEncoding.EncodeToString(protectedHeaderJSON)

	// create JWE
	jwe := map[string]interface{}{
		"protected":  protectedHeaderEncoded,
		"iv":         base64.RawURLEncoding.EncodeToString(nonce[:]),
		"ciphertext": base64.RawURLEncoding.EncodeToString(encrypted),
		"recipients": []map[string]interface{}{
			{
				"header": map[string]interface{}{
					"kid": to.FullString(), // sending to's DID
				},
			},
		},
	}

	// serialize the JWE
	jweJSON, err := json.Marshal(jwe)
	if err != nil {
		return "", fmt.Errorf("failed to marshal JWE: %w", err)
	}

	return string(jweJSON), nil
}

// decrypts the JWE and returns the payload
func (p *KeyProvider) DecryptJWE(jwe string) (map[string]interface{}, error) {
	// parse the JWE
	var jweData map[string]interface{}
	err := json.Unmarshal([]byte(jwe), &jweData)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal JWE: %w", err)
	}

	// extracts our needed components
	nonceEncoded, ok := jweData["iv"].(string)
	if !ok {
		return nil, fmt.Errorf("nonce not found in JWE")
	}
	nonce, err := base64.RawURLEncoding.DecodeString(nonceEncoded)
	if err != nil {
		return nil, fmt.Errorf("failed to decode nonce: %w", err)
	}

	ciphertextEncoded, ok := jweData["ciphertext"].(string)
	if !ok {
		return nil, fmt.Errorf("ciphertext not found in JWE")
	}
	ciphertext, err := base64.RawURLEncoding.DecodeString(ciphertextEncoded)
	if err != nil {
		return nil, fmt.Errorf("failed to decode ciphertext: %w", err)
	}

	// decodes the protected header
	protectedHeaderEncoded, ok := jweData["protected"].(string)
	if !ok {
		return nil, fmt.Errorf("protected header not found in JWE")
	}
	protectedHeaderJSON, err := base64.RawURLEncoding.DecodeString(protectedHeaderEncoded)
	if err != nil {
		return nil, fmt.Errorf("failed to decode protected header: %w", err)
	}
	var protectedHeader map[string]interface{}
	err = json.Unmarshal(protectedHeaderJSON, &protectedHeader)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal protected header: %w", err)
	}

	// get the sender's DID from 'skid'
	senderDIDStr, ok := protectedHeader["skid"].(string)
	if !ok {
		return nil, fmt.Errorf("sender's DID not found in protected header")
	}

	// get the sender's public key from the DID
	senderPubKey, err := PubKeyFromKeyDID(senderDIDStr)
	if err != nil {
		return nil, fmt.Errorf("failed to get sender's pub key: %w", err)
	}

	// convert keys to curve25519
	recipientPrivKey, err := ed25519PrivateKeyToCurve25519(p.privKey)
	if err != nil {
		return nil, fmt.Errorf("failed to convert recipient's private key: %w", err)
	}
	senderCurvePubKey, err := ed25519PublicKeyToCurve25519(senderPubKey)
	if err != nil {
		return nil, fmt.Errorf("failed to convert sender's public key: %w", err)
	}

	// decrypt the ciphertext
	var nonceArray [24]byte
	copy(nonceArray[:], nonce)

	decrypted, ok := box.Open(nil, ciphertext, &nonceArray, &senderCurvePubKey, &recipientPrivKey)
	if !ok {
		return nil, ErrDecryptionFailed
	}

	// parse the decrypted payload
	var payload map[string]interface{}
	err = json.Unmarshal(decrypted, &payload)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal decrypted payload: %w", err)
	}

	return payload, nil
}

// converts an ed25519 private key to a curve25519 private key
func ed25519PrivateKeyToCurve25519(privKey ed25519.PrivateKey) ([32]byte, error) {
	var curvePrivKey [32]byte

	// check that the private key has the correct size
	if len(privKey) != ed25519.PrivateKeySize {
		return curvePrivKey, fmt.Errorf("invalid ed25519 private key size")
	}

	// get the seed and hash it with sha-512
	seed := privKey.Seed()
	h := sha512.Sum512(seed)

	// apply clamping to the hashed seed
	h[0] &= 248
	h[31] &= 127
	h[31] |= 64

	// copy the first 32 bytes as the curve25519 private key
	copy(curvePrivKey[:], h[:32])

	return curvePrivKey, nil
}

// converts an ed25519 public key to a curve25519 public key
func ed25519PublicKeyToCurve25519(pubKey ed25519.PublicKey) ([32]byte, error) {
	var curvePubKey [32]byte

	// parse the ed25519 public key to an edwards25519 point
	edPubKeyPoint, err := new(edwards25519.Point).SetBytes(pubKey)
	if err != nil {
		return curvePubKey, fmt.Errorf("invalid ed25519 public key: %v", err)
	}

	// convert to a curve25519 public key in montgomery form
	copy(curvePubKey[:], edPubKeyPoint.BytesMontgomery())

	return curvePubKey, nil
}
