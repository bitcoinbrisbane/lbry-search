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

	"github.com/bitcoinbrisbane/lbry-search/internal/index"
	"github.com/bitcoinbrisbane/lbry-search/internal/lbcd"
	"github.com/bitcoinbrisbane/lbry-search/internal/store"
)

func main() {
	dbPath := flag.String("db", "lbry-search.db", "SQLite index file")
	rpc := flag.String("rpc", "http://127.0.0.1:9245", "lbcd JSON-RPC URL")
	user := flag.String("rpcuser", os.Getenv("LBCD_RPCUSER"), "lbcd RPC user (or LBCD_RPCUSER)")
	pass := flag.String("rpcpass", os.Getenv("LBCD_RPCPASS"), "lbcd RPC password (or LBCD_RPCPASS)")
	workers := flag.Int("workers", 8, "blocks fetched in parallel while catching up")
	poll := flag.Duration("poll", 10*time.Second, "how often to check for new blocks at the tip")
	from := flag.Int64("from", 0, "height an empty index starts at, e.g. a pruned lbcd's pruneheight "+
		"(claims created below it aren't indexed)")
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
	switch {
	case h < 0 && *from > 0:
		log.Printf("empty index, starting at height %d; following %s", *from, *rpc)
	case h >= 0 && *from > 0:
		log.Printf("index at height %d (ignoring -from, which only applies to an empty index); following %s", h, *rpc)
	default:
		log.Printf("index at height %d; following %s", h, *rpc)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	syncer := &index.Syncer{Store: s, Node: lbcd.New(*rpc, *user, *pass), Workers: *workers, Poll: *poll, From: *from}
	if err := syncer.Run(ctx); err != nil && ctx.Err() == nil {
		log.Fatal(err)
	}
	log.Print("stopped")
}
