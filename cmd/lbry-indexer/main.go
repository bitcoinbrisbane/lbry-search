// lbry-indexer follows an LBRY full node (lbcd) and keeps a SQLite full-text index of
// every stream and channel claim, for lbry-search to serve.
//
//	lbry-indexer -db lbry-search.db -rpc http://127.0.0.1:9245 -rpcuser lbry -rpcpass secret
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/LBRYFoundation/lbry-search/internal/index"
	"github.com/LBRYFoundation/lbry-search/internal/lbcd"
	"github.com/LBRYFoundation/lbry-search/internal/store"
)

func main() {
	dbPath := flag.String("db", "lbry-search.db", "SQLite index file")
	rpc := flag.String("rpc", "http://127.0.0.1:9245", "lbcd JSON-RPC URL")
	user := flag.String("rpcuser", os.Getenv("LBCD_RPCUSER"), "lbcd RPC user (or LBCD_RPCUSER)")
	pass := flag.String("rpcpass", os.Getenv("LBCD_RPCPASS"), "lbcd RPC password (or LBCD_RPCPASS)")
	workers := flag.Int("workers", 8, "blocks fetched in parallel while catching up")
	poll := flag.Duration("poll", 10*time.Second, "how often to check for new blocks at the tip")
	flag.Parse()

	s, err := store.Open(*dbPath)
	if err != nil {
		log.Fatal(err)
	}
	defer s.Close()
	h, _, err := s.Tip()
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("index at height %d; following %s", h, *rpc)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	syncer := &index.Syncer{Store: s, Node: lbcd.New(*rpc, *user, *pass), Workers: *workers, Poll: *poll}
	if err := syncer.Run(ctx); err != nil && ctx.Err() == nil {
		log.Fatal(err)
	}
	log.Print("stopped")
}
