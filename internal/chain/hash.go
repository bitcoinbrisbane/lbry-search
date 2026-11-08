package chain

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
)

// DoubleSHA256 is Bitcoin-style sha256(sha256(b)).
func DoubleSHA256(b []byte) []byte {
	first := sha256.Sum256(b)
	second := sha256.Sum256(first[:])
	return second[:]
}

// Hash160 is ripemd160(sha256(b)).
func Hash160(b []byte) []byte {
	s := sha256.Sum256(b)
	return RIPEMD160(s[:])
}

// Reverse returns a reversed copy (internal byte order ↔ display order).
func Reverse(b []byte) []byte {
	out := make([]byte, len(b))
	for i := range b {
		out[i] = b[len(b)-1-i]
	}
	return out
}

// DisplayHex is the reversed hex used for txids, block hashes and claim IDs.
func DisplayHex(b []byte) string { return hex.EncodeToString(Reverse(b)) }

// ClaimID of a new claim: hash160(txhash ‖ nout big-endian), shown reversed.
// txHash is in internal byte order (sha256d of the transaction, not reversed).
func ClaimID(txHash []byte, nout uint32) string {
	buf := make([]byte, 36)
	copy(buf, txHash)
	binary.BigEndian.PutUint32(buf[32:], nout)
	return DisplayHex(Hash160(buf))
}
