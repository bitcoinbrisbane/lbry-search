// Package gql is lbry-search's GraphQL API: search, autocomplete, claims and
// channels (with their claims, paged by cursor), over the same index as the
// Lighthouse-style REST API, with the moderation blocklist applied.
package gql

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/graphql-go/graphql"

	"github.com/bitcoinbrisbane/lbry-search/internal/store"
)

// MaxPage is the most items one connection returns.
const MaxPage = 100

// API holds the schema and what its resolvers read.
type API struct {
	Schema  graphql.Schema
	store   *store.Store
	blocked func() map[string]bool
}

// New builds the schema. blocked returns the current moderation list (claim and channel IDs).
func New(s *store.Store, blocked func() map[string]bool) (*API, error) {
	if blocked == nil {
		blocked = func() map[string]bool { return nil }
	}
	api := &API{store: s, blocked: blocked}
	schema, err := api.schema()
	if err != nil {
		return nil, err
	}
	api.Schema = schema
	return api, nil
}

// --- per-request claim loader -----------------------------------------------

type loaderKey struct{}

// loader caches claims for one request, so a page of results looks up their
// channels in one query rather than one each.
type loader struct {
	store *store.Store
	mu    sync.Mutex
	cache map[string]*store.Record // nil value = not in the index
}

func (a *API) withLoader(ctx context.Context) context.Context {
	return context.WithValue(ctx, loaderKey{}, &loader{store: a.store, cache: map[string]*store.Record{}})
}

func loaderFrom(ctx context.Context, s *store.Store) *loader {
	if l, ok := ctx.Value(loaderKey{}).(*loader); ok {
		return l
	}
	return &loader{store: s, cache: map[string]*store.Record{}}
}

// prime fetches any of ids not already cached, in one query.
func (l *loader) prime(ids []string) error {
	l.mu.Lock()
	var missing []string
	seen := map[string]bool{}
	for _, id := range ids {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		if _, ok := l.cache[id]; !ok {
			missing = append(missing, id)
		}
	}
	l.mu.Unlock()
	if len(missing) == 0 {
		return nil
	}
	found, err := l.store.Claims(missing)
	if err != nil {
		return err
	}
	l.mu.Lock()
	for _, id := range missing {
		l.cache[id] = found[id]
	}
	l.mu.Unlock()
	return nil
}

func (l *loader) get(id string) (*store.Record, error) {
	if err := l.prime([]string{id}); err != nil {
		return nil, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.cache[id], nil
}

// --- cursors ------------------------------------------------------------------

func encodeCursor(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }

func decodeCursor(c, prefix string) (string, error) {
	b, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil || !strings.HasPrefix(string(b), prefix) {
		return "", errors.New("invalid cursor")
	}
	return strings.TrimPrefix(string(b), prefix), nil
}

func searchCursor(offset int) string { return encodeCursor("s:" + strconv.Itoa(offset)) }

func channelCursor(r *store.Record) string {
	return encodeCursor(fmt.Sprintf("c:%d:%s", r.Height, r.ClaimID))
}

// --- helpers ------------------------------------------------------------------

func intArg(p graphql.ResolveParams, name string, def int) int {
	if v, ok := p.Args[name].(int); ok {
		return v
	}
	return def
}

func strArg(p graphql.ResolveParams, name string) string {
	s, _ := p.Args[name].(string)
	return s
}

func pageSize(p graphql.ResolveParams, def int) int {
	n := intArg(p, "first", def)
	if n < 1 {
		n = 1
	}
	if n > MaxPage {
		n = MaxPage
	}
	return n
}

func (a *API) isBlocked(r *store.Record) bool {
	b := a.blocked()
	return b[r.ClaimID] || (r.ChannelID != "" && b[r.ChannelID])
}

func connection(edges []map[string]any, hasNext bool) map[string]any {
	nodes := make([]any, len(edges))
	end := ""
	for i, e := range edges {
		nodes[i] = e["node"]
		end = e["cursor"].(string)
	}
	var endCursor any
	if end != "" {
		endCursor = end
	}
	return map[string]any{
		"edges": edges,
		"nodes": nodes,
		"pageInfo": map[string]any{
			"hasNextPage": hasNext,
			"endCursor":   endCursor,
		},
	}
}

// --- schema -------------------------------------------------------------------

func (a *API) schema() (graphql.Schema, error) {
	claimType := graphql.NewEnum(graphql.EnumConfig{
		Name: "ClaimType",
		Values: graphql.EnumValueConfigMap{
			"STREAM":     {Value: "stream", Description: "A video, audio, image or other file."},
			"CHANNEL":    {Value: "channel", Description: "A channel (@name)."},
			"REPOST":     {Value: "repost"},
			"COLLECTION": {Value: "collection", Description: "A playlist."},
		},
	})

	rec := func(p graphql.ResolveParams) *store.Record { r, _ := p.Source.(*store.Record); return r }
	str := func(get func(*store.Record) string) graphql.FieldResolveFn {
		return func(p graphql.ResolveParams) (any, error) {
			if s := get(rec(p)); s != "" {
				return s, nil
			}
			return nil, nil
		}
	}
	dewies := func(get func(*store.Record) int64) graphql.FieldResolveFn {
		return func(p graphql.ResolveParams) (any, error) { return strconv.FormatInt(get(rec(p)), 10), nil }
	}

	pageInfo := graphql.NewObject(graphql.ObjectConfig{
		Name: "PageInfo",
		Fields: graphql.Fields{
			"hasNextPage": {Type: graphql.NewNonNull(graphql.Boolean)},
			"endCursor":   {Type: graphql.String, Description: "Pass as `after` for the next page."},
		},
	})

	var claimObj, channelObj *graphql.Object
	var claimConn *graphql.Object

	claimObj = graphql.NewObject(graphql.ObjectConfig{
		Name:        "Claim",
		Description: "A claim on the LBRY blockchain: a stream, channel, repost or collection.",
		Fields: graphql.FieldsThunk(func() graphql.Fields {
			return graphql.Fields{
				"id":          {Type: graphql.NewNonNull(graphql.ID), Description: "Claim ID.", Resolve: func(p graphql.ResolveParams) (any, error) { return rec(p).ClaimID, nil }},
				"name":        {Type: graphql.NewNonNull(graphql.String), Resolve: func(p graphql.ResolveParams) (any, error) { return rec(p).Name, nil }},
				"type":        {Type: graphql.NewNonNull(claimType), Resolve: func(p graphql.ResolveParams) (any, error) { return rec(p).Type, nil }},
				"url":         {Type: graphql.NewNonNull(graphql.String), Description: "Full lbry:// URL.", Resolve: func(p graphql.ResolveParams) (any, error) { return rec(p).URL(), nil }},
				"title":       {Type: graphql.String, Resolve: str(func(r *store.Record) string { return r.Title })},
				"description": {Type: graphql.String, Resolve: str(func(r *store.Record) string { return r.Description })},
				"author":      {Type: graphql.String, Resolve: str(func(r *store.Record) string { return r.Author })},
				"thumbnail":   {Type: graphql.String, Description: "Thumbnail URL.", Resolve: str(func(r *store.Record) string { return r.Thumbnail })},
				"mediaType":   {Type: graphql.String, Description: "MIME type, e.g. video/mp4.", Resolve: str(func(r *store.Record) string { return r.MediaType })},
				"tags":        {Type: graphql.NewNonNull(graphql.NewList(graphql.NewNonNull(graphql.String))), Resolve: func(p graphql.ResolveParams) (any, error) { return nonNil(rec(p).Tags), nil }},
				"languages":   {Type: graphql.NewNonNull(graphql.NewList(graphql.NewNonNull(graphql.String))), Resolve: func(p graphql.ResolveParams) (any, error) { return nonNil(rec(p).Languages), nil }},
				"duration": {Type: graphql.Int, Description: "Seconds, for video and audio.", Resolve: func(p graphql.ResolveParams) (any, error) {
					if d := rec(p).Duration; d > 0 {
						return int(d), nil
					}
					return nil, nil
				}},
				"releaseTime": {Type: graphql.Int, Description: "Unix time the publisher gave.", Resolve: func(p graphql.ResolveParams) (any, error) {
					if t := rec(p).ReleaseTime; t > 0 {
						return int(t), nil
					}
					return nil, nil
				}},
				"height":        {Type: graphql.NewNonNull(graphql.Int), Description: "Block the claim was made in.", Resolve: func(p graphql.ResolveParams) (any, error) { return int(rec(p).Height), nil }},
				"updatedHeight": {Type: graphql.NewNonNull(graphql.Int), Description: "Block of its last update.", Resolve: func(p graphql.ResolveParams) (any, error) { return int(rec(p).UpdatedHeight), nil }},
				"txid":          {Type: graphql.NewNonNull(graphql.String), Resolve: func(p graphql.ResolveParams) (any, error) { return rec(p).TxID, nil }},
				"nout":          {Type: graphql.NewNonNull(graphql.Int), Resolve: func(p graphql.ResolveParams) (any, error) { return rec(p).Nout, nil }},
				"amountDewies":  {Type: graphql.NewNonNull(graphql.String), Description: "LBC on the claim itself, in dewies (1 LBC = 10^8).", Resolve: dewies(func(r *store.Record) int64 { return r.Amount })},
				"stakedDewies":  {Type: graphql.NewNonNull(graphql.String), Description: "Claim amount plus supports, in dewies.", Resolve: dewies(func(r *store.Record) int64 { return r.Staked() })},
				"fee": {Type: graphql.NewObject(graphql.ObjectConfig{Name: "Fee", Fields: graphql.Fields{
					"amount":   {Type: graphql.NewNonNull(graphql.String), Description: "In the currency's smallest unit as stored on chain."},
					"currency": {Type: graphql.NewNonNull(graphql.String)},
				}}), Resolve: func(p graphql.ResolveParams) (any, error) {
					r := rec(p)
					if r.FeeAmount == 0 {
						return nil, nil
					}
					return map[string]any{"amount": strconv.FormatInt(r.FeeAmount, 10), "currency": r.FeeCurrency}, nil
				}},
				"channel": {Type: channelObj, Description: "The channel that signed it.", Resolve: func(p graphql.ResolveParams) (any, error) {
					r := rec(p)
					if r.ChannelID == "" || r.Type == "channel" {
						return nil, nil
					}
					ch, err := loaderFrom(p.Context, a.store).get(r.ChannelID)
					if err != nil {
						return nil, err
					}
					if ch == nil { // signed by a channel the index doesn't have
						return &store.Record{ClaimID: r.ChannelID, Name: r.ChannelName, Type: "channel"}, nil
					}
					return ch, nil
				}},
				"repostOf": {Type: claimObj, Description: "For reposts, the original.", Resolve: func(p graphql.ResolveParams) (any, error) {
					r := rec(p)
					if r.RepostOf == "" {
						return nil, nil
					}
					orig, err := loaderFrom(p.Context, a.store).get(r.RepostOf)
					if err != nil || orig == nil || a.isBlocked(orig) {
						return nil, err
					}
					return orig, nil
				}},
			}
		}),
	})

	claimEdge := graphql.NewObject(graphql.ObjectConfig{
		Name: "ClaimEdge",
		Fields: graphql.Fields{
			"cursor": {Type: graphql.NewNonNull(graphql.String)},
			"node":   {Type: graphql.NewNonNull(claimObj)},
			"score":  {Type: graphql.Float, Description: "Search relevance (search results only)."},
		},
	})
	claimConn = graphql.NewObject(graphql.ObjectConfig{
		Name: "ClaimConnection",
		Fields: graphql.Fields{
			"edges":    {Type: graphql.NewNonNull(graphql.NewList(graphql.NewNonNull(claimEdge)))},
			"nodes":    {Type: graphql.NewNonNull(graphql.NewList(graphql.NewNonNull(claimObj)))},
			"pageInfo": {Type: graphql.NewNonNull(pageInfo)},
		},
	})

	channelObj = graphql.NewObject(graphql.ObjectConfig{
		Name:        "Channel",
		Description: "A channel (@name) and what it has published.",
		Fields: graphql.Fields{
			"id":           {Type: graphql.NewNonNull(graphql.ID), Resolve: func(p graphql.ResolveParams) (any, error) { return rec(p).ClaimID, nil }},
			"name":         {Type: graphql.NewNonNull(graphql.String), Resolve: func(p graphql.ResolveParams) (any, error) { return rec(p).Name, nil }},
			"url":          {Type: graphql.NewNonNull(graphql.String), Resolve: func(p graphql.ResolveParams) (any, error) { return rec(p).URL(), nil }},
			"title":        {Type: graphql.String, Resolve: str(func(r *store.Record) string { return r.Title })},
			"description":  {Type: graphql.String, Resolve: str(func(r *store.Record) string { return r.Description })},
			"thumbnail":    {Type: graphql.String, Resolve: str(func(r *store.Record) string { return r.Thumbnail })},
			"tags":         {Type: graphql.NewNonNull(graphql.NewList(graphql.NewNonNull(graphql.String))), Resolve: func(p graphql.ResolveParams) (any, error) { return nonNil(rec(p).Tags), nil }},
			"stakedDewies": {Type: graphql.NewNonNull(graphql.String), Resolve: dewies(func(r *store.Record) int64 { return r.Staked() })},
			"height": {Type: graphql.Int, Description: "Block the channel was made in (null if the index doesn't have it).", Resolve: func(p graphql.ResolveParams) (any, error) {
				if h := rec(p).Height; h > 0 {
					return int(h), nil
				}
				return nil, nil
			}},
			"claims": {
				Type:        graphql.NewNonNull(claimConn),
				Description: "The channel's claims, newest first.",
				Args: graphql.FieldConfigArgument{
					"first": {Type: graphql.Int, DefaultValue: 20},
					"after": {Type: graphql.String},
					"type":  {Type: claimType},
				},
				Resolve: a.channelClaims,
			},
		},
	})

	searchArgs := graphql.FieldConfigArgument{
		"text":        {Type: graphql.NewNonNull(graphql.String)},
		"first":       {Type: graphql.Int, DefaultValue: 20},
		"after":       {Type: graphql.String},
		"type":        {Type: claimType},
		"mediaType":   {Type: graphql.String, Description: "`video`, `audio`, or a MIME prefix such as `video/mp4`."},
		"minDuration": {Type: graphql.Int, Description: "Seconds."},
		"maxDuration": {Type: graphql.Int, Description: "Seconds."},
	}

	query := graphql.NewObject(graphql.ObjectConfig{
		Name: "Query",
		Fields: graphql.Fields{
			"search": {
				Type:        graphql.NewNonNull(claimConn),
				Description: "Full-text search, ranked by relevance, LBC staked and freshness.",
				Args:        searchArgs,
				Resolve:     func(p graphql.ResolveParams) (any, error) { return a.search(p, false) },
			},
			"autocomplete": {
				Type:        graphql.NewNonNull(graphql.NewList(graphql.NewNonNull(claimObj))),
				Description: "Search as you type: the last word matches as a prefix.",
				Args: graphql.FieldConfigArgument{
					"text":  {Type: graphql.NewNonNull(graphql.String)},
					"first": {Type: graphql.Int, DefaultValue: 10},
					"type":  {Type: claimType},
				},
				Resolve: func(p graphql.ResolveParams) (any, error) {
					conn, err := a.search(p, true)
					if err != nil {
						return nil, err
					}
					return conn.(map[string]any)["nodes"], nil
				},
			},
			"claim": {
				Type: claimObj,
				Args: graphql.FieldConfigArgument{"id": {Type: graphql.NewNonNull(graphql.ID)}},
				Resolve: func(p graphql.ResolveParams) (any, error) {
					r, err := loaderFrom(p.Context, a.store).get(strArg(p, "id"))
					if err != nil || r == nil || a.isBlocked(r) {
						return nil, err
					}
					return r, nil
				},
			},
			"channel": {
				Type: channelObj,
				Args: graphql.FieldConfigArgument{"id": {Type: graphql.NewNonNull(graphql.ID)}},
				Resolve: func(p graphql.ResolveParams) (any, error) {
					r, err := loaderFrom(p.Context, a.store).get(strArg(p, "id"))
					if err != nil || r == nil || r.Type != "channel" || a.isBlocked(r) {
						return nil, err
					}
					return r, nil
				},
			},
			"status": {
				Type: graphql.NewNonNull(graphql.NewObject(graphql.ObjectConfig{
					Name: "Status",
					Fields: graphql.Fields{
						"height": {Type: graphql.NewNonNull(graphql.Int), Description: "Last indexed block (-1 when empty)."},
						"hash":   {Type: graphql.String},
					},
				})),
				Resolve: func(p graphql.ResolveParams) (any, error) {
					h, hash, err := a.store.Tip()
					if err != nil {
						return nil, err
					}
					var hv any
					if hash != "" {
						hv = hash
					}
					return map[string]any{"height": int(h), "hash": hv}, nil
				},
			},
		},
	})
	return graphql.NewSchema(graphql.SchemaConfig{Query: query})
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// search runs a full-text search and returns a ClaimConnection.
func (a *API) search(p graphql.ResolveParams, prefix bool) (any, error) {
	size := pageSize(p, 20)
	offset := 0
	if c := strArg(p, "after"); c != "" {
		s, err := decodeCursor(c, "s:")
		if err != nil {
			return nil, err
		}
		if offset, err = strconv.Atoi(s); err != nil || offset < 0 {
			return nil, errors.New("invalid cursor")
		}
		offset++ // the cursor is the last item's position
	}
	q := store.Query{
		Text:      strArg(p, "text"),
		Size:      size + 1, // one extra to know if there's a next page
		From:      offset,
		ClaimType: strArg(p, "type"),
		Prefix:    prefix,
		Exclude:   a.blocked(),
	}
	switch mt := strings.ToLower(strArg(p, "mediaType")); mt {
	case "":
	case "video", "audio", "image", "text", "application":
		q.MediaPrefix = mt + "/"
	default:
		q.MediaPrefix = mt
	}
	if n := intArg(p, "minDuration", 0); n > 0 {
		q.MinDuration = uint32(n)
	}
	if n := intArg(p, "maxDuration", 0); n > 0 {
		q.MaxDuration = uint32(n)
	}
	hits, err := a.store.Search(q)
	if err != nil {
		return nil, err
	}
	hasNext := len(hits) > size
	if hasNext {
		hits = hits[:size]
	}

	// Load the full claims and their channels in two queries.
	l := loaderFrom(p.Context, a.store)
	ids := make([]string, len(hits))
	for i, h := range hits {
		ids[i] = h.ClaimID
	}
	if err := l.prime(ids); err != nil {
		return nil, err
	}
	var channels []string
	for _, id := range ids {
		if r, _ := l.get(id); r != nil {
			channels = append(channels, r.ChannelID, r.RepostOf)
		}
	}
	if err := l.prime(channels); err != nil {
		return nil, err
	}

	edges := make([]map[string]any, 0, len(hits))
	for i, h := range hits {
		r, _ := l.get(h.ClaimID)
		if r == nil {
			continue // abandoned since the search ran
		}
		edges = append(edges, map[string]any{"cursor": searchCursor(offset + i), "node": r, "score": h.Score})
	}
	return connection(edges, hasNext), nil
}

// channelClaims pages through a channel's claims, newest first, skipping blocked ones.
func (a *API) channelClaims(p graphql.ResolveParams) (any, error) {
	ch, _ := p.Source.(*store.Record)
	if ch == nil {
		return connection(nil, false), nil
	}
	size := pageSize(p, 20)
	var after store.ChannelPage
	if c := strArg(p, "after"); c != "" {
		s, err := decodeCursor(c, "c:")
		if err != nil {
			return nil, err
		}
		hs, id, ok := strings.Cut(s, ":")
		h, err := strconv.ParseInt(hs, 10, 64)
		if !ok || err != nil || id == "" {
			return nil, errors.New("invalid cursor")
		}
		after = store.ChannelPage{Height: h, ClaimID: id}
	}
	blocked := a.blocked()
	l := loaderFrom(p.Context, a.store)
	var edges []map[string]any
	hasNext := false
	for len(edges) <= size {
		batch, err := a.store.ChannelClaims(ch.ClaimID, strArg(p, "type"), after, size+1)
		if err != nil {
			return nil, err
		}
		for _, r := range batch {
			after = store.ChannelPage{Height: r.Height, ClaimID: r.ClaimID}
			if blocked[r.ClaimID] {
				continue
			}
			if len(edges) == size {
				hasNext = true
				break
			}
			l.mu.Lock()
			l.cache[r.ClaimID] = r
			l.mu.Unlock()
			edges = append(edges, map[string]any{"cursor": channelCursor(r), "node": r})
		}
		if hasNext || len(batch) <= size {
			break
		}
	}
	// Channels are already known; reposts point elsewhere.
	l.mu.Lock()
	l.cache[ch.ClaimID] = ch
	l.mu.Unlock()
	var reposts []string
	for _, e := range edges {
		if r := e["node"].(*store.Record); r.RepostOf != "" {
			reposts = append(reposts, r.RepostOf)
		}
	}
	if err := l.prime(reposts); err != nil {
		return nil, err
	}
	return connection(edges, hasNext), nil
}
