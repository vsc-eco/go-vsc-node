// based closely on: https://github.com/vsc-eco/vsc-node/blob/main/src/services/new/utils/crypto/bls-did.ts
package dids

import (
	"encoding/base64"
	"fmt"
	"math/big"

	blocks "github.com/ipfs/go-block-format"
	"github.com/multiformats/go-multibase"
	blst "github.com/supranational/blst/bindings/go"
)

// ===== constants =====

const BlsDIDPrefix = "did:key:z"

// ===== interface assertions =====

var _ DID[blst.P1Affine] = BlsDID("")
var _ Provider = BlsProvider{}

// ===== BlsDID =====

type BlsDID string

func NewBlsDID(pubKey *blst.P1Affine) (BlsDID, error) {
	if pubKey == nil {
		return "", fmt.Errorf("failed to generate pub key from priv key")
	}

	// compress
	pubKeyBytes := pubKey.Compress()

	// prepend indicator bytes and bytes of compressed key
	data := append([]byte{0xea, 0x01}, pubKeyBytes...)

	// encode to base58, as is standard
	base58Encoded, err := multibase.Encode(multibase.Base58BTC, data)
	if err != nil {
		return "", err
	}

	return BlsDID(BlsDIDPrefix + string(base58Encoded)), nil
}

// ===== implementing the DID interface =====

// stringifies the DID
func (d BlsDID) String() string {
	return string(d)
}

// returns the key part of the DID
func (d BlsDID) Identifier() blst.P1Affine {
	base58Encoded := string(d)[len(BlsDIDPrefix):]

	// decode from base58
	_, data, err := multibase.Decode(base58Encoded)
	if err != nil {
		return blst.P1Affine{}
	}

	// remove indicator bytes
	pubKeyBytes := data[2:]

	// decompress the public key using uncompress
	pubKey := new(blst.P1Affine)
	if pubKey.Uncompress(pubKeyBytes) == nil {
		fmt.Println("Error uncompressing public key")
		return blst.P1Affine{}
	}

	// return the uncompressed public key
	return *pubKey
}

// verifies if the signature is valid for the block
func (d BlsDID) Verify(block blocks.Block, sig string) (bool, error) {
	if block == nil {
		return false, fmt.Errorf("failed to verify signature: block is nil")
	}

	// get the pub key from the DID
	pubKey := d.Identifier()

	// decode the signature from base64
	sigBytes, err := base64.StdEncoding.DecodeString(sig)
	if err != nil {
		return false, fmt.Errorf("failed to decode signature: %w", err)
	}

	// decompress the sig into a P2Affine-type
	signature := new(blst.P2Affine)
	if signature.Uncompress(sigBytes) == nil {
		return false, fmt.Errorf("failed to uncompress signature for DID: %s", d.String())
	}

	// verify the sig using the pub key and the CID bytes directly
	verified := signature.Verify(true, &pubKey, true, block.Cid().Bytes(), nil)

	// returns the verification result
	if !verified {
		return false, fmt.Errorf("sig verification failed for DID: %s", d.String())
	}

	return true, nil
}

// ===== KeyDIDProvider =====

type BlsProvider struct {
	privKey *blst.SecretKey
}

// create a new bls provider
func NewBlsProvider(privKey *blst.SecretKey) (BlsProvider, error) {

	if privKey == nil {
		return BlsProvider{}, fmt.Errorf("failed to create BLS provider: private key is nil")
	}

	return BlsProvider{privKey}, nil
}

// sign a block using the BLS priv key
func (b BlsProvider) Sign(block blocks.Block) (string, error) {
	if block == nil {
		return "", fmt.Errorf("failed to sign block: block is nil")
	}

	// sign the CID using the BLS priv key
	sig := new(blst.P2Affine).Sign(b.privKey, block.Cid().Bytes(), nil)

	// compress and put to base 64; make the sig transportable
	encodedSig := base64.StdEncoding.EncodeToString(sig.Compress())

	return encodedSig, nil
}

// ===== BLS circuit generator =====

// a Member represents an account and its public key
//
// This is meant to match the old implmentation: https://github.com/vsc-eco/vsc-node/blob/main/src/services/new/utils/crypto/bls-did.ts.
// It seems like this could be eventually just a DID, depending on how the code evolves and/or
// how this is needed.
type Member struct {
	Account string
	DID     BlsDID
}

// a helper struct that allows you to generate a BLS circuit
type BlsCircuitGenerator struct {
	members []Member
}

// create a new BLS circuit generator
func NewBlsCircuitGenerator(members []Member) BlsCircuitGenerator {
	return BlsCircuitGenerator{members}
}

// generate a partial BLS circuit
func (bcg BlsCircuitGenerator) Generate(msg blocks.Block) (partialBlsCircuit, error) {
	circuit, err := newBlsCircuit(msg)
	if err != nil {
		return partialBlsCircuit{}, err
	}

	return partialBlsCircuit{
		circuit: circuit,
		members: bcg.members,
	}, nil
}

func (bcg *BlsCircuitGenerator) AddMember(member Member) {
	// check if member already exists by DID
	for _, m := range bcg.members {
		if m.DID == member.DID {
			return // already exists, keep operation idempotent, so just pretend it's ok
		}
	}

	// add new member
	bcg.members = append(bcg.members, member)
}

// set the members of the BLS circuit generator
func (bcg *BlsCircuitGenerator) SetMembers(members []Member) error {
	// check if duplicate members exist by DID
	seen := make(map[BlsDID]struct{})
	for _, member := range members {
		if _, ok := seen[member.DID]; ok {
			return fmt.Errorf("duplicate member: %s", member.DID)
		}
		seen[member.DID] = struct{}{}
	}

	// set members
	bcg.members = members
	return nil
}

// get the members of the BLS circuit generator as a list of DIDs
func (bcg BlsCircuitGenerator) CircuitMap() []BlsDID {
	var dids []BlsDID
	for _, member := range bcg.members {
		dids = append(dids, member.DID)
	}
	return dids
}

// ===== partial bls circuit =====

// a partial BLS circuit that can be finalized later once all members have signed
type partialBlsCircuit struct {
	circuit *BlsCircuit // reference to the eventual full circuit
	members []Member
}

// get the members of the partial BLS circuit as a list of DIDs
func (pbc *partialBlsCircuit) CircuitMap() []BlsDID {
	var dids []BlsDID
	for _, member := range pbc.members {
		dids = append(dids, member.DID)
	}
	return dids
}

// view the message (Block)
func (pbc *partialBlsCircuit) Msg() blocks.Block {
	return pbc.circuit.msg
}

// add a signature to the partial BLS circuit and verify it
func (pbc *partialBlsCircuit) AddAndVerify(member Member, sig string) error {
	// check if member exists
	found := false
	for _, m := range pbc.members {
		if m.DID == member.DID {
			found = true
			break
		}
	}

	if !found {
		return fmt.Errorf("member not found: %s", member.DID)
	}

	// verify the signature
	verified, err := member.DID.Verify(pbc.circuit.msg, sig)
	if err != nil {
		return fmt.Errorf("failed to verify signature: %w", err)
	}

	if !verified {
		return fmt.Errorf("invalid signature")
	}

	// add and aggregate the signature and public key
	if err := pbc.circuit.add(member, sig); err != nil {
		return fmt.Errorf("failed to add and aggregate signature: %w", err)
	}

	return nil
}

// finalize the partial BLS circuit into a full BLS circuit
func (pbc *partialBlsCircuit) Finalize() (*BlsCircuit, error) {
	// if the block (message) is nil, fail the finalization
	if pbc.circuit.msg == nil {
		return nil, fmt.Errorf("failed to finalize BLS circuit: block is nil")
	}

	// if the sig is nil, fail the finalization
	if pbc.circuit.aggSig == nil {
		return nil, fmt.Errorf("failed to finalize BLS circuit: signature is nil")
	}

	// if no members have signed, fail the finalization
	if len(pbc.members) == 0 {
		return nil, fmt.Errorf("failed to finalize BLS circuit: no members")
	}

	var pubKeys []*blst.P1Affine
	for _, member := range pbc.members {
		pubKey := member.DID.Identifier()
		pubKeys = append(pubKeys, &pubKey)
	}

	var aggPub blst.P1Aggregate
	aggPub.Aggregate(pubKeys, true)

	// Set the aggDID to the aggregated public key
	aggPubKey := aggPub.ToAffine()
	aggDID, err := NewBlsDID(aggPubKey)
	if err != nil {
		return nil, fmt.Errorf("failed to set aggregate DID: %w", err)
	}
	pbc.circuit.aggDID = aggDID

	// if the number of public keys aggregated is not equal to the number of members, fail the finalization
	if len(pbc.circuit.aggPubKeys) != len(pbc.members) {
		return nil, fmt.Errorf("failed to finalize BLS circuit: not all members have signed")
	}

	return pbc.circuit, nil
}

// ===== full BLS circuit =====

// represents a full BLS circuit with all members signed
type BlsCircuit struct {
	msg        blocks.Block
	aggPubKeys map[BlsDID]bool
	aggSig     *blst.P2Affine
	aggDID     BlsDID
}

// message (Block) getter
func (b *BlsCircuit) Msg() blocks.Block {
	return b.msg
}

// verify public keys via dids
func (b *BlsCircuit) VerifyDIDs(dids []BlsDID) (bool, error) {
	// create a slice to hold the pub keys from the DIDs
	var pubKeys []*blst.P1Affine

	for _, did := range dids {
		pubKey := did.Identifier()
		pubKeys = append(pubKeys, &pubKey)
	}

	// aggregate all the pub keys
	var aggPub blst.P1Aggregate
	aggPub.Aggregate(pubKeys, true)
	aggPubKey := aggPub.ToAffine()

	// check if the aggregated public key matches the current circuit's aggregate DID's public key (if empty, error out)
	if b.aggDID == "" {
		return false, fmt.Errorf("aggregate DID is not set")
	}
	circuitAggPubKey := b.aggDID.Identifier()
	if !aggPubKey.Equals(&circuitAggPubKey) {
		return false, fmt.Errorf("aggregated public key does not match circuit's public key")
	}

	return true, nil
}

// internal method to create a new BLS circuit
func newBlsCircuit(msg blocks.Block) (*BlsCircuit, error) {
	if msg == nil {
		return nil, fmt.Errorf("failed to create BLS circuit: block is nil")
	}
	return &BlsCircuit{
		msg:        msg,
		aggPubKeys: make(map[BlsDID]bool),
	}, nil
}

// verifies the correctness of the BLS circuit by comparing aggregate pub key and aggregate sig
func (b *BlsCircuit) Verify() (bool, error) {
	if b.aggSig == nil {
		return false, fmt.Errorf("aggregate signature is nil")
	}

	if b.aggDID == "" {
		return false, fmt.Errorf("aggregate public key is nil")
	}

	if b.msg == nil {
		return false, fmt.Errorf("block is nil")
	}

	cidBytes := b.msg.Cid().Bytes()

	// get the aggregated pub key from the DID
	pubKey := b.aggDID.Identifier()

	// verify the sig using the public key and the CID bytes (message)
	verified := b.aggSig.Verify(true, &pubKey, true, cidBytes, nil)
	if !verified {
		return false, fmt.Errorf("aggregate signature verification failed")
	}

	return true, nil
}

// internally adds a new sig and pub key to the circuit and aggregates them.
func (b *BlsCircuit) add(member Member, sig string) error {

	pubKey := member.DID.Identifier()

	sigBytes, err := base64.StdEncoding.DecodeString(sig)
	if err != nil {
		return fmt.Errorf("failed to decode signature: %w", err)
	}

	// decompresses the sig
	signature := new(blst.P2Affine)
	if signature.Uncompress(sigBytes) == nil {
		return fmt.Errorf("failed to uncompress signature for DID: %s", member.DID.String())
	}

	// verifies the sig using the public key and the CID bytes (message)
	verified := signature.Verify(true, &pubKey, true, b.msg.Cid().Bytes(), nil)
	if !verified {
		return fmt.Errorf("signature verification failed for DID: %s", member.DID.String())
	}

	// adds the pub key to the map (to track which members have signed)
	b.aggPubKeys[member.DID] = true

	// aggregates the sig
	if b.aggSig == nil {
		// if doesn't already exist, set the new sig
		b.aggSig = signature
	} else {
		// if already exists, aggregate the new sig with the existing one
		var aggSig blst.P2Aggregate
		aggSig.Aggregate([]*blst.P2Affine{b.aggSig, signature}, true)
		b.aggSig = aggSig.ToAffine()
	}

	return nil
}

// serialize the bls circuit
func (b *BlsCircuit) Serialize(circuitMap []BlsDID) (map[string]string, error) {
	// check if sig exists
	if b.aggSig == nil {
		return nil, fmt.Errorf("no valid BLS signature")
	}

	// compress and encode signature to base64url (as the TypeScript old implementation does)
	sigBytes := b.aggSig.Compress()
	sigEncoded := base64.RawURLEncoding.EncodeToString(sigBytes)

	// create a bitset storing which DIDs have signed
	var bitset big.Int
	for idx, did := range circuitMap {
		if b.aggPubKeys[did] {
			bitset.SetBit(&bitset, idx, 1)
		}
	}

	// convert bitset to hex string
	h := bitset.Text(16)
	if len(h)%2 != 0 {
		h = "0" + h
	}

	// convert hex string to base64url
	bvEncoded := base64.RawURLEncoding.EncodeToString([]byte(h))

	// include block data (encoded in the base64url format, to abide by the TS implementation)
	blockData := base64.RawURLEncoding.EncodeToString(b.msg.RawData())

	// return signature, bitset, and data (Block)
	return map[string]string{
		"sig":  sigEncoded,
		"bv":   bvEncoded,
		"data": blockData,
	}, nil
}

// reconstructs the BLS circuit from base 64 url data and keyset
func DeserializeBlsCircuit(signedPayload map[string]string, keyset []string) (*BlsCircuit, error) {
	// decode signature from base64url
	sigBytes, err := base64.RawURLEncoding.DecodeString(signedPayload["sig"])
	if err != nil {
		return nil, fmt.Errorf("failed to decode signature: %w", err)
	}

	// decompress the signature
	sig := new(blst.P2Affine)
	if sig.Uncompress(sigBytes) == nil {
		return nil, fmt.Errorf("failed to uncompress signature")
	}

	// decode the bitvector from base64url
	bitsetHex, err := base64.RawURLEncoding.DecodeString(signedPayload["bv"])
	if err != nil {
		return nil, fmt.Errorf("failed to decode bit vector: %w", err)
	}

	// rebuild the bitset from the hex string
	var bitset big.Int
	bitset.SetString(string(bitsetHex), 16)

	// rebuild the public keys map based on the bitset and keyset
	aggPubKeys := make(map[BlsDID]bool)
	var pubKeys []*blst.P1Affine
	for idx, did := range keyset {
		if bitset.Bit(idx) == 1 {
			aggPubKeys[BlsDID(did)] = true
			pubKey := BlsDID(did).Identifier()
			pubKeys = append(pubKeys, &pubKey)
		}
	}

	// recreate the aggregate pub key (aggDID) by aggregating all the the pub keys
	var aggPub blst.P1Aggregate
	aggPub.Aggregate(pubKeys, true)

	aggPubKey := aggPub.ToAffine()
	aggDID, err := NewBlsDID(aggPubKey)
	if err != nil {
		return nil, fmt.Errorf("failed to recreate aggregate DID: %w", err)
	}

	// decode and reconstruct the block
	blockData, err := base64.RawURLEncoding.DecodeString(signedPayload["data"])
	if err != nil {
		return nil, fmt.Errorf("failed to decode block data: %w", err)
	}
	block := blocks.NewBlock(blockData)

	// return the reconstructed circuit with the recreated aggDID and block
	return &BlsCircuit{
		msg:        block,
		aggSig:     sig,
		aggPubKeys: aggPubKeys,
		aggDID:     aggDID,
	}, nil
}
