package chain

import (
	"encoding/binary"
	"encoding/hex"
	"testing"
)

func TestRIPEMD160(t *testing.T) {
	for in, want := range map[string]string{
		"":               "9c1185a5c5e9fc54612808977ee8f548b2258d31",
		"abc":            "8eb208f7e05d987a9b044a8e98c6b087f15a0bfc",
		"message digest": "5d0689ef49d2fae572b881b123a85ffa21595f36",
		"12345678901234567890123456789012345678901234567890123456789012345678901234567890": "9b752e45573d4b39f4dbd3323cab82bf63326bfb",
	} {
		if got := hex.EncodeToString(RIPEMD160([]byte(in))); got != want {
			t.Errorf("ripemd160(%q) = %s, want %s", in, got, want)
		}
	}
}

func push(b []byte) []byte {
	if len(b) < 0x4c {
		return append([]byte{byte(len(b))}, b...)
	}
	return append([]byte{0x4c, byte(len(b))}, b...)
}

var p2pkh = append(append([]byte{0x76, 0xa9, 0x14}, make([]byte, 20)...), 0x88, 0xac)

func varint(n int) []byte { return []byte{byte(n)} }

func tx(inputs [][]byte, outputs [][]byte) []byte {
	out := []byte{1, 0, 0, 0}
	out = append(out, varint(len(inputs))...)
	for _, in := range inputs {
		out = append(out, in...)
	}
	out = append(out, varint(len(outputs))...)
	for _, o := range outputs {
		out = append(out, o...)
	}
	return append(out, 0, 0, 0, 0)
}

func input(prevHash []byte, idx uint32) []byte {
	b := append([]byte{}, prevHash...)
	b = binary.LittleEndian.AppendUint32(b, idx)
	return append(b, 0, 0xff, 0xff, 0xff, 0xff)
}

func output(value uint64, script []byte) []byte {
	b := binary.LittleEndian.AppendUint64(nil, value)
	return append(append(b, varint(len(script))...), script...)
}

func TestParseTxAndClaimScripts(t *testing.T) {
	claimScript := append(append(append([]byte{0xb5}, push([]byte("hello"))...), push([]byte{0x00, 0x42})...), 0x6d, 0x75)
	claimScript = append(claimScript, p2pkh...)
	id := make([]byte, 20)
	id[0] = 0x01
	support := append(append(append([]byte{0xb6}, push([]byte("hello"))...), push(id)...), 0x6d, 0x75)
	support = append(support, p2pkh...)
	update := append(append(append(append([]byte{0xb7}, push([]byte("hello"))...), push(id)...), push([]byte{0x00})...), 0x6d, 0x6d)
	update = append(update, p2pkh...)
	supportData := append(append(append(append([]byte{0xb6}, push([]byte("hello"))...), push(id)...), push([]byte("x"))...), 0x6d, 0x6d)

	prev := make([]byte, 32)
	prev[0] = 9
	raw := tx([][]byte{input(prev, 3)}, [][]byte{output(100000000, claimScript), output(5, support), output(7, update), output(1, supportData), output(2, p2pkh)})
	parsed, err := ParseTx(raw)
	if err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(parsed.Hash) != hex.EncodeToString(DoubleSHA256(raw)) {
		t.Fatal("txid")
	}
	if len(parsed.Inputs) != 1 || parsed.Inputs[0].Prev.Index != 3 || len(parsed.Outputs) != 5 || parsed.Outputs[0].Value != 100000000 {
		t.Fatalf("%+v", parsed)
	}
	c := ParseClaimScript(parsed.Outputs[0].Script)
	if c.Kind != ClaimName || string(c.Name) != "hello" || hex.EncodeToString(c.Value) != "0042" {
		t.Fatalf("%+v", c)
	}
	s := ParseClaimScript(parsed.Outputs[1].Script)
	if s.Kind != SupportClaim || s.ClaimID != "0000000000000000000000000000000000000001" {
		t.Fatalf("%+v", s)
	}
	u := ParseClaimScript(parsed.Outputs[2].Script)
	if u.Kind != UpdateClaim || u.ClaimID != s.ClaimID {
		t.Fatalf("%+v", u)
	}
	if d := ParseClaimScript(parsed.Outputs[3].Script); d.Kind != SupportClaim || string(d.Value) != "x" {
		t.Fatalf("%+v", d)
	}
	if ParseClaimScript(parsed.Outputs[4].Script).Kind != NotClaim {
		t.Fatal("p2pkh is not a claim")
	}
}

// Same algorithm as lbry-sdk Output.claim_hash and lbcd change.NewClaimID, checked
// against Python's hashlib in the commit message's vector.
func TestClaimID(t *testing.T) {
	txHash := make([]byte, 32)
	for i := range txHash {
		txHash[i] = byte(i)
	}
	if got := ClaimID(txHash, 1); got != claimIDVector {
		t.Fatalf("claim id %s, want %s", got, claimIDVector)
	}
}

// hashlib: ripemd160(sha256(bytes(range(32)) + struct.pack('>I', 1))).digest()[::-1].hex()
const claimIDVector = "c09474ff1a824d0d76d1bd32b83b6f201d4b8a22"
