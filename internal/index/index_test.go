package index

import (
	"crypto/sha256"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/LBRYFoundation/lbry-search/internal/chain"
	"github.com/LBRYFoundation/lbry-search/internal/store"
)

// --- building claims and transactions by hand ---

func push(b []byte) []byte {
	if len(b) < 0x4c {
		return append([]byte{byte(len(b))}, b...)
	}
	return append([]byte{0x4c, byte(len(b))}, b...)
}
func pbVarint(v uint64) []byte {
	var out []byte
	for v >= 0x80 {
		out = append(out, byte(v)|0x80)
		v >>= 7
	}
	return append(out, byte(v))
}
func pbBytes(num int, b []byte) []byte {
	return append(append(pbVarint(uint64(num<<3|2)), pbVarint(uint64(len(b)))...), b...)
}
func pbUint(num int, v uint64) []byte { return append(pbVarint(uint64(num<<3)), pbVarint(v)...) }
func cat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

var p2pkh = append(append([]byte{0x76, 0xa9, 0x14}, make([]byte, 20)...), 0x88, 0xac)

func channelValue() []byte {
	return cat([]byte{0}, pbBytes(2, nil), pbBytes(8, []byte("Sailing Jane")))
}

func streamValue(channelID, title string, duration uint64, tags ...string) []byte {
	video := pbBytes(11, pbUint(3, duration))
	src := pbBytes(1, pbBytes(4, []byte("video/mp4")))
	c := cat(pbBytes(1, cat(src, video)), pbBytes(8, []byte(title)))
	for _, t := range tags {
		c = append(c, pbBytes(11, []byte(t))...)
	}
	if channelID == "" {
		return cat([]byte{0}, c)
	}
	id := chain.Reverse(mustHex(channelID))
	return cat([]byte{1}, id, make([]byte, 64), c)
}

func mustHex(s string) []byte {
	b := make([]byte, len(s)/2)
	fmt.Sscanf(s, "%x", &b)
	return b
}

func claimOut(name string, value []byte, amount int64) chain.TxOut {
	s := cat([]byte{0xb5}, push([]byte(name)), push(value), []byte{0x6d, 0x75}, p2pkh)
	return chain.TxOut{Value: amount, Script: s}
}
func updateOut(name, claimID string, value []byte, amount int64) chain.TxOut {
	s := cat([]byte{0xb7}, push([]byte(name)), push(chain.Reverse(mustHex(claimID))), push(value), []byte{0x6d, 0x6d}, p2pkh)
	return chain.TxOut{Value: amount, Script: s}
}
func supportOut(name, claimID string, amount int64) chain.TxOut {
	s := cat([]byte{0xb6}, push([]byte(name)), push(chain.Reverse(mustHex(claimID))), []byte{0x6d, 0x75}, p2pkh)
	return chain.TxOut{Value: amount, Script: s}
}

var txCounter byte

func newTx(inputs []chain.OutPoint, outputs ...chain.TxOut) *chain.Tx {
	txCounter++
	h := sha256.Sum256([]byte{txCounter})
	t := &chain.Tx{Hash: h[:], Outputs: outputs}
	for _, in := range inputs {
		t.Inputs = append(t.Inputs, chain.TxIn{Prev: in})
	}
	if len(t.Inputs) == 0 { // fund from somewhere unrelated
		other := sha256.Sum256([]byte{txCounter, 0xff})
		t.Inputs = []chain.TxIn{{Prev: chain.OutPoint{TxHash: other[:], Index: 0}}}
	}
	return t
}

func out(t *chain.Tx, n uint32) chain.OutPoint { return chain.OutPoint{TxHash: t.Hash, Index: n} }

func search(t *testing.T, s *store.Store, text string) []store.Hit {
	t.Helper()
	hits, err := s.Search(store.Query{Text: text, Now: time.Unix(1_800_000_000, 0)})
	if err != nil {
		t.Fatal(err)
	}
	return hits
}

func TestIndexUpdatesSupportsAbandonsAndReorgs(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "idx.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// Block 1: a channel, a video signed by it, an unrelated video, and a support.
	chTx := newTx(nil, claimOut("@sailingjane", channelValue(), 1e8))
	channelID := chain.ClaimID(chTx.Hash, 0)
	vidTx := newTx(nil, claimOut("fraser-island", streamValue(channelID, "Sailing to Fraser Island", 600, "sailing", "boats"), 1e7))
	videoID := chain.ClaimID(vidTx.Hash, 0)
	otherTx := newTx(nil, claimOut("bread", streamValue("", "Sourdough on a sailing trip", 300, "cooking"), 1e7))
	supTx := newTx(nil, supportOut("fraser-island", videoID, 50e8))
	must(t, ApplyBlock(s, 1, "h1", &chain.Block{Txs: []*chain.Tx{chTx, vidTx, otherTx, supTx}}, true))

	hits := search(t, s, "sailing")
	if len(hits) != 3 {
		t.Fatalf("want channel + 2 videos, got %+v", hits)
	}
	if hits[0].ClaimID != videoID || hits[0].Staked != 1e7+50e8 || hits[0].Channel != "@sailingjane" {
		t.Fatalf("the video with 50 LBC staked first, got %+v", hits[0])
	}
	if got := search(t, s, "@SailingJane"); len(got) == 0 || got[0].ClaimID != channelID {
		t.Fatalf("an exact channel name comes first, got %+v", got)
	}
	if got := search(t, s, "frase"); len(got) != 0 {
		t.Fatal("no prefix match in plain search")
	}
	if got, _ := s.Search(store.Query{Text: "frase", Prefix: true}); len(got) != 1 || got[0].ClaimID != videoID {
		t.Fatalf("autocomplete: %+v", got)
	}
	if got, _ := s.Search(store.Query{Text: "sailing", ClaimType: "stream", MaxDuration: 400}); len(got) != 1 || got[0].Name != "bread" {
		t.Fatalf("short streams only: %+v", got)
	}

	// Block 2: retitle the video (update spends its claim output) and spend the support.
	upd := newTx([]chain.OutPoint{out(vidTx, 0), out(supTx, 0)},
		updateOut("fraser-island", videoID, streamValue(channelID, "Moreton Bay crossing", 600), 1e7))
	must(t, ApplyBlock(s, 2, "h2", &chain.Block{Txs: []*chain.Tx{upd}}, true))
	if got := search(t, s, "boats"); len(got) != 0 {
		t.Fatalf("old tags gone: %+v", got)
	}
	got := search(t, s, "moreton")
	if len(got) != 1 || got[0].ClaimID != videoID || got[0].Staked != 1e7 {
		t.Fatalf("updated, support spent: %+v", got)
	}

	// Block 3: abandon the bread video.
	must(t, ApplyBlock(s, 3, "h3", &chain.Block{Txs: []*chain.Tx{newTx([]chain.OutPoint{out(otherTx, 0)}, chain.TxOut{Value: 1, Script: p2pkh})}}, true))
	if got := search(t, s, "sourdough"); len(got) != 0 {
		t.Fatalf("abandoned: %+v", got)
	}

	// Reorg back to block 1: everything as it was.
	must(t, s.RollbackTo(1))
	if h, hash, _ := s.Tip(); h != 1 || hash != "h1" {
		t.Fatalf("tip %d %s", h, hash)
	}
	if got := search(t, s, "boats"); len(got) != 1 || got[0].Title != "Sailing to Fraser Island" || got[0].Staked != 1e7+50e8 {
		t.Fatalf("title and support restored: %+v", got)
	}
	if got := search(t, s, "sourdough"); len(got) != 1 {
		t.Fatalf("un-abandoned: %+v", got)
	}
	if got := search(t, s, "moreton"); len(got) != 0 {
		t.Fatalf("update undone: %+v", got)
	}
	// And forward again on the other branch.
	must(t, ApplyBlock(s, 2, "h2b", &chain.Block{Txs: []*chain.Tx{upd}}, true))
	if got := search(t, s, "moreton"); len(got) != 1 {
		t.Fatalf("re-applied: %+v", got)
	}

	// Moderation: excluded channel hides its videos.
	if got, _ := s.Search(store.Query{Text: "moreton", Exclude: map[string]bool{channelID: true}}); len(got) != 0 {
		t.Fatalf("excluded: %+v", got)
	}
}

func TestMatchExpression(t *testing.T) {
	if got := store.MatchExpression(`Sailing "to" Fraser-Island) NEAR(*`, true); got != `"sailing" "to" "fraser" "island" "near"*` {
		t.Fatal(got)
	}
	if store.MatchExpression("  !!! ", false) != "" {
		t.Fatal("punctuation only")
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
