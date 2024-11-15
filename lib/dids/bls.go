package dids

import (
	"encoding/base64"
	"fmt"
	"math/big"

	"github.com/ipfs/go-cid"
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

// verifies if the sig is valid for the cid
func (d BlsDID) Verify(cid cid.Cid, sig string) (bool, error) {

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
	// and return this directly
	return signature.Verify(true, pubKey, true, cid.Bytes(), nil), nil
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

// signs a cid using the BLS priv key
func (b BlsProvider) Sign(cid cid.Cid) (string, error) {

	// sign the CID using the BLS priv key
	sig := new(BlsSig).Sign(b.privKey, cid.Bytes(), nil)

	// compress and encode to base64; make the sig transportable
	return base64.StdEncoding.EncodeToString(sig.Compress()), nil
}

// ===== BLS circuit generator =====

// represents an account and its associated pub key
type Member struct {
	Account string // likely account username
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
func (bcg *BlsCircuitGenerator) Generate(cid cid.Cid) (*partialBlsCircuit, error) {
	circuit, err := newBlsCircuit(&cid, bcg.CircuitMap())
	if err != nil {
		return nil, err
	}

	return &partialBlsCircuit{
		circuit: circuit,
		members: bcg.members,
	}, nil
}

// adds a member to the list of "members that must sign to finalize this circuit" list
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

// view the message (cid)
func (pbc *partialBlsCircuit) Cid() cid.Cid {
	return *pbc.circuit.cid
}

// add a sig to the partial BLS circuit and verify it
// from @vaultec: members should be predefined in the structure before AddAndVerify
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
	// if the cid (message) is nil, fail the finalization
	if pbc.circuit.cid == nil {
		return nil, fmt.Errorf("failed to finalize BLS circuit: cid is nil")
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
	if err := pbc.circuit.aggregateSignatures(); err != nil {
		return nil, fmt.Errorf("failed to aggregate signatures: %w", err)
	}

	// returns the fully populated and verified BLS circuit
	return pbc.circuit, nil
}

// ===== full BLS circuit =====

// a full BLS circuit with all members signed
type BlsCircuit struct {
	// === core internal structure of what is a BLS circuit ===
	cid       *cid.Cid
	aggSig    *BlsSig
	bitVector *big.Int
	// === external input vars ===
	// order is important here so we can use this
	// in combo with the bit vector
	keyset []BlsDID
	// sigs we construct when we call `add` with
	// each member's sig
	sigs map[BlsDID]*BlsSig
}

// message (cid) getter
func (b *BlsCircuit) Cid() cid.Cid {
	return *b.cid
}

// internal method to create a new BLS circuit
func newBlsCircuit(cid *cid.Cid, keyset []BlsDID) (*BlsCircuit, error) {
	if cid == nil {
		return nil, fmt.Errorf("failed to create BLS circuit: cid is nil")
	}
	return &BlsCircuit{
		cid:    cid,
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
	verified := signature.Verify(true, pubKey, true, b.cid.Bytes(), nil)
	if !verified {
		return fmt.Errorf("signature verification failed for DID: %s", member.DID.String())
	}

	// store the sig
	b.sigs[member.DID] = signature

	return nil
}

// aggregates the collected signatures and builds the bit vector
func (b *BlsCircuit) aggregateSignatures() error {
	if len(b.sigs) == 0 {
		return fmt.Errorf("no signatures to aggregate")
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

	var aggSig blst.P2Aggregate
	aggSig.Aggregate(sigs, true)
	b.aggSig = aggSig.ToAffine()

	return nil
}

// returns the aggregated signature (`internally aggSig`) as a base64 encoded string
func (b *BlsCircuit) AggregatedSignature() (string, error) {
	if b.aggSig == nil {
		return "", fmt.Errorf("aggregated signature not generated")
	}
	aggSigBytes := b.aggSig.Compress()
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

// returns the included DIDs in the circuit
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
	// ensure signatures are aggregated (if not, we can quickly do it here)
	if b.aggSig == nil || b.bitVector == nil {
		if err := b.aggregateSignatures(); err != nil {
			return nil, fmt.Errorf("failed to aggregate signatures: %w", err)
		}
	}

	// get the bit vector
	bvEncoded, err := b.BitVector()
	if err != nil {
		return nil, fmt.Errorf("failed to get bit vector: %w", err)
	}

	// get the encoded aggregated signature
	sig, err := b.AggregatedSignature()
	if err != nil {
		return nil, fmt.Errorf("failed to get sig: %w", err)
	}

	// create the SerializedCircuit struct
	return &SerializedCircuit{
		Signature: sig,
		BitVector: bvEncoded,
	}, nil
}

// reconstructs the aggregate DID from serialized data and keyset
func DeserializeBlsCircuit(serialized SerializedCircuit, keyset []BlsDID, cid cid.Cid) (*BlsCircuit, error) {
	// decode the bit vector
	bvBytes, err := base64.RawURLEncoding.DecodeString(serialized.BitVector)
	if err != nil {
		return nil, fmt.Errorf("failed to decode bit vector: %w", err)
	}
	bitset := new(big.Int)
	bitset.SetString(string(bvBytes), 16)

	// build the included public keys based on the bitset
	var includedPubKeys []*BlsPubKey
	for idx, did := range keyset {
		if bitset.Bit(idx) == 1 {
			// if the bit is set, then we know
			// that this DID was included in the aggregation
			pubKey := did.Identifier()
			includedPubKeys = append(includedPubKeys, pubKey)
		}
	}

	if len(includedPubKeys) == 0 {
		return nil, fmt.Errorf("no public keys to aggregate")
	}

	// agg the pub keys all at once
	// as internally this is the
	// fastest way to do it
	var aggPub blst.P1Aggregate
	aggPub.Aggregate(includedPubKeys, true)
	aggPubKey := aggPub.ToAffine()

	_, err = NewBlsDID(aggPubKey)
	if err != nil {
		return nil, fmt.Errorf("failed to generate aggregate DID: %w", err)
	}

	sigBytes, err := base64.StdEncoding.DecodeString(serialized.Signature)
	if err != nil {
		return nil, fmt.Errorf("failed to decode signature: %w", err)
	}

	aggSignature := new(BlsSig)
	if aggSignature.Uncompress(sigBytes) == nil {
		return nil, fmt.Errorf("failed to uncompress aggregated signature")
	}

	// return the new circuit
	return &BlsCircuit{
		aggSig:    aggSignature,
		bitVector: bitset,
		keyset:    keyset,
		cid:       &cid,
		// we don't need the `sigs` field here, because we don't use `sigs` in any of the
		// public methods of `BlsCircuit`, although it might be expected
	}, nil
}

// verifies the correctness of the BLS circuit by checking the aggregated signature
// and ensuring that the included DIDs are correct and their aggregated signature matches
// the CID that they signed
func (b *BlsCircuit) Verify() (bool, []BlsDID, error) {
	if b.cid == nil {
		return false, nil, fmt.Errorf("cid is nil")
	}

	// should have already been aggregated when the circuit was finalized
	// but just as a "sanity check" we'll ensure they are before we proceed
	// @vaultec note: in production we won't have access to the original signatures

	_, err := b.BitVector()
	if err != nil {
		return false, nil, fmt.Errorf("failed to get bit vector: %w", err)
	}

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

	if len(includedPubKeys) == 0 || len(includedDIDs) == 0 { // checking both semi-redundantly to be safe and prevent against future changes
		return false, nil, fmt.Errorf("no included DIDs to verify")
	}

	// aggregate the pub keys
	var aggPub blst.P1Aggregate
	// agg all the pub keys at once
	aggPub.Aggregate(includedPubKeys, true)
	aggPubKey := aggPub.ToAffine()

	// verify the aggregated sig and return results
	return b.aggSig.Verify(true, aggPubKey, true, b.cid.Bytes(), nil), includedDIDs, nil
}
