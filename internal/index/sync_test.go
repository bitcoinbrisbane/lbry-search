package index

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bitcoinbrisbane/lbry-search/internal/chain"
	"github.com/bitcoinbrisbane/lbry-search/internal/lbcd"
	"github.com/bitcoinbrisbane/lbry-search/internal/store"
)

// A block that starts mid-chain touches claims the index never saw: those are
// skipped, and claims created in it are indexed as usual.
func TestApplyMidChainSkipsUnseenClaims(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "idx.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	unseen := strings.Repeat("ab", 20) // a claim created below the start height
	elsewhere := newTx(nil)            // its claim and support outputs
	upd := newTx([]chain.OutPoint{out(elsewhere, 0)},
		updateOut("old-video", unseen, streamValue("", "Retitled old video", 60), 1e7))
	sup := newTx(nil, supportOut("old-video", unseen, 5e8))
	spend := newTx([]chain.OutPoint{out(elsewhere, 1)}, chain.TxOut{Value: 1, Script: p2pkh})
	fresh := newTx(nil, claimOut("new-video", streamValue("", "A brand new kayak video", 60), 1e7))
	freshID := chain.ClaimID(fresh.Hash, 0)
	must(t, ApplyBlock(s, 1000, "h1000", &chain.Block{Txs: []*chain.Tx{upd, sup, spend, fresh}}, true))

	if got := search(t, s, "retitled"); len(got) != 0 {
		t.Fatalf("an update of an unseen claim isn't indexed: %+v", got)
	}
	if r, err := s.Claim(unseen); err != nil || r != nil {
		t.Fatalf("unseen claim stays out: %+v %v", r, err)
	}
	if got := search(t, s, "kayak"); len(got) != 1 || got[0].ClaimID != freshID {
		t.Fatalf("new claim indexed: %+v", got)
	}
	// Spending the support later is quiet too.
	must(t, ApplyBlock(s, 1001, "h1001", &chain.Block{Txs: []*chain.Tx{
		newTx([]chain.OutPoint{out(sup, 0)}, chain.TxOut{Value: 1, Script: p2pkh})}}, true))
}

// fakeNode is a pruned lbcd serving empty blocks from pruneHeight to tip.
type fakeNode struct {
	pruneHeight, tip int64
	raw              map[int64]string // height -> block hex
	hash             map[int64]string // height -> display hash
	onCount          func(calls int)

	mu     sync.Mutex
	holds  []int64
	counts int
}

func newFakeNode(pruneHeight, tip int64) *fakeNode {
	n := &fakeNode{pruneHeight: pruneHeight, tip: tip, raw: map[int64]string{}, hash: map[int64]string{}}
	prev := make([]byte, 32)
	for h := pruneHeight; h <= tip; h++ {
		hdr := make([]byte, chain.HeaderSize)
		binary.LittleEndian.PutUint32(hdr[0:4], 1)
		copy(hdr[4:36], prev)
		binary.LittleEndian.PutUint32(hdr[100:104], uint32(1_600_000_000+h))
		n.raw[h] = hex.EncodeToString(append(hdr, 0)) // no transactions
		prev = chain.DoubleSHA256(hdr)
		n.hash[h] = chain.DisplayHex(prev)
	}
	return n
}

func (n *fakeNode) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Method string            `json:"method"`
		Params []json.RawMessage `json:"params"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	var h int64
	if len(req.Params) > 0 {
		json.Unmarshal(req.Params[0], &h)
	}
	var result any
	switch req.Method {
	case "getblockchaininfo":
		result = map[string]any{"pruned": true, "pruneheight": n.pruneHeight}
	case "setprunehold":
		n.mu.Lock()
		n.holds = append(n.holds, h)
		n.mu.Unlock()
	case "getblockcount":
		n.mu.Lock()
		n.counts++
		c := n.counts
		n.mu.Unlock()
		if n.onCount != nil {
			n.onCount(c)
		}
		result = n.tip
	case "getblockhash":
		result = n.hash[h]
	case "getblock":
		var hash string
		json.Unmarshal(req.Params[0], &hash)
		for height, hh := range n.hash {
			if hh == hash {
				result = n.raw[height]
			}
		}
	}
	json.NewEncoder(w).Encode(map[string]any{"result": result, "error": nil})
}

func TestSyncFromPrunedHeight(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "idx.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	node := newFakeNode(1000, 1004)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	node.onCount = func(calls int) {
		if calls == 2 { // the first pass indexed everything
			cancel()
		}
	}
	srv := httptest.NewServer(node)
	defer srv.Close()

	syncer := &Syncer{Store: s, Node: lbcd.New(srv.URL, "", ""), Workers: 2, Poll: 10 * time.Millisecond, From: 1000}
	if err := syncer.Run(ctx); err != context.Canceled {
		t.Fatalf("run: %v", err)
	}
	if h, hash, _ := s.Tip(); h != 1004 || hash != node.hash[1004] {
		t.Fatalf("tip %d %s", h, hash)
	}
	if hash, _ := s.BlockHash(1000); hash != node.hash[1000] {
		t.Fatalf("started at 1000: %q", hash)
	}
	if hash, _ := s.BlockHash(999); hash != "" {
		t.Fatalf("nothing below the start: %q", hash)
	}
	if len(node.holds) == 0 || node.holds[0] != 999 || node.holds[len(node.holds)-1] != 1004 {
		t.Fatalf("prune holds %v", node.holds)
	}
}

func TestSyncRefusesPrunedGapAndHoldsAtGenesis(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "idx.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	node := newFakeNode(1000, 1004)
	srv := httptest.NewServer(node)
	defer srv.Close()

	syncer := &Syncer{Store: s, Node: lbcd.New(srv.URL, "", "")}
	err = syncer.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "-from 1000") {
		t.Fatalf("want a pruned-gap error suggesting -from 1000, got %v", err)
	}
	// -1 would lift the hold, letting the node delete blocks before we read them.
	if len(node.holds) != 1 || node.holds[0] != 0 {
		t.Fatalf("an empty index holds at 0, got %v", node.holds)
	}
}
