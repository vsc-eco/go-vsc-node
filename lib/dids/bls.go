package dids

import (
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"sync/atomic"

	blocks "github.com/ipfs/go-block-format"
	"github.com/ipfs/go-cid"
	"github.com/multiformats/go-multibase"

	bls "github.com/protolambda/bls12-381-util"
)

// ===== constants =====

const BlsDIDPrefix = "did:key:"

// ===== type aliases for clarity =====

type BlsPubKey = bls.Pubkey
type BlsSig = bls.Signature
type blsAggSig = bls.Signature

// included for completeness, though it's obvious from the name
// that this is the private (secret) key
type BlsPrivKey = bls.SecretKey

// ===== interface assertions =====

var _ DID = BlsDID("")
var _ Provider[cid.Cid] = &blsProvider{}

// ===== BlsDID =====

type BlsDID string

func NewBlsDID(pubKey *BlsPubKey) (BlsDID, error) {
	if pubKey == nil {
		return "", fmt.Errorf("failed to generate pub key from priv key")
	}

	// compress
	pubKeyBytes := pubKey.Serialize()

	// prepend indicator bytes & bytes of compressed key
	data := append([]byte{0xea, 0x01}, pubKeyBytes[:]...)

	// encode to base58, as is standard
	base58Encoded, err := multibase.Encode(multibase.Base58BTC, data)
	if err != nil {
		return "", err
	}

	return BlsDID(BlsDIDPrefix + string(base58Encoded)), nil
}

// ParseBlsDID takes in the election member key
func ParseBlsDID(did string) (BlsDID, error) {
	if !strings.HasPrefix(did, BlsDIDPrefix) {
		return "", fmt.Errorf("invalid BLS DID: %s", did)
	}

	// remove the prefix
	encoded := did[len(BlsDIDPrefix):]
	encoding, data, err := multibase.Decode(encoded)
	if err != nil {
		return "", err
	}

	// ensure the encoding is base58
	if encoding != multibase.Base58BTC {
		return "", fmt.Errorf("invalid encoding for BLS DID: %c", encoding)
	}

	// ensure the data has the right prefix bytes
	if len(data) < 2 || data[0] != 0xea || data[1] != 0x01 {
		return "", fmt.Errorf("invalid BLS DID data: %s", hex.EncodeToString(data))
	}

	// ensure pub key is valid
	pubKeyBytes := data[2:]
	pubKey := new(BlsPubKey)
	if err := pubKey.Deserialize((*[48]byte)(pubKeyBytes)); err != nil {
		return "", fmt.Errorf("failed to deserialize pub key: %w", err)
	}

	return BlsDID(did), nil
}

// ===== implementing the DID interface =====

// stringifies the DID
func (d BlsDID) String() string {
	return string(d)
}

// returns the key part of the DID ("did:key:z<what_is_returned>")
func (d BlsDID) Identifier() *BlsPubKey {
	base58Encoded := string(d)[len(BlsDIDPrefix):]

	// decode from base58
	_, data, err := multibase.Decode(base58Encoded)
	if err != nil {
		fmt.Println(err)
		return nil
	}

	// remove indicator bytes
	pubKeyBytes := data[2:]

	// decompress the pub key
	pubKey := new(BlsPubKey)
	if pubKey.Deserialize((*[48]byte)(pubKeyBytes)) != nil {
		return nil
	}

	// return uncompressed pub key
	return pubKey
}

// verifies if the sig is valid for the block (based on its CID)
func (d BlsDID) Verify(blk blocks.Block, sig string) (bool, error) {
	cid := blk.Cid()
	// get the pub key from the DID
	pubKey := d.Identifier()

	// decode the sig from base64
	sigBytes, err := base64.StdEncoding.DecodeString(sig)
	if err != nil {
		return false, fmt.Errorf("failed to decode signature: %w", err)
	}

	// decompress the sig into a P2Affine-type (which is a BlsSig)
	signature := new(BlsSig)
	if signature.Deserialize((*[96]byte)(sigBytes)) == nil {
		return false, fmt.Errorf("failed to uncompress signature for DID: %s", d.String())
	}

	// verify the sig using the pub key and the CID bytes directly
	// verified := signature.Verify(true, pubKey, true, cid.Bytes(), nil)
	verified := bls.Verify(pubKey, cid.Bytes(), signature)

	// return the verification result
	if !verified {
		return false, fmt.Errorf("sig verification failed for DID: %s", d.String())
	}

	return true, nil
}

// ===== KeyDIDProvider =====

type blsProvider struct {
	privKey *BlsPrivKey
}

type BlsProvider = *blsProvider

// creates a new BLS provider
func NewBlsProvider(privKey *BlsPrivKey) (BlsProvider, error) {
	if privKey == nil {
		return nil, fmt.Errorf("failed to create BLS provider: private key is nil")
	}

	return &blsProvider{privKey}, nil
}

// signs a block using the BLS priv key
func (b BlsProvider) Sign(cid cid.Cid) (string, error) {

	sigBytes, err := b.SignRaw(cid)
	if err != nil {
		return "", err
	}

	encodedSig := base64.URLEncoding.EncodeToString(sigBytes[:])

	return encodedSig, nil
}

func (b BlsProvider) Type() string {
	return "bls"
}

// signs a block using the BLS priv key
func (b BlsProvider) SignRaw(cid cid.Cid) ([96]byte, error) {

	sig := bls.Sign(b.privKey, cid.Bytes())
	// sign the CID using the BLS priv key
	// sig := new(blst.P2Affine).Sign(b.privKey, cid.Bytes(), nil)

	// compress and encode to base64; make the sig transportable
	sigBytes := sig.Serialize()

	return sigBytes, nil
}

// ===== BLS circuit generator =====

// represents an account and its associated pub key
type Member = BlsDID

// helper struct that allows you to gen a BLS circuit
type BlsCircuitGenerator struct {
	members []Member
}

// create a new BLS circuit generator
func NewBlsCircuitGenerator(members []Member) *BlsCircuitGenerator {
	return &BlsCircuitGenerator{members}
}

// gen a partial BLS circuit
func (bcg BlsCircuitGenerator) Generate(msg cid.Cid) (PartialBlsCircuit, error) {
	circuit, err := newBlsCircuit(&msg, bcg.CircuitMap())
	if err != nil {
		return nil, err
	}

	return &partialBlsCircuit{
		circuit: circuit,
		members: bcg.members,
	}, nil
}

func (bcg *BlsCircuitGenerator) AddMember(member Member) {
	// check if member already exists by DID
	for _, m := range bcg.members {
		if m == member {
			return // already exists, keep operation idempotent, so just pretend it's ok
		}
	}

	// append new member to "must signs"
	bcg.members = append(bcg.members, member)
}

// set the members of the BLS circuit generator
func (bcg *BlsCircuitGenerator) SetMembers(members []Member) error {
	// check if dupe members exist by DID
	seen := make(map[BlsDID]struct{})
	for _, member := range members {
		if _, ok := seen[member]; ok {
			// no duplicate members allowed, else, error
			return fmt.Errorf("duplicate member: %s", member)
		}
		seen[member] = struct{}{}
	}

	// set members
	bcg.members = members
	return nil
}

// get the members of the BLS circuit generator as a list of DIDs
func (bcg BlsCircuitGenerator) CircuitMap() []BlsDID {
	var dids []BlsDID
	for _, member := range bcg.members {
		dids = append(dids, member)
	}
	return dids
}

// ===== partial BLS circuit =====

// a partial BLS circuit that can be finalized later once all members have signed
type partialBlsCircuit struct {
	circuit *BlsCircuit // the underlying full BLS circuit
	members []Member
}

type PartialBlsCircuit = *partialBlsCircuit

func (pbc *partialBlsCircuit) Signers() []BlsDID {
	pbc.circuit.mutex.Lock()
	defer pbc.circuit.mutex.Unlock()
	dids := make([]BlsDID, 0, len(pbc.circuit.sigs))
	for did := range pbc.circuit.sigs {
		dids = append(dids, did)
	}
	return dids
}

func (pbc *partialBlsCircuit) SignerCount() int {
	pbc.circuit.mutex.Lock()
	defer pbc.circuit.mutex.Unlock()
	return len(pbc.circuit.sigs)
}

// get the members of the partial BLS circuit as a list of DIDs
func (pbc *partialBlsCircuit) CircuitMap() []BlsDID {
	var dids []BlsDID
	for _, member := range pbc.members {
		dids = append(dids, member)
	}
	return dids
}

// view the message (Block)
func (pbc *partialBlsCircuit) Msg() cid.Cid {
	return *pbc.circuit.msg
}

// add a sig to the partial BLS circuit and verify it

// from @vaultec note members should be predefined in the structure before AddAndVerify. TH
func (pbc *partialBlsCircuit) AddAndVerify(member Member, sig string) (bool, error) {
	// check if member exists
	found := false
	for _, m := range pbc.members {
		if m == member {
			found = true
			break
		}
	}

	// if not found, error
	if !found {
		return false, fmt.Errorf("member not found: %s", member)
	}

	// add and verify the signature and public key
	added, err := pbc.circuit.add(member, sig)
	if err != nil {
		return false, fmt.Errorf("failed to add and verify signature: %w", err)
	}

	return added, nil
}

func (pbc *partialBlsCircuit) AddAndVerifyRaw(member BlsDID, sig []byte) (bool, error) {
	// check if member exists
	found := false
	for _, m := range pbc.members {
		if m == member {
			found = true
			break
		}
	}

	// if not found, error
	if !found {
		return false, fmt.Errorf("member not found: %s", member)
	}

	// add and verify the signature and public key
	added, err := pbc.circuit.addRaw(member, sig)
	if err != nil {
		return false, fmt.Errorf("failed to add and verify signature: %w", err)
	}

	return added, nil
}

// finalize the partial BLS circuit into a full
// BLS circuit which we can then use
func (pbc *partialBlsCircuit) Finalize() (*BlsCircuit, error) {
	// if the block (message) is nil, fail the finalization
	if pbc.circuit.msg == nil {
		return nil, fmt.Errorf("failed to finalize BLS circuit: block is nil")
	}

	// if no members have signed, fail the finalization
	if len(pbc.circuit.sigs) == 0 {
		return nil, fmt.Errorf("failed to finalize BLS circuit: no signatures collected")
	}

	// finalize the BLS circuit
	pbc.circuit.finalize()

	// aggregate signatures and build bit vector
	if err := pbc.circuit.aggregateSignatures(); err != nil {
		return nil, fmt.Errorf("failed to aggregate signatures: %w", err)
	}
	// pbc.circuit.msg = pbc.Msg()

	// returns the fully populated and verified BLS circuit
	return pbc.circuit, nil
}

// ===== full BLS circuit =====

// a full BLS circuit with all members signed
type BlsCircuit struct {
	//Core structure of what is a BLS circuit
	msg       *cid.Cid
	aggSigs   *BlsSig
	bitVector *big.Int

	//External input variable
	keyset []BlsDID

	//Construction variable
	mutex     sync.Mutex
	finalized atomic.Bool
	sigs      map[BlsDID]*BlsSig

	//Calculated Value
	// includedDIDs []BlsDID
}

// message (Block) getter
func (b *BlsCircuit) Msg() cid.Cid {
	return *b.msg
}

// internal method to create a new BLS circuit
func newBlsCircuit(msg *cid.Cid, keyset []BlsDID) (*BlsCircuit, error) {
	if msg == nil {
		return nil, fmt.Errorf("failed to create BLS circuit: block is nil")
	}
	return &BlsCircuit{
		msg:    msg,
		keyset: keyset,
		// pre-alloc based on size we know it must be
		sigs: make(map[BlsDID]*BlsSig, len(keyset)),
	}, nil
}

// internally adds a new sig and pub key to the circuit after verification
func (b *BlsCircuit) add(member Member, sig string) (bool, error) {
	sigBytes, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil {
		return false, fmt.Errorf("failed to decode signature: %w", err)
	}

	return b.addRaw(member, sigBytes)
}

func (b *BlsCircuit) addRaw(DID BlsDID, sigBytes []byte) (bool, error) {
	pubKey := DID.Identifier()

	// decompress the sig
	signature := new(BlsSig)
	if signature.Deserialize((*[96]byte)(sigBytes)) != nil {
		return false, fmt.Errorf("failed to uncompress signature for DID: %s", DID.String())
	}

	// verify the sig using the pub key and the CID bytes (message)
	verified := bls.Verify(pubKey, b.msg.Bytes(), signature)
	if !verified {
		return false, nil
	}

	// store the sig
	if !b.finalized.Load() {
		b.mutex.Lock()
		b.sigs[DID] = signature
		b.mutex.Unlock()
	}

	return true, nil
}

// finalizes the BLS circuit
func (b *BlsCircuit) finalize() {
	b.finalized.Store(true)
}

// aggregates the collected signatures and builds the bit vector
func (b *BlsCircuit) aggregateSignatures() error {
	if len(b.sigs) == 0 {
		return fmt.Errorf("no signatures to aggregate")
	}

	if !b.finalized.Load() {
		return fmt.Errorf("circuit not finalized")
	}

	var sigs []*BlsSig
	b.bitVector = big.NewInt(0)
	// b.includedDIDs = nil

	for idx, did := range b.keyset {
		if sig, ok := b.sigs[did]; ok {
			sigs = append(sigs, sig)
			// b.includedDIDs = append(b.includedDIDs, did)
			b.bitVector.SetBit(b.bitVector, idx, 1)
		}
	}

	if len(sigs) == 0 {
		return fmt.Errorf("no signatures to aggregate")
	}

	sig, _ := bls.Aggregate(sigs)
	b.aggSigs = sig

	return nil
}

// agg sigs
func (b *BlsCircuit) AggregatedSignature() (string, error) {
	if b.aggSigs == nil {
		return "", fmt.Errorf("aggregated signature not generated")
	}
	aggSigBytes := b.aggSigs.Serialize()
	aggSigEncoded := base64.RawURLEncoding.EncodeToString(aggSigBytes[:])
	return aggSigEncoded, nil
}

// returns the bit vector as a base64 encoded string
func (b *BlsCircuit) BitVector() (string, error) {
	if b.bitVector == nil {
		return "", fmt.Errorf("bit vector not generated")
	}
	bs := b.bitVector.Bytes()

	bvEncoded := base64.RawURLEncoding.EncodeToString(bs)
	return bvEncoded, nil
}

func (b *BlsCircuit) RawBitVector() *big.Int {
	return b.bitVector
}

// returns the included DIDs
func (b *BlsCircuit) IncludedDIDs() []BlsDID {
	var includedDIDs []BlsDID

	for idx, did := range b.keyset {
		if b.bitVector.Bit(idx) == 1 {
			includedDIDs = append(includedDIDs, did)
		}
	}
	return includedDIDs
}

// SerializedCircuit contains Signature plus Bitvector
// Include MembershipMap when reconstructing
type SerializedCircuit struct {
	Signature string `json:"sig"`
	BitVector string `json:"bv"`
}

// serialize the BLS circuit, including the bit vector
func (b *BlsCircuit) Serialize() (*SerializedCircuit, error) {
	// ensure signatures are aggregated
	if b.aggSigs == nil || b.bitVector == nil {
		if err := b.aggregateSignatures(); err != nil {
			return nil, fmt.Errorf("failed to aggregate signatures: %w", err)
		}
	}

	// get the bit vector
	bvEncoded, err := b.BitVector()
	if err != nil {
		return nil, fmt.Errorf("failed to get bit vector: %w", err)
	}

	sig, err := b.AggregatedSignature()

	if err != nil {
		return nil, fmt.Errorf("failed to get sig: %w", err)
	}

	// create the SerializedCircuit struct
	serializedCircuit := &SerializedCircuit{
		Signature: sig,
		BitVector: bvEncoded,
	}

	return serializedCircuit, nil
}

// reconstructs the aggregate DID from serialized data and keyset
func DeserializeBlsCircuit(serialized SerializedCircuit, keyset []BlsDID, msg cid.Cid) (*BlsCircuit, error) {
	// unmarshal the JSON data

	// decode the bit vector
	bvBytes, err := base64.RawURLEncoding.DecodeString(serialized.BitVector)
	if err != nil {
		return nil, fmt.Errorf("failed to decode bit vector: %w", err)
	}
	bitset := new(big.Int)
	//SetString was failing
	bitset.SetBytes(bvBytes)

	//Decode should be base64url
	sigBytes, err := base64.RawURLEncoding.DecodeString(serialized.Signature)

	if err != nil {
		return nil, err
	}

	signature := new(BlsSig)
	err = signature.Deserialize((*[96]byte)(sigBytes))
	if err != nil {
		return nil, fmt.Errorf("failed to uncompress signatures")
	}

	// return the aggregate DID
	return &BlsCircuit{
		aggSigs:   signature,
		bitVector: bitset,
		keyset:    keyset,
		msg:       &msg,
	}, nil
}

// verifies the correctness of the BLS circuit by comparing the aggregate signature with the dynamically aggregated pub keys
//
// returns whether it's valid and which BLS DIDs were part of the valid sig aggregation
// "sig": aggregated sig as base64 string
// "bv": bit vector as base64 string (rawurlencoded base64 of bitset.Text(16))
func (b *BlsCircuit) Verify() (bool, []BlsDID, error) {
	if b.msg == nil {
		return false, nil, fmt.Errorf("Msg is nil")
	}

	// should have already been aggregated when the circuit was finalized
	// but just as a "sanity check" we'll ensure they are before we proceed
	//@vaultec note: in production we won't have access to the original signatures.

	// if b.aggSigs == nil || b.bitVector == nil {
	// 	if err := b.aggregateSignatures(); err != nil {
	// 		return false, nil, fmt.Errorf("failed to aggregate signatures: %w", err)
	// 	}
	// }

	// get the aggregated sig and bit vector
	// sig, err := b.AggregatedSignature()
	// if err != nil {
	// 	return false, nil, fmt.Errorf("failed to get aggregated signature: %w", err)
	// }

	_, err := b.BitVector()
	if err != nil {
		return false, nil, fmt.Errorf("failed to get bit vector: %w", err)
	}

	// decode the sig from base64
	// sigBytes, err := base64.StdEncoding.DecodeString(sig)
	// if err != nil {
	// 	return false, nil, fmt.Errorf("failed to decode signature: %w", err)
	// }

	// decompress the aggregated sig
	// aggSignature := new(BlsSig)
	// if aggSignature.Uncompress(sigBytes) == nil {
	// 	return false, nil, fmt.Errorf("failed to uncompress aggregated signature")
	// }

	// decode the bit vector from base64
	// bvBytes, err := base64.RawURLEncoding.DecodeString(bv)
	// if err != nil {
	// 	return false, nil, fmt.Errorf("failed to decode bit vector: %w", err)
	// }

	// // rebuild the bitset from the hex string
	// bitset := new(big.Int)
	// bitset.SetString(string(bvBytes), 16)

	// build the included public keys based on the bitset
	var includedPubKeys []*BlsPubKey
	var includedDIDs []BlsDID

	for idx, did := range b.keyset {
		if b.bitVector.Bit(idx) == 1 {
			pubKey := did.Identifier()
			includedPubKeys = append(includedPubKeys, pubKey)
			includedDIDs = append(includedDIDs, did)
		}
	}

	if len(includedPubKeys) == 0 {
		return false, nil, fmt.Errorf("no public keys to verify")
	}

	// aggregate the pub keys
	// agg all the pub keys at once
	pubKey, _ := bls.AggregatePubkeys(includedPubKeys)
	// aggWorks := aggPub.Aggregate(includedPubKeys, true)
	// aggPubKey := aggPub.ToAffine()

	// verify the aggregated sig
	verified := bls.Verify(pubKey, b.msg.Bytes(), b.aggSigs)
	// verified := b.aggSigs.FastAggregateVerify(true, includedPubKeys, b.msg.Bytes(), nil)

	return verified, includedDIDs, nil
}
