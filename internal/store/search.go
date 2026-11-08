package store

import (
	"math"
	"sort"
	"strings"
	"time"
	"unicode"
)

// Query is a search request.
type Query struct {
	Text        string
	Size, From  int
	ClaimType   string // "stream", "channel" or "" for both
	MediaPrefix string // e.g. "video/" (Lighthouse's mediaType=video)
	MaxDuration uint32 // seconds, 0 = any
	MinDuration uint32
	Exclude     map[string]bool // claim IDs or channel IDs (moderation list)
	Prefix      bool            // treat the last word as a prefix (autocomplete)
	Now         time.Time
}

// Hit is one result, in Lighthouse's shape plus a few extras.
type Hit struct {
	Name      string  `json:"name"`
	ClaimID   string  `json:"claimId"`
	Title     string  `json:"title,omitempty"`
	Channel   string  `json:"channel,omitempty"`
	ChannelID string  `json:"channel_claim_id,omitempty"`
	Type      string  `json:"value_type"`
	Duration  uint32  `json:"duration,omitempty"`
	Thumbnail string  `json:"thumbnail_url,omitempty"`
	Staked    int64   `json:"effective_amount"`
	Score     float64 `json:"score"`
	repostOf  string
}

// candidates fetched by text relevance before re-ranking with stake and recency.
const candidatePool = 300

// MatchExpression turns user text into a safe FTS5 query: each word quoted, all required.
func MatchExpression(text string, prefix bool) string {
	words := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	})
	if len(words) > 12 {
		words = words[:12]
	}
	parts := make([]string, 0, len(words))
	for i, w := range words {
		p := `"` + w + `"`
		if prefix && i == len(words)-1 {
			p += "*"
		}
		parts = append(parts, p)
	}
	return strings.Join(parts, " ")
}

// Search: BM25 relevance (channel name ×8, claim name ×4, title and author ×1, tags and
// description ×0.5, like the hub's Elasticsearch weights), then boosted by LBC staked
// and freshness; channels whose name matches the whole query come first.
func (s *Store) Search(q Query) ([]Hit, error) {
	match := MatchExpression(q.Text, q.Prefix)
	if match == "" {
		return nil, nil
	}
	if q.Size <= 0 || q.Size > 100 {
		q.Size = 20
	}
	if q.Now.IsZero() {
		q.Now = time.Now()
	}
	sqlText := `SELECT c.name, c.claim_id, c.title, c.channel_name, c.channel_id, c.type, c.duration, c.thumbnail,
			c.amount + c.support_amount, c.release_time, c.height, c.repost_of,
			bm25(claims_fts, 1.0, 4.0, 8.0, 1.0, 0.5, 0.5) AS rank
		FROM claims_fts JOIN claims c ON c.rowid = claims_fts.rowid
		WHERE claims_fts MATCH ? AND c.abandoned = 0`
	args := []any{match}
	if q.ClaimType != "" {
		sqlText += ` AND c.type = ?`
		args = append(args, q.ClaimType)
	}
	if q.MediaPrefix != "" {
		sqlText += ` AND (c.type = 'channel' OR c.media_type LIKE ?)`
		args = append(args, q.MediaPrefix+"%")
	}
	if q.MaxDuration > 0 {
		sqlText += ` AND c.duration BETWEEN 1 AND ?`
		args = append(args, q.MaxDuration)
	}
	if q.MinDuration > 0 {
		sqlText += ` AND c.duration >= ?`
		args = append(args, q.MinDuration)
	}
	sqlText += ` ORDER BY rank LIMIT ?`
	args = append(args, candidatePool+q.From)

	rows, err := s.db.Query(sqlText, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	whole := strings.ReplaceAll(strings.ToLower(strings.TrimSpace(q.Text)), " ", "")
	whole = strings.TrimPrefix(whole, "@")
	var hits []Hit
	for rows.Next() {
		var h Hit
		var release, height int64
		var rank float64
		if err := rows.Scan(&h.Name, &h.ClaimID, &h.Title, &h.Channel, &h.ChannelID, &h.Type, &h.Duration,
			&h.Thumbnail, &h.Staked, &release, &height, &h.repostOf, &rank); err != nil {
			return nil, err
		}
		if q.Exclude[h.ClaimID] || (h.ChannelID != "" && q.Exclude[h.ChannelID]) {
			continue
		}
		relevance := -rank // bm25: lower is better
		lbc := float64(h.Staked) / 1e8
		stake := 1 + math.Log10(1+math.Max(lbc, 0))/2
		fresh := 1.0
		if release > 0 {
			days := q.Now.Sub(time.Unix(release, 0)).Hours() / 24
			if days >= 0 {
				fresh = 1 + 0.5*math.Exp(-days/30) // up to +50% in the first weeks
			}
		}
		h.Score = relevance * stake * fresh
		if h.Type == "channel" && strings.ToLower(strings.TrimPrefix(h.Name, "@")) == whole {
			h.Score += 1e6 // an exact channel name wins
		}
		hits = append(hits, h)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })
	if q.From >= len(hits) {
		return []Hit{}, nil
	}
	hits = hits[q.From:]
	if len(hits) > q.Size {
		hits = hits[:q.Size]
	}
	return hits, nil
}
