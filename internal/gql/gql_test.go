package gql

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bitcoinbrisbane/lbry-search/internal/store"
)

const (
	chanID  = "1111111111111111111111111111111111111111"
	otherCh = "2222222222222222222222222222222222222222"
)

func vid(n int) string { return fmt.Sprintf("%040x", n+0x100) }

// seed: a channel with 30 sailing videos over 30 blocks, and a blocked channel's video.
func seed(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "idx.db"))
	if err != nil {
		t.Fatal(err)
	}
	put := func(h int64, c store.Claim) {
		tx, err := s.Begin(h, fmt.Sprintf("%064x", h), false)
		if err != nil {
			t.Fatal(err)
		}
		if err := tx.PutClaim(c, false); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	put(1, store.Claim{ClaimID: chanID, Name: "@sailor", TxID: fmt.Sprintf("%064x", 1), Amount: 1e8, Type: "channel", Title: "The Sailor"})
	put(2, store.Claim{ClaimID: otherCh, Name: "@spam", TxID: fmt.Sprintf("%064x", 2), Amount: 1e8, Type: "channel", Title: "Spam"})
	for i := 0; i < 30; i++ {
		put(int64(10+i), store.Claim{
			ClaimID: vid(i), Name: fmt.Sprintf("sailing-%d", i), TxID: fmt.Sprintf("%064x", 100+i),
			Amount: int64(1e8 * (i + 1)), Type: "stream", ChannelID: chanID,
			Title: fmt.Sprintf("Sailing episode %d", i), Tags: []string{"sailing", "boats"},
			MediaType: "video/mp4", Duration: uint32(60 * (i + 1)), ReleaseTime: 1700000000 + int64(i),
		})
	}
	put(50, store.Claim{ClaimID: vid(99), Name: "sailing-spam", TxID: fmt.Sprintf("%064x", 999),
		Amount: 1e12, Type: "stream", ChannelID: otherCh, Title: "Sailing spam", MediaType: "video/mp4"})
	return s
}

type result struct {
	Data   map[string]json.RawMessage `json:"data"`
	Errors []struct{ Message string } `json:"errors"`
}

func run(t *testing.T, h http.Handler, query string, vars map[string]any) result {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"query": query, "variables": vars})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/graphql", bytes.NewReader(body)))
	var res result
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("%v: %s", err, rec.Body.String())
	}
	return res
}

func ok(t *testing.T, res result) {
	t.Helper()
	if len(res.Errors) > 0 {
		t.Fatalf("errors: %+v", res.Errors)
	}
}

type conn struct {
	Nodes []struct {
		ID, Name, URL, StakedDewies string
		Title                       *string
		Duration                    *int
		Tags                        []string
		Channel                     *struct {
			ID, Name string
			Title    *string
		}
	}
	PageInfo struct {
		HasNextPage bool
		EndCursor   *string
	}
}

func handler(t *testing.T) http.Handler {
	s := seed(t)
	api, err := New(s, func() map[string]bool { return map[string]bool{otherCh: true, vid(29): true} })
	if err != nil {
		t.Fatal(err)
	}
	return api.Handler(true)
}

func TestSearchPagesAndFilters(t *testing.T) {
	h := handler(t)
	q := `query($after: String, $first: Int) {
		search(text: "sailing", first: $first, after: $after, type: STREAM, mediaType: "video") {
			nodes { id name url stakedDewies title duration tags channel { id name title } }
			pageInfo { hasNextPage endCursor }
		}
		status { height }
	}`
	seen := map[string]bool{}
	var after any
	pages := 0
	for {
		res := run(t, h, q, map[string]any{"after": after, "first": 10})
		ok(t, res)
		var c conn
		json.Unmarshal(res.Data["search"], &c)
		for _, n := range c.Nodes {
			if seen[n.ID] {
				t.Fatalf("%s twice", n.ID)
			}
			seen[n.ID] = true
			if n.ID == vid(99) || n.ID == vid(29) {
				t.Fatalf("blocked claim %s returned", n.ID)
			}
			if n.Channel == nil || n.Channel.Name != "@sailor" || n.Channel.Title == nil || *n.Channel.Title != "The Sailor" {
				t.Fatalf("channel: %+v", n.Channel)
			}
			if !strings.HasPrefix(n.URL, "lbry://@sailor#"+chanID+"/sailing-") || len(n.Tags) != 2 {
				t.Fatalf("node: %+v", n)
			}
		}
		pages++
		if !c.PageInfo.HasNextPage {
			break
		}
		after = *c.PageInfo.EndCursor
	}
	if len(seen) != 29 || pages != 3 {
		t.Fatalf("got %d claims over %d pages, want 29 over 3", len(seen), pages)
	}

	res := run(t, h, `{ search(text: "sailing", maxDuration: 180) { nodes { duration } } }`, nil)
	ok(t, res)
	var c conn
	json.Unmarshal(res.Data["search"], &c)
	if len(c.Nodes) != 3 {
		t.Fatalf("maxDuration 180: %d results, want 3", len(c.Nodes))
	}
}

func TestChannelClaimsAndLookups(t *testing.T) {
	h := handler(t)
	q := `query($id: ID!, $after: String) {
		channel(id: $id) { name title claims(first: 12, after: $after) {
			nodes { id name } pageInfo { hasNextPage endCursor } } }
	}`
	var ids []string
	var after any
	for {
		res := run(t, h, q, map[string]any{"id": chanID, "after": after})
		ok(t, res)
		var ch struct {
			Name   string
			Claims conn
		}
		json.Unmarshal(res.Data["channel"], &ch)
		for _, n := range ch.Claims.Nodes {
			ids = append(ids, n.ID)
		}
		if !ch.Claims.PageInfo.HasNextPage {
			break
		}
		after = *ch.Claims.PageInfo.EndCursor
	}
	// Newest first, without the blocked episode 29.
	if len(ids) != 29 || ids[0] != vid(28) || ids[28] != vid(0) {
		t.Fatalf("channel claims: %d, first %s last %s", len(ids), ids[0], ids[len(ids)-1])
	}

	res := run(t, h, `{ a: claim(id: "`+vid(3)+`") { title type duration } b: claim(id: "`+vid(29)+`") { title }
		c: channel(id: "`+otherCh+`") { name } d: channel(id: "`+vid(3)+`") { name } }`, nil)
	ok(t, res)
	var a struct {
		Title, Type string
		Duration    int
	}
	json.Unmarshal(res.Data["a"], &a)
	if a.Title != "Sailing episode 3" || a.Type != "STREAM" || a.Duration != 240 {
		t.Fatalf("claim: %s", res.Data["a"])
	}
	for _, k := range []string{"b", "c", "d"} {
		if string(res.Data[k]) != "null" {
			t.Fatalf("%s should be null: %s", k, res.Data[k])
		}
	}

	res = run(t, h, `{ autocomplete(text: "sailor", first: 3) { name } }`, nil)
	ok(t, res)
	var ac []struct{ Name string }
	json.Unmarshal(res.Data["autocomplete"], &ac)
	if len(ac) != 3 || ac[0].Name != "@sailor" {
		t.Fatalf("autocomplete: %+v", ac)
	}
}

func TestLimits(t *testing.T) {
	h := handler(t)
	res := run(t, h, `{ search(text: "a", first: 100) { nodes { channel { claims(first: 100) { nodes { id name title } } } } } }`, nil)
	if len(res.Errors) == 0 || !strings.Contains(res.Errors[0].Message, "too much") {
		t.Fatalf("expected a cost error, got %+v", res.Errors)
	}
	deep := `{ claim(id: "x") ` + strings.Repeat("{ repostOf ", 11) + "{ id }" + strings.Repeat(" }", 12)
	res = run(t, h, deep, nil)
	if len(res.Errors) == 0 || !strings.Contains(res.Errors[0].Message, "deep") {
		t.Fatalf("expected a depth error, got %+v", res.Errors)
	}
	// GraphiQL's introspection is allowed.
	res = run(t, h, `{ __schema { types { name fields { name type { name ofType { name ofType { name ofType { name ofType { name ofType { name ofType { name } } } } } } } } } } }`, nil)
	ok(t, res)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/graphql", nil)
	req.Header.Set("Accept", "text/html")
	h.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), "GraphiQL") {
		t.Fatal("no GraphiQL page")
	}
}

func TestSearchInChannel(t *testing.T) {
	api, err := New(seed(t), func() map[string]bool { return nil })
	if err != nil {
		t.Fatal(err)
	}
	h := api.Handler(false)
	q := `query($channel: ID, $first: Int) {
		search(text: "sailing", channel: $channel, first: $first) { nodes { id channel { id } } pageInfo { hasNextPage } }
	}`
	count := func(channel any) (int, bool) {
		t.Helper()
		res := run(t, h, q, map[string]any{"channel": channel, "first": 100})
		ok(t, res)
		var c conn
		json.Unmarshal(res.Data["search"], &c)
		for _, n := range c.Nodes {
			if channel != nil && (n.Channel == nil || n.Channel.ID != channel) {
				t.Fatalf("channel %v: got %s from %+v", channel, n.ID, n.Channel)
			}
		}
		return len(c.Nodes), c.PageInfo.HasNextPage
	}
	// The spam channel's one video is staked far above the rest, but only it is in its channel.
	if n, more := count(otherCh); n != 1 || more {
		t.Fatalf("spam channel: %d results (more=%v), want 1", n, more)
	}
	if n, _ := count(chanID); n != 30 {
		t.Fatalf("sailor channel: %d results, want 30", n)
	}
	if n, _ := count("3333333333333333333333333333333333333333"); n != 0 {
		t.Fatalf("unknown channel: %d results, want 0", n)
	}
	// Without the argument: both channels' videos.
	if n, _ := count(nil); n != 31 {
		t.Fatalf("no channel: %d results, want 31", n)
	}
}

func TestFullPage(t *testing.T) {
	h := handler(t)
	// 29 unblocked sailing videos: a page of 100 holds them all, and says it's the last.
	res := run(t, h, `{ search(text: "sailing", type: STREAM, first: 100) { nodes { id } pageInfo { hasNextPage } } }`, nil)
	ok(t, res)
	var c conn
	json.Unmarshal(res.Data["search"], &c)
	if len(c.Nodes) != 29 || c.PageInfo.HasNextPage {
		t.Fatalf("first: 100 gave %d results (more=%v), want 29 and no next page", len(c.Nodes), c.PageInfo.HasNextPage)
	}
}
