package keys

import (
	"bytes"
	"crypto"
	"fmt"

	"github.com/btcsuite/btcutil/base58"
	"github.com/ethereum/go-ethereum/crypto/secp256k1"
)

var network_id byte = 0x80

var (
	ErrNetworkIdMismatch = fmt.Errorf("private key network id mismatch")
	ErrChecksumMismatch  = fmt.Errorf("private key checksum mismatch")
	ErrNotOnCurve        = fmt.Errorf("private key not on secp256k1 curve")
)

type PrivateKey struct {
	key []byte
}

func doubleSha256(input []byte) []byte {
	return crypto.SHA256.New().Sum(crypto.SHA256.New().Sum(input))
}

//	function decodePrivate(encodedKey: string): Buffer {
//	  const buffer: Buffer = bs58.decode(encodedKey)
//	  assert.deepEqual(
//	    buffer.slice(0, 1),
//	    NETWORK_ID,
//	    'private key network id mismatch'
//	  )
//	  const checksum = buffer.slice(-4)
//	  const key = buffer.slice(0, -4)
//	  const checksumVerify = doubleSha256(key).slice(0, 4)
//	  assert.deepEqual(checksumVerify, checksum, 'private key checksum mismatch')
//	  return key
//	}
//
// https://github.com/openhive-network/dhive/blob/master/src/crypto.ts#L129C1-L141C2
func decodePrivate(encodedKey string) ([]byte, error) {
	buf, _, err := base58.CheckDecode(encodedKey)
	if err != nil {
		return nil, err
	}

	if buf[0] != network_id {
		return nil, ErrNetworkIdMismatch
	}

	checksum := buf[len(buf)-5:] // last 4 bytes is the checksum
	key := buf[:len(buf)-4]

	computedChecksum := doubleSha256(key)[:4]

	if !bytes.Equal(checksum, computedChecksum) {
		return nil, ErrChecksumMismatch
	}

	return key, nil
}

func NewPrivateKey(key []byte) (*PrivateKey, error) {
	x, _ := secp256k1.S256().Unmarshal(key)
	if x == nil {
		return nil, ErrNotOnCurve
	}

	return &PrivateKey{
		key,
	}, nil
}

func NewPrivateKeyFromString(wif string) (*PrivateKey, error) {
	bytes, err := decodePrivate(wif)
	if err != nil {
		return nil, err
	}

	return NewPrivateKey(bytes)
}

func NewPrivateKeyFromSeed(seed string) (*PrivateKey, error) {
	return NewPrivateKey(crypto.SHA256.New().Sum([]byte(seed)))
}

type KeyRole string

const (
	KeyRoleActive  KeyRole = "active"
	KeyRoleOwner   KeyRole = "owner"
	KeyRolePosting KeyRole = "posting"
	KeyRoleMemo    KeyRole = "memo"
)

func NewPrivateKeyFromLogin(
	username string,
	password string,
	role KeyRole,
) (*PrivateKey, error) {
	seed := username + string(role) + password
	return NewPrivateKeyFromSeed(seed)
}

/**
 * Return true if signature is canonical, otherwise false.
 */
func isCanonicalSignature(signature []byte) bool {
	return ((signature[0]&0x80) == 0 &&
		!(signature[0] == 0 && (signature[1]&0x80) == 0) &&
		(signature[32]&0x80) == 0 &&
		!(signature[32] == 0 && (signature[33]&0x80) == 0))
}

type Signature struct {
	signature  [64]byte
	recoveryId byte // TODO what is the size?
}

func (key *PrivateKey) Sign(message [32]byte) (*Signature, error) {
	attempts := byte(0)
	var signature []byte
	for attempts == 0 || !isCanonicalSignature(signature[:64]) { // our sig contains recovery id, we need to remove it before calling the func
		attempts++
		data := crypto.SHA256.New().Sum(append(message[:], attempts))
		sig, err := secp256k1.Sign(data, key.key)
		if err != nil {
			return nil, err
		}
		signature = sig
	}
	return &Signature{
		signature:  [64]byte(signature[:64]),
		recoveryId: signature[64],
	}, nil
}

func (key *PrivateKey) Public(prefix ...string) {
	secp256k1.S256().ScalarBaseMult(key.key)
}

/**
 * Return true if string is wif, otherwise false.
 */
// function isWif(privWif: string | Buffer): boolean {
//   try {
//       const bufWif = new Buffer(bs58.decode(privWif))
//       const privKey = bufWif.slice(0, -4)
//       const checksum = bufWif.slice(-4)
//       let newChecksum = sha256(privKey)
//       newChecksum = sha256(newChecksum)
//       newChecksum = newChecksum.slice(0, 4)
//       return (checksum.toString() === newChecksum.toString())
//   } catch (e) {
//       return false
//   }
// }

/**
 * ECDSA (secp256k1) public key.
 */
// export class PublicKey {

//   public readonly uncompressed: Buffer

//   constructor(
//     public readonly key: any,
//     public readonly prefix = DEFAULT_ADDRESS_PREFIX,
//   ) {
//     assert(secp256k1.publicKeyVerify(key), 'invalid public key')
//     this.uncompressed = Buffer.from(secp256k1.publicKeyConvert(key, false))
//   }

//   public static fromBuffer(key: ByteBuffer) {
//     assert(secp256k1.publicKeyVerify(key), 'invalid buffer as public key')
//     return { key }
//   }

//   /**
//    * Create a new instance from a WIF-encoded key.
//    */
//   public static fromString(wif: string) {
//     const { key, prefix } = decodePublic(wif)
//     return new PublicKey(key, prefix)
//   }

//   /**
//    * Create a new instance.
//    */
//   public static from(value: string | PublicKey) {
//     if (value instanceof PublicKey) {
//       return value
//     } else {
//       return PublicKey.fromString(value)
//     }
//   }

//   /**
//    * Verify a 32-byte signature.
//    * @param message 32-byte message to verify.
//    * @param signature Signature to verify.
//    */
//   public verify(message: Buffer, signature: Signature): boolean {
//     return secp256k1.verify(message, signature.data, this.key)
//   }

//   /**
//    * Return a WIF-encoded representation of the key.
//    */
//   public toString() {
//     return encodePublic(this.key, this.prefix)
//   }

//   /**
//    * Return JSON representation of this key, same as toString().
//    */
//   public toJSON() {
//     return this.toString()
//   }

//   /**
//    * Used by `utils.inspect` and `console.log` in node.js.
//    */
//   public inspect() {
//     return `PublicKey: ${ this.toString() }`
//   }
// }

// export type KeyRole = 'owner' | 'active' | 'posting' | 'memo'

//   public multiply(pub: any): Buffer {
//     return Buffer.from(secp256k1.publicKeyTweakMul(pub.key, this.secret, false))
//   }

/**
 * Derive the public key for this private key.
 */
// public createPublic(prefix?: string): PublicKey {
//   return new PublicKey(secp256k1.publicKeyCreate(this.key), prefix)
// }
