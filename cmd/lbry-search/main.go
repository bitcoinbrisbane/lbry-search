// lbry-search serves full-text search over the index lbry-indexer builds, with
// Lighthouse's API so existing LBRY apps only change a URL.
//
//	GET /search?s=sailing&size=20&from=0[&claimType=stream|channel][&mediaType=video][&maxDuration=180][&minDuration=181]
//	GET /autocomplete?s=sail
//	GET /status
package main

import (
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/bitcoinbrisbane/lbry-search/internal/store"
)

type server struct {
	store   *store.Store
	blocked func() map[string]bool
}

func main() {
	dbPath := flag.String("db", "lbry-search.db", "SQLite index file (written by lbry-indexer)")
	addr := flag.String("addr", ":8080", "listen address")
	blocklist := flag.String("blocklist", "", "moderation list URL (JSON with claims and channels arrays), refreshed every 10 minutes")
	flag.Parse()

	s, err := store.OpenReadOnly(*dbPath)
	if err != nil {
		log.Fatal(err)
	}
	srv := &server{store: s, blocked: watchBlocklist(*blocklist)}
	mux := http.NewServeMux()
	mux.HandleFunc("/search", srv.search(false))
	mux.HandleFunc("/autocomplete", srv.search(true))
	mux.HandleFunc("/status", srv.status)
	log.Printf("lbry-search on %s", *addr)
	log.Fatal(http.ListenAndServe(*addr, cors(mux)))
}

func (srv *server) search(autocomplete bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		v := r.URL.Query()
		q := store.Query{
			Text:      v.Get("s"),
			Size:      atoi(v.Get("size"), 20),
			From:      atoi(v.Get("from"), 0),
			ClaimType: v.Get("claimType"),
			Prefix:    autocomplete,
			Exclude:   srv.blocked(),
		}
		if autocomplete && q.Size > 10 {
			q.Size = 10
		}
		switch v.Get("mediaType") {
		case "video":
			q.MediaPrefix = "video/"
		case "audio":
			q.MediaPrefix = "audio/"
		}
		q.MaxDuration = uint32(atoi(v.Get("maxDuration"), 0))
		q.MinDuration = uint32(atoi(v.Get("minDuration"), 0))
		hits, err := srv.store.Search(q)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if hits == nil {
			hits = []store.Hit{}
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "public, max-age=60")
		json.NewEncoder(w).Encode(hits)
	}
}

func (srv *server) status(w http.ResponseWriter, _ *http.Request) {
	h, hash, err := srv.store.Tip()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"height": h, "hash": hash})
}

func atoi(s string, def int) int {
	if n, err := strconv.Atoi(s); err == nil && n >= 0 {
		return n
	}
	return def
}

func cors(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		h.ServeHTTP(w, r)
	})
}

// watchBlocklist fetches a moderation list (Carrel's format) and refreshes it.
func watchBlocklist(url string) func() map[string]bool {
	var mu sync.RWMutex
	current := map[string]bool{}
	if url == "" {
		return func() map[string]bool { return current }
	}
	load := func() {
		resp, err := http.Get(url)
		if err != nil {
			log.Printf("blocklist: %v", err)
			return
		}
		defer resp.Body.Close()
		var list struct {
			Claims   []string `json:"claims"`
			Channels []string `json:"channels"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
			log.Printf("blocklist: %v", err)
			return
		}
		next := map[string]bool{}
		for _, id := range append(list.Claims, list.Channels...) {
			next[id] = true
		}
		mu.Lock()
		current = next
		mu.Unlock()
	}
	load()
	go func() {
		for range time.Tick(10 * time.Minute) {
			load()
		}
	}()
	return func() map[string]bool {
		mu.RLock()
		defer mu.RUnlock()
		return current
	}
}
