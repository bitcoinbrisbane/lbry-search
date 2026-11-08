package index

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/bitcoinbrisbane/lbry-search/internal/chain"
	"github.com/bitcoinbrisbane/lbry-search/internal/lbcd"
	"github.com/bitcoinbrisbane/lbry-search/internal/store"
)

// Syncer follows lbcd's best chain into the store.
type Syncer struct {
	Store   *store.Store
	Node    *lbcd.Client
	Workers int           // blocks fetched in parallel during catch-up
	Poll    time.Duration // how often to look for new blocks at the tip
}

type fetched struct {
	height int64
	hash   string
	block  *chain.Block
	err    error
}

// Run syncs until ctx is cancelled.
func (s *Syncer) Run(ctx context.Context) error {
	if s.Workers <= 0 {
		s.Workers = 8
	}
	if s.Poll <= 0 {
		s.Poll = 10 * time.Second
	}
	lastLog := time.Now()
	for {
		tip, err := s.Node.BlockCount(ctx)
		if err != nil {
			log.Printf("lbcd: %v (retrying)", err)
			if !sleep(ctx, s.Poll) {
				return ctx.Err()
			}
			continue
		}
		have, _, err := s.Store.Tip()
		if err != nil {
			return err
		}
		if have >= tip {
			if !sleep(ctx, s.Poll) {
				return ctx.Err()
			}
			continue
		}
		end := have + int64(s.Workers*16)
		if end > tip {
			end = tip
		}
		blocks := s.fetchRange(ctx, have+1, end)
		for _, f := range blocks {
			if f.err != nil {
				log.Printf("block %d: %v (retrying)", f.height, f.err)
				break
			}
			if reorg, err := s.checkLink(ctx, f); err != nil {
				return err
			} else if reorg {
				break // start again from the rolled-back tip
			}
			keepUndo := f.height > tip-store.UndoDepth
			if err := ApplyBlock(s.Store, f.height, f.hash, f.block, keepUndo); err != nil {
				return fmt.Errorf("apply %d: %w", f.height, err)
			}
			if time.Since(lastLog) > 15*time.Second || f.height == tip {
				log.Printf("indexed %d / %d", f.height, tip)
				lastLog = time.Now()
			}
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
}

// checkLink rolls back if the block doesn't build on what we indexed below it.
func (s *Syncer) checkLink(ctx context.Context, f fetched) (bool, error) {
	if f.height == 0 {
		return false, nil
	}
	below, err := s.Store.BlockHash(f.height - 1)
	if err != nil || below == "" || below == chain.DisplayHex(f.block.PrevHash) {
		return false, err
	}
	// Walk down until our hash matches the node's again.
	for h := f.height - 1; h >= 0 && h >= f.height-store.UndoDepth; h-- {
		ours, err := s.Store.BlockHash(h)
		if err != nil {
			return false, err
		}
		theirs, err := s.Node.BlockHash(ctx, h)
		if err != nil {
			return false, err
		}
		if ours == theirs {
			log.Printf("reorg: rolling back to %d", h)
			return true, s.Store.RollbackTo(h)
		}
	}
	return false, fmt.Errorf("reorg deeper than %d blocks below %d: rebuild the index", store.UndoDepth, f.height)
}

// fetchRange downloads and parses blocks in parallel, returned in height order.
func (s *Syncer) fetchRange(ctx context.Context, from, to int64) []fetched {
	out := make([]fetched, to-from+1)
	jobs := make(chan int64)
	done := make(chan struct{})
	for w := 0; w < s.Workers; w++ {
		go func() {
			for h := range jobs {
				f := fetched{height: h}
				f.hash, f.err = s.Node.BlockHash(ctx, h)
				var raw []byte
				if f.err == nil {
					raw, f.err = s.Node.RawBlock(ctx, f.hash)
				}
				if f.err == nil {
					f.block, f.err = chain.ParseBlock(raw)
				}
				out[h-from] = f
			}
			done <- struct{}{}
		}()
	}
	for h := from; h <= to; h++ {
		jobs <- h
	}
	close(jobs)
	for w := 0; w < s.Workers; w++ {
		<-done
	}
	return out
}

func sleep(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}
