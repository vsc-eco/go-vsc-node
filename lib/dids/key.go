package dids

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"golang.org/x/crypto/hkdf"

	blocks "github.com/ipfs/go-block-format"
	"github.com/jorrizza/ed2curve25519"
	"github.com/multiformats/go-multibase"
	"golang.org/x/crypto/curve25519"
)

// ===== constants =====

const KeyDIDPrefix = "did:key:"

// ===== interface assertions =====

var _ DID[ed25519.PublicKey] = KeyDID("")
var _ Provider = KeyProvider{}

// ===== KeyDID =====

type KeyDID string

func NewKeyDID(pubKey ed25519.PublicKey) (DID[ed25519.PublicKey], error) {

	if pubKey == nil {
		return KeyDID(""), fmt.Errorf("invalid public key")
	}

	// adds indicator bytes saying "this is an ed25519 key"
	data := append([]byte{0xED, 0x01}, pubKey...)

	// encoding everything in base58, as per the spec
	base58Encoded, err := multibase.Encode(multibase.Base58BTC, data)
	if err != nil {
		return KeyDID(""), err
	}

	return KeyDID(KeyDIDPrefix + string(base58Encoded)), nil
}

// ===== implementing the DID interface =====

func (d KeyDID) String() string {
	return string(d)
}

func (d KeyDID) Identifier() ed25519.PublicKey {
	// remove the "did:key:" prefix
	base58Encoded := string(d)[len(KeyDIDPrefix):]

	// decoding the base58 encoded string
	_, data, err := multibase.Decode(base58Encoded)
	if err != nil {
		return nil
	}

	// remove the indicator bytes
	return ed25519.PublicKey(data[2:])
}

func (d KeyDID) Verify(block blocks.Block, sig string) (bool, error) {
	// re-create the header
	header := map[string]interface{}{
		"alg": "EdDSA",
		"kid": d.String() + "#" + strings.Split(d.String(), ":")[2],
	}

	// marshal
	headerJSON, err := json.Marshal(header)
	if err != nil {
		return false, fmt.Errorf("error marshalling header: %w", err)
	}

	// encode header as raw url base 64
	encodedHeader := base64.RawURLEncoding.EncodeToString(headerJSON)
	signingInput := encodedHeader + "." + base64.RawURLEncoding.EncodeToString(block.Cid().Bytes())

	// decode the sig
	decodedSignature, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil {
		return false, fmt.Errorf("invalid signature encoding: %w", err)
	}

	// get pub key from the DID
	pubKey := d.Identifier()
	if pubKey == nil {
		return false, fmt.Errorf("invalid DID identifier: nil public key")
	}

	// verify the sig
	verified := ed25519.Verify(pubKey, []byte(signingInput), decodedSignature)
	if !verified {
		return false, fmt.Errorf("signature verification failed")
	}

	return true, nil
}

// ===== KeyDIDProvider =====

type KeyProvider struct {
	privKey ed25519.PrivateKey
}

func NewKeyProvider(privKey ed25519.PrivateKey) KeyProvider {
	return KeyProvider{privKey: privKey}
}

// ===== implementing the Provider and KeyDIDProvider interfaces =====

func (k KeyProvider) Sign(block blocks.Block) (string, error) {
	// gen the DID from the priv key
	did, err := NewKeyDID(k.privKey.Public().(ed25519.PublicKey))
	if err != nil {
		return "", err
	}

	// construct the "kid" string for the header
	kidStr := did.String() + "#" + strings.Split(did.String(), ":")[2]

	// create the JWT header (to match https://github.com/vsc-eco/vsc-node/blob/main/src/services/new/transactionPool.ts#L153)
	header := map[string]interface{}{
		"alg": "EdDSA",
		"kid": kidStr,
	}

	// serialize the header to JSON
	headerJSON, err := json.Marshal(header)
	if err != nil {
		return "", err
	}

	// base64 raw URL encode the header and payload (CID)
	encodedHeader := base64.RawURLEncoding.EncodeToString(headerJSON)
	encodedPayload := base64.RawURLEncoding.EncodeToString(block.Cid().Bytes())

	// create the sig input (header + payload)
	signingInput := encodedHeader + "." + encodedPayload

	// sigh the sig input using Ed25519
	sig := ed25519.Sign(k.privKey, []byte(signingInput))

	// raw base64 URL encode the sig
	return base64.RawURLEncoding.EncodeToString(sig), nil
}

// ===== other methods =====

// creates JWE using the recipient's pub key
func CreateJWE(payload map[string]interface{}, recipient ed25519.PublicKey) (string, error) {

	// convert recipient's ed25519 pub key to curve25519 pub key
	curve25519PubKey := ed2curve25519.Ed25519PublicKeyToCurve25519(recipient)

	// gen ephemeral keypair for ECDH using x25519
	ephemeralPriv := make([]byte, 32)
	_, err := rand.Read(ephemeralPriv)
	if err != nil {
		return "", err
	}
	ephemeralPub, err := curve25519.X25519(ephemeralPriv, curve25519.Basepoint)
	if err != nil {
		return "", err
	}

	// perform x25519 to compute shared secret
	sharedSecret, err := curve25519.X25519(ephemeralPriv, curve25519PubKey[:])
	if err != nil {
		return "", err
	}

	// apply HKDF to derive AES key from shared secret using SHA-256
	// helpful ref: https://kerkour.com/derive-keys-hkdf-sha256-golang
	hkdf := hkdf.New(sha256.New, sharedSecret, nil, nil)
	aesKey := make([]byte, 32)
	if _, err := io.ReadFull(hkdf, aesKey); err != nil {
		return "", err
	}

	// encrypt the payload using AES-GCM
	block, err := aes.NewCipher(aesKey)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}

	// make a random gcm.NonceSize()-sized nonce for encryption
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}

	// marshal and then encrypt the payload
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	ciphertext := gcm.Seal(nil, nonce, payloadJSON, nil)

	// extract the tag and separate it from ciphertext and to ensure
	// the integrity of the ciphertext
	//
	// referenced: https://www.reddit.com/r/cryptography/comments/11wvpdu/can_the_length_of_an_aesgcm_output_be_predicted/
	// which caused me to look into cipher.AEAD type and found Overhead() which gets the length of the tag
	tagStart := len(ciphertext) - gcm.Overhead()
	authTag := ciphertext[tagStart:]
	cipherText := ciphertext[:tagStart]

	// create the JWE header in accordance with spec mentioned in the Sign method
	header := map[string]interface{}{
		"alg": "ECDH-ES",                                       // using ECDH (x25519) for key exchange
		"enc": "A256GCM",                                       // AES-GCM for encryption
		"epk": base64.StdEncoding.EncodeToString(ephemeralPub), // ephemeral pub key
		"typ": "JWE",
	}
	headerJSON, err := json.Marshal(header)
	if err != nil {
		return "", err
	}

	// base64 encode everything
	encodedHeader := base64.StdEncoding.EncodeToString(headerJSON)
	encodedCiphertext := base64.StdEncoding.EncodeToString(cipherText)
	encodedTag := base64.StdEncoding.EncodeToString(authTag)
	encodedNonce := base64.StdEncoding.EncodeToString(nonce)

	// return the JWE in a compact format
	//
	// ref: https://docs.authlib.org/en/v1.0.1/jose/jwe.html
	//
	// we don't use the 2nd "portion" of the compact JWE form since we generate our own shared key and stick the ephemeral pub key in the header (epk)
	// we don't use this so it's standard to leave it blank
	return fmt.Sprintf("%s..%s.%s.%s", encodedHeader, encodedNonce, encodedCiphertext, encodedTag), nil
}

func (k KeyProvider) DecryptJWE(jwe string) (map[string]interface{}, error) {
	// split a compact JWE (5 parts) into all 5 components
	parts := strings.Split(jwe, ".")
	if len(parts) != 5 {
		return nil, fmt.Errorf("invalid JWE format")
	}

	// decode each component from base64 (as it was originally encoded into this)
	headerJSON, err := base64.StdEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, fmt.Errorf("invalid header encoding: %w", err)
	}
	nonce, err := base64.StdEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, fmt.Errorf("invalid nonce encoding: %w", err)
	}
	ciphertext, err := base64.StdEncoding.DecodeString(parts[3])
	if err != nil {
		return nil, fmt.Errorf("invalid ciphertext encoding: %w", err)
	}
	authTag, err := base64.StdEncoding.DecodeString(parts[4])
	if err != nil {
		return nil, fmt.Errorf("invalid authentication tag encoding: %w", err)
	}

	// combine the ciphertext and auth tag
	fullCiphertext := append(ciphertext, authTag...)

	// parse the header to extract the ephemeral pub key passed
	// in originally during the JWE being encrypted
	var header map[string]interface{}
	err = json.Unmarshal(headerJSON, &header)
	if err != nil {
		return nil, fmt.Errorf("invalid header JSON: %w", err)
	}

	// this MUST be set into the header so we can decrypt
	ephemeralPubKeyStr, ok := header["epk"].(string)
	if !ok {
		// if the key isn't there, there's really nothing we can do
		return nil, fmt.Errorf("invalid ephemeral public key in header")
	}

	// decode the ephemeral pub key from base64 (as it was originally encoded into this)
	ephemeralPubKey, err := base64.StdEncoding.DecodeString(ephemeralPubKeyStr)
	if err != nil {
		return nil, fmt.Errorf("invalid ephemeral pub key encoding: %w", err)
	}

	// convert the ed25519 priv key to curve25519 priv key for decryption
	//
	// I got this from a library created to fix this issue: https://github.com/golang/go/issues/20504
	curve25519PrivKey := ed2curve25519.Ed25519PrivateKeyToCurve25519(k.privKey)

	// perform x25519 ECDH to compute the shared secret between the ephemeral pub key and
	// our curve25519 priv key derived from our ed25519 priv key
	sharedSecret, err := curve25519.X25519(curve25519PrivKey[:], ephemeralPubKey)
	if err != nil {
		return nil, fmt.Errorf("error during ECDH: %w", err)
	}

	// applying HKDF to derive a shared AES key from shared secret using sha256
	//
	// helpful ref: https://kerkour.com/derive-keys-hkdf-sha256-golang
	hkdf := hkdf.New(sha256.New, sharedSecret, nil, nil)
	aesKey := make([]byte, 32) // aes256 key is 32 bytes
	if _, err := io.ReadFull(hkdf, aesKey); err != nil {
		return nil, err
	}

	// init AES-CGM for to use for decryption
	block, err := aes.NewCipher(aesKey)
	if err != nil {
		return nil, fmt.Errorf("error initializing AES cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("error initializing GCM: %w", err)
	}

	// decrypt the ciphertext using AES-GCM with a gcm.NonceSize()-sized nonce and auth tag
	plaintext, err := gcm.Open(nil, nonce, fullCiphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("decryption failed: %w", err)
	}

	// unmarshal the decrypted payload
	var payload map[string]interface{}
	err = json.Unmarshal(plaintext, &payload)
	if err != nil {
		return nil, fmt.Errorf("error unmarshalling payload: %w", err)
	}

	return payload, nil
}
