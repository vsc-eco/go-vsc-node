package dids

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"

	blocks "github.com/ipfs/go-block-format"
	"github.com/multiformats/go-multibase"
	blst "github.com/supranational/blst/bindings/go"
)

// ===== constants =====

const BlsDIDPrefix = "did:key:z"

// ===== type aliases for clarity =====

type BlsPubKey = blst.P1Affine
type BlsSig = blst.P2Affine

// included for completeness, though it's obvious from the name
// that this is the private (secret) key
type BlsPrivKey = blst.SecretKey

// ===== interface assertions =====

var _ DID[*BlsPubKey] = BlsDID("")
var _ Provider = BlsProvider{}

// ===== BlsDID =====

type BlsDID string

func NewBlsDID(pubKey *BlsPubKey) (BlsDID, error) {
	if pubKey == nil {
		return "", fmt.Errorf("failed to generate pub key from priv key")
	}

	// compress
	pubKeyBytes := pubKey.Compress()

	// prepend indicator bytes & bytes of compressed key
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

// returns the key part of the DID ("did:key:z<what_is_returned>")
func (d BlsDID) Identifier() *BlsPubKey {
	base58Encoded := string(d)[len(BlsDIDPrefix):]

	// decode from base58
	_, data, err := multibase.Decode(base58Encoded)
	if err != nil {
		return nil
	}

	// remove indicator bytes
	pubKeyBytes := data[2:]

	// decompress the pub key
	pubKey := new(BlsPubKey)
	if pubKey.Uncompress(pubKeyBytes) == nil {
		return nil
	}

	// return uncompressed pub key
	return pubKey
}

// verifies if the sig is valid for the block (based on its CID)
func (d BlsDID) Verify(block blocks.Block, sig string) (bool, error) {
	if block == nil {
		return false, fmt.Errorf("failed to verify signature: block is nil")
	}

	// get the pub key from the DID
	pubKey := d.Identifier()

	// decode the sig from base64
	sigBytes, err := base64.StdEncoding.DecodeString(sig)
	if err != nil {
		return false, fmt.Errorf("failed to decode signature: %w", err)
	}

	// decompress the sig into a P2Affine-type (which is a BlsSig)
	signature := new(BlsSig)
	if signature.Uncompress(sigBytes) == nil {
		return false, fmt.Errorf("failed to uncompress signature for DID: %s", d.String())
	}

	// verify the sig using the pub key and the CID bytes directly
	verified := signature.Verify(true, pubKey, true, block.Cid().Bytes(), nil)

	// return the verification result
	if !verified {
		return false, fmt.Errorf("sig verification failed for DID: %s", d.String())
	}

	return true, nil
}

// ===== KeyDIDProvider =====

type BlsProvider struct {
	privKey *BlsPrivKey
}

// creates a new BLS provider
func NewBlsProvider(privKey *BlsPrivKey) (BlsProvider, error) {
	if privKey == nil {
		return BlsProvider{}, fmt.Errorf("failed to create BLS provider: private key is nil")
	}

	return BlsProvider{privKey}, nil
}

// signs a block using the BLS priv key
func (b BlsProvider) Sign(block blocks.Block) (string, error) {
	if block == nil {
		return "", fmt.Errorf("failed to sign block: block is nil")
	}

	// sign the CID using the BLS priv key
	sig := new(BlsSig).Sign(b.privKey, block.Cid().Bytes(), nil)

	// compress and encode to base64; make the sig transportable
	encodedSig := base64.StdEncoding.EncodeToString(sig.Compress())

	return encodedSig, nil
}

// ===== BLS circuit generator =====

// represents an account and its associated pub key
type Member struct {
	Account string
	DID     BlsDID
}

// helper struct that allows you to gen a BLS circuit
type BlsCircuitGenerator struct {
	members []Member
}

// create a new BLS circuit generator
func NewBlsCircuitGenerator(members []Member) *BlsCircuitGenerator {
	return &BlsCircuitGenerator{members}
}

// gen a partial BLS circuit
func (bcg BlsCircuitGenerator) Generate(msg blocks.Block) (*partialBlsCircuit, error) {
	circuit, err := newBlsCircuit(msg, bcg.CircuitMap())
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
		if m.DID == member.DID {
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
		if _, ok := seen[member.DID]; ok {
			// no duplicate members allowed, else, error
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

// ===== partial BLS circuit =====

// a partial BLS circuit that can be finalized later once all members have signed
type partialBlsCircuit struct {
	circuit *BlsCircuit // the underlying full BLS circuit
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

// add a sig to the partial BLS circuit and verify it
func (pbc *partialBlsCircuit) AddAndVerify(member Member, sig string) error {
	// check if member exists
	found := false
	for _, m := range pbc.members {
		if m.DID == member.DID {
			found = true
			break
		}
	}

	// if not found, error
	if !found {
		return fmt.Errorf("member not found: %s", member.DID)
	}

	// add and verify the signature and public key
	if err := pbc.circuit.add(member, sig); err != nil {
		return fmt.Errorf("failed to add and verify signature: %w", err)
	}

	return nil
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

	// if not all members have signed, fail the finalization
	if len(pbc.circuit.sigs) != len(pbc.members) {
		return nil, fmt.Errorf("failed to finalize BLS circuit: not all members have signed")
	}

	// aggregate signatures and build bit vector
	if err := pbc.circuit.AggregateSignatures(); err != nil {
		return nil, fmt.Errorf("failed to aggregate signatures: %w", err)
	}

	// returns the fully populated and verified BLS circuit
	return pbc.circuit, nil
}

// ===== full BLS circuit =====

// a full BLS circuit with all members signed
type BlsCircuit struct {
	msg          blocks.Block
	keyset       []BlsDID
	sigs         map[BlsDID]*BlsSig
	aggSigs      *BlsSig
	bitVector    *big.Int
	includedDIDs []BlsDID
}

// message (Block) getter
func (b *BlsCircuit) Msg() blocks.Block {
	return b.msg
}

// internal method to create a new BLS circuit
func newBlsCircuit(msg blocks.Block, keyset []BlsDID) (*BlsCircuit, error) {
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
func (b *BlsCircuit) add(member Member, sig string) error {
	pubKey := member.DID.Identifier()

	sigBytes, err := base64.StdEncoding.DecodeString(sig)
	if err != nil {
		return fmt.Errorf("failed to decode signature: %w", err)
	}

	// decompress the sig
	signature := new(BlsSig)
	if signature.Uncompress(sigBytes) == nil {
		return fmt.Errorf("failed to uncompress signature for DID: %s", member.DID.String())
	}

	// verify the sig using the pub key and the CID bytes (message)
	verified := signature.Verify(true, pubKey, true, b.msg.Cid().Bytes(), nil)
	if !verified {
		return fmt.Errorf("signature verification failed for DID: %s", member.DID.String())
	}

	// store the sig
	b.sigs[member.DID] = signature

	return nil
}

// aggregates the collected signatures and builds the bit vector
func (b *BlsCircuit) AggregateSignatures() error {
	if len(b.sigs) == 0 {
		return fmt.Errorf("no signatures to aggregate")
	}

	var sigs []*BlsSig
	b.bitVector = big.NewInt(0)
	b.includedDIDs = nil

	for idx, did := range b.keyset {
		if sig, ok := b.sigs[did]; ok {
			sigs = append(sigs, sig)
			b.includedDIDs = append(b.includedDIDs, did)
			b.bitVector.SetBit(b.bitVector, idx, 1)
		}
	}

	if len(sigs) == 0 {
		return fmt.Errorf("no signatures to aggregate")
	}

	var aggSig blst.P2Aggregate
	aggSig.Aggregate(sigs, true)
	b.aggSigs = aggSig.ToAffine()

	return nil
}

// agg sigs
func (b *BlsCircuit) AggregatedSignature() (string, error) {
	if b.aggSigs == nil {
		return "", fmt.Errorf("aggregated signature not generated")
	}
	aggSigBytes := b.aggSigs.Compress()
	aggSigEncoded := base64.StdEncoding.EncodeToString(aggSigBytes)
	return aggSigEncoded, nil
}

// returns the bit vector as a base64 encoded string
func (b *BlsCircuit) BitVector() (string, error) {
	if b.bitVector == nil {
		return "", fmt.Errorf("bit vector not generated")
	}
	h := b.bitVector.Text(16)
	if len(h)%2 != 0 {
		h = "0" + h
	}
	bvEncoded := base64.RawURLEncoding.EncodeToString([]byte(h))
	return bvEncoded, nil
}

// returns the included DIDs
func (b *BlsCircuit) IncludedDIDs() []BlsDID {
	return b.includedDIDs
}

type SerializedCircuit struct {
	CID       string `json:"cid"`
	BitVector string `json:"bv"`
}

// serialize the BLS circuit, including the bit vector
func (b *BlsCircuit) Serialize() ([]byte, error) {
	// ensure signatures are aggregated
	if b.aggSigs == nil || b.bitVector == nil {
		if err := b.AggregateSignatures(); err != nil {
			return nil, fmt.Errorf("failed to aggregate signatures: %w", err)
		}
	}

	// get the bit vector
	bvEncoded, err := b.BitVector()
	if err != nil {
		return nil, fmt.Errorf("failed to get bit vector: %w", err)
	}

	// encode CID
	cidEncoded := base64.RawURLEncoding.EncodeToString(b.msg.Cid().Bytes())

	// create the SerializedCircuit struct
	serializedCircuit := &SerializedCircuit{
		CID:       cidEncoded,
		BitVector: bvEncoded,
	}

	// marshal to JSON
	serializedData, err := json.Marshal(serializedCircuit)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal serialized circuit to JSON: %w", err)
	}

	return serializedData, nil
}

type AggregateDID struct {
	AggPubKey    BlsDID
	IncludedDIDs []BlsDID
	// include CID in case it's needed to later get the full Block
	CID string
}

// reconstructs the aggregate DID from serialized data and keyset
func DeserializeBlsCircuit(serializedData []byte, keyset []BlsDID) (*AggregateDID, error) {
	// unmarshal the JSON data
	var serialized SerializedCircuit
	err := json.Unmarshal(serializedData, &serialized)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal serialized circuit from JSON: %w", err)
	}

	// decode the bit vector
	bvBytes, err := base64.RawURLEncoding.DecodeString(serialized.BitVector)
	if err != nil {
		return nil, fmt.Errorf("failed to decode bit vector: %w", err)
	}
	bitset := new(big.Int)
	bitset.SetString(string(bvBytes), 16)

	// build the included public keys based on the bitset
	var includedPubKeys []*BlsPubKey
	var includedDIDs []BlsDID

	for idx, did := range keyset {
		if bitset.Bit(idx) == 1 {
			pubKey := did.Identifier()
			includedPubKeys = append(includedPubKeys, pubKey)
			includedDIDs = append(includedDIDs, did)
		}
	}

	if len(includedPubKeys) == 0 {
		return nil, fmt.Errorf("no public keys to aggregate")
	}

	// agg the pub keys all at once
	var aggPub blst.P1Aggregate
	aggPub.Aggregate(includedPubKeys, true)
	aggPubKey := aggPub.ToAffine()

	aggDID, err := NewBlsDID(aggPubKey)
	if err != nil {
		return nil, fmt.Errorf("failed to generate aggregate DID: %w", err)
	}

	// return the aggregate DID
	return &AggregateDID{
		AggPubKey:    aggDID,
		IncludedDIDs: includedDIDs,
		CID:          serialized.CID,
	}, nil
}

// verifies the correctness of the BLS circuit by comparing the aggregate signature with the dynamically aggregated pub keys
//
// returns whether it's valid and which BLS DIDs were part of the valid sig aggregation
// "sig": aggregated sig as base64 string
// "bv": bit vector as base64 string (rawurlencoded base64 of bitset.Text(16))
func (b *BlsCircuit) Verify(sig string, bv string) (bool, []BlsDID, error) {
	if b.msg == nil {
		return false, nil, fmt.Errorf("block is nil")
	}

	// decode the sig from base64
	sigBytes, err := base64.StdEncoding.DecodeString(sig)
	if err != nil {
		return false, nil, fmt.Errorf("failed to decode signature: %w", err)
	}

	// decompress the aggregated sig
	aggSignature := new(BlsSig)
	if aggSignature.Uncompress(sigBytes) == nil {
		return false, nil, fmt.Errorf("failed to uncompress aggregated signature")
	}

	// decode the bit vector from base64
	bvBytes, err := base64.RawURLEncoding.DecodeString(bv)
	if err != nil {
		return false, nil, fmt.Errorf("failed to decode bit vector: %w", err)
	}

	// rebuild the bitset from the hex string
	bitset := new(big.Int)
	bitset.SetString(string(bvBytes), 16)

	// build the included public keys based on the bitset
	var includedPubKeys []*BlsPubKey
	var includedDIDs []BlsDID

	for idx, did := range b.keyset {
		if bitset.Bit(idx) == 1 {
			pubKey := did.Identifier()
			includedPubKeys = append(includedPubKeys, pubKey)
			includedDIDs = append(includedDIDs, did)
		}
	}

	if len(includedPubKeys) == 0 {
		return false, nil, fmt.Errorf("no public keys to verify")
	}

	// aggregate the pub keys
	var aggPub blst.P1Aggregate
	// agg all the pub keys at once
	aggPub.Aggregate(includedPubKeys, true)
	aggPubKey := aggPub.ToAffine()

	// verify the aggregated sig
	verified := aggSignature.Verify(true, aggPubKey, true, b.msg.Cid().Bytes(), nil)

	return verified, includedDIDs, nil
}
