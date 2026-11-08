// Package index turns blocks into store changes: new claims, updates, supports,
// abandons and spent supports.
package index

import (
	"fmt"

	"github.com/LBRYFoundation/lbry-search/internal/chain"
	"github.com/LBRYFoundation/lbry-search/internal/claim"
	"github.com/LBRYFoundation/lbry-search/internal/store"
)

// ApplyBlock writes one block's claim activity at height. keepUndo makes it possible
// to roll the block back (needed within store.UndoDepth of the node's tip).
func ApplyBlock(s *store.Store, height int64, hash string, blk *chain.Block, keepUndo bool) error {
	tx, err := s.Begin(height, hash, keepUndo)
	if err != nil {
		return err
	}
	for _, t := range blk.Txs {
		if err := applyTx(tx, t); err != nil {
			tx.Rollback()
			return fmt.Errorf("tx %s: %w", t.ID(), err)
		}
	}
	return tx.Commit()
}

func applyTx(tx *store.Tx, t *chain.Tx) error {
	// Claims updated in this transaction: spending their old output isn't an abandon.
	updated := map[string]bool{}
	for _, out := range t.Outputs {
		if cs := chain.ParseClaimScript(out.Script); cs.Kind == chain.UpdateClaim {
			updated[cs.ClaimID] = true
		}
	}
	if !t.IsCoinbase() {
		for _, in := range t.Inputs {
			sp, err := tx.Spend(in.Prev.Key())
			if err != nil {
				return err
			}
			if sp != nil && sp.Kind == "claim" && !updated[sp.ClaimID] {
				if err := tx.Abandon(sp.ClaimID); err != nil {
					return err
				}
			}
		}
	}
	txid := t.ID()
	for i, out := range t.Outputs {
		cs := chain.ParseClaimScript(out.Script)
		nout := uint32(i)
		switch cs.Kind {
		case chain.ClaimName, chain.UpdateClaim:
			id := cs.ClaimID
			if cs.Kind == chain.ClaimName {
				id = chain.ClaimID(t.Hash, nout)
			}
			m := claim.Decode(cs.Value)
			if err := tx.PutClaim(store.Claim{
				ClaimID: id, Name: string(cs.Name), TxID: txid, Nout: nout, Amount: out.Value,
				Type: string(m.Type), Version: m.Version, ChannelID: m.ChannelID, Title: m.Title,
				Description: m.Description, Author: m.Author, Tags: m.Tags, Languages: m.Languages,
				Thumbnail: m.Thumbnail, MediaType: m.MediaType, Duration: m.Duration, ReleaseTime: m.ReleaseTime,
				RepostOf: m.RepostOf, FeeAmount: m.FeeAmount, FeeCurrency: m.FeeCurrency,
			}, cs.Kind == chain.UpdateClaim); err != nil {
				return err
			}
		case chain.SupportClaim:
			if err := tx.PutSupport(fmt.Sprintf("%s:%d", txid, nout), cs.ClaimID, out.Value); err != nil {
				return err
			}
		}
	}
	return nil
}
