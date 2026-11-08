package store

import (
	"database/sql"
	"strings"
)

// Record is a claim with everything the index keeps about it (the GraphQL API's Claim).
type Record struct {
	ClaimID       string
	Name          string
	TxID          string
	Nout          int
	Amount        int64 // dewies on the claim
	SupportAmount int64 // dewies in unspent supports
	Height        int64 // created
	UpdatedHeight int64
	Type          string
	ChannelID     string
	ChannelName   string
	Title         string
	Description   string
	Author        string
	Tags          []string
	Languages     []string
	Thumbnail     string
	MediaType     string
	Duration      uint32
	ReleaseTime   int64
	RepostOf      string
	FeeAmount     int64
	FeeCurrency   string
}

// Staked is the claim's amount plus its supports, in dewies.
func (r *Record) Staked() int64 { return r.Amount + r.SupportAmount }

// URL is the claim's full lbry:// URL (channel/name for signed streams).
func (r *Record) URL() string {
	if r.ChannelName != "" && r.ChannelID != "" && r.Type != "channel" {
		return "lbry://" + r.ChannelName + "#" + r.ChannelID + "/" + r.Name + "#" + r.ClaimID
	}
	return "lbry://" + r.Name + "#" + r.ClaimID
}

const recordColumns = `claim_id, name, txid, nout, amount, support_amount, height, updated_height, type,
	channel_id, channel_name, title, description, author, tags, languages, thumbnail, media_type,
	duration, release_time, repost_of, fee_amount, fee_currency`

func scanRecord(row interface{ Scan(...any) error }) (*Record, error) {
	var r Record
	var tags, langs string
	err := row.Scan(&r.ClaimID, &r.Name, &r.TxID, &r.Nout, &r.Amount, &r.SupportAmount, &r.Height,
		&r.UpdatedHeight, &r.Type, &r.ChannelID, &r.ChannelName, &r.Title, &r.Description, &r.Author,
		&tags, &langs, &r.Thumbnail, &r.MediaType, &r.Duration, &r.ReleaseTime, &r.RepostOf,
		&r.FeeAmount, &r.FeeCurrency)
	if err != nil {
		return nil, err
	}
	r.Tags = strings.Fields(tags)
	r.Languages = strings.Fields(langs)
	return &r, nil
}

// Claim returns a live (not abandoned) claim, or nil.
func (s *Store) Claim(id string) (*Record, error) {
	r, err := scanRecord(s.db.QueryRow(`SELECT `+recordColumns+` FROM claims WHERE claim_id = ? AND abandoned = 0`, id))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return r, err
}

// Claims returns the live claims among ids, keyed by claim ID.
func (s *Store) Claims(ids []string) (map[string]*Record, error) {
	out := make(map[string]*Record, len(ids))
	for len(ids) > 0 {
		batch := ids
		if len(batch) > 500 {
			batch = batch[:500]
		}
		ids = ids[len(batch):]
		args := make([]any, len(batch))
		for i, id := range batch {
			args[i] = id
		}
		rows, err := s.db.Query(`SELECT `+recordColumns+` FROM claims WHERE abandoned = 0 AND claim_id IN (?`+
			strings.Repeat(",?", len(batch)-1)+`)`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			r, err := scanRecord(rows)
			if err != nil {
				rows.Close()
				return nil, err
			}
			out[r.ClaimID] = r
		}
		err = rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// ChannelPage is where a page of a channel's claims starts: after the claim at
// (Height, ClaimID) in newest-first order, or from the newest when zero.
type ChannelPage struct {
	Height  int64
	ClaimID string
}

// ChannelClaims lists a channel's live claims, newest first (by the block they
// were made in), up to limit after the given position. claimType filters ("" for all).
func (s *Store) ChannelClaims(channelID, claimType string, after ChannelPage, limit int) ([]*Record, error) {
	q := `SELECT ` + recordColumns + ` FROM claims WHERE channel_id = ? AND abandoned = 0`
	args := []any{channelID}
	if claimType != "" {
		q += ` AND type = ?`
		args = append(args, claimType)
	}
	if after.ClaimID != "" {
		q += ` AND (height < ? OR (height = ? AND claim_id < ?))`
		args = append(args, after.Height, after.Height, after.ClaimID)
	}
	q += ` ORDER BY height DESC, claim_id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Record
	for rows.Next() {
		r, err := scanRecord(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
