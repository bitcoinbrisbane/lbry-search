// Package store keeps the index in one SQLite file: claims with their metadata and
// LBC staked, an FTS5 full-text index over streams and channels, the claim and support
// outputs still unspent (to notice abandons and spent supports), and an undo log for
// the last blocks so a chain reorganisation can be rolled back.
package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	_ "github.com/mattn/go-sqlite3" // build with -tags sqlite_fts5
)

// UndoDepth is how many recent blocks can be rolled back.
const UndoDepth = 200

type Store struct {
	db *sql.DB
}

const schema = `
PRAGMA journal_mode = WAL;
PRAGMA synchronous = NORMAL;
CREATE TABLE IF NOT EXISTS blocks (
	height INTEGER PRIMARY KEY,
	hash   TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS claims (
	claim_id       TEXT PRIMARY KEY,
	name           TEXT NOT NULL,
	txid           TEXT NOT NULL,
	nout           INTEGER NOT NULL,
	amount         INTEGER NOT NULL,           -- dewies on the claim itself
	support_amount INTEGER NOT NULL DEFAULT 0, -- dewies in unspent supports
	height         INTEGER NOT NULL,           -- created
	updated_height INTEGER NOT NULL,
	type           TEXT NOT NULL,              -- stream, channel, repost, collection or ''
	version        INTEGER NOT NULL,
	channel_id     TEXT NOT NULL DEFAULT '',
	channel_name   TEXT NOT NULL DEFAULT '',
	title          TEXT NOT NULL DEFAULT '',
	description    TEXT NOT NULL DEFAULT '',
	author         TEXT NOT NULL DEFAULT '',
	tags           TEXT NOT NULL DEFAULT '',   -- space separated
	languages      TEXT NOT NULL DEFAULT '',
	thumbnail      TEXT NOT NULL DEFAULT '',
	media_type     TEXT NOT NULL DEFAULT '',
	duration       INTEGER NOT NULL DEFAULT 0,
	release_time   INTEGER NOT NULL DEFAULT 0,
	repost_of      TEXT NOT NULL DEFAULT '',
	fee_amount     INTEGER NOT NULL DEFAULT 0,
	fee_currency   TEXT NOT NULL DEFAULT '',
	abandoned      INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS claims_channel ON claims(channel_id);
CREATE INDEX IF NOT EXISTS claims_channel_height ON claims(channel_id, height);
CREATE INDEX IF NOT EXISTS claims_name ON claims(name);
CREATE TABLE IF NOT EXISTS outputs (
	outpoint TEXT PRIMARY KEY,                 -- txid:nout
	claim_id TEXT NOT NULL,
	kind     TEXT NOT NULL,                    -- claim or support
	amount   INTEGER NOT NULL
);
CREATE VIRTUAL TABLE IF NOT EXISTS claims_fts USING fts5(
	title, name, channel_name, author, tags, description,
	tokenize = 'unicode61 remove_diacritics 2'
);
CREATE TABLE IF NOT EXISTS undo (
	id     INTEGER PRIMARY KEY AUTOINCREMENT,
	height INTEGER NOT NULL,
	tbl    TEXT NOT NULL,
	key    TEXT NOT NULL,
	row    TEXT                                -- JSON of the row before, NULL if it didn't exist
);
CREATE INDEX IF NOT EXISTS undo_height ON undo(height);
`

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite3", path+"?_busy_timeout=5000")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // one writer; readers in the search server open their own handle
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("schema (built with -tags sqlite_fts5?): %w", err)
	}
	return &Store{db: db}, nil
}

// OpenReadOnly is for the search server, alongside a running indexer.
func OpenReadOnly(path string) (*Store, error) {
	db, err := sql.Open("sqlite3", "file:"+path+"?mode=ro&_busy_timeout=5000")
	if err != nil {
		return nil, err
	}
	return &Store{db: db}, db.Ping()
}

func (s *Store) Close() error { return s.db.Close() }

// Tip is the last indexed block, or height -1 when empty.
func (s *Store) Tip() (int64, string, error) {
	var h int64
	var hash string
	err := s.db.QueryRow(`SELECT height, hash FROM blocks ORDER BY height DESC LIMIT 1`).Scan(&h, &hash)
	if err == sql.ErrNoRows {
		return -1, "", nil
	}
	return h, hash, err
}

func (s *Store) BlockHash(height int64) (string, error) {
	var hash string
	err := s.db.QueryRow(`SELECT hash FROM blocks WHERE height = ?`, height).Scan(&hash)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return hash, err
}

// --- writing a block -------------------------------------------------------

// Tx is one block's changes, applied in a database transaction with undo records.
type Tx struct {
	tx       *sql.Tx
	height   int64
	ftsDirty map[string]bool
	undo     bool
}

// Begin starts a block. keepUndo records how to roll it back; it's skipped while
// catching up far below the tip, where a reorg can't reach.
func (s *Store) Begin(height int64, hash string, keepUndo bool) (*Tx, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	t := &Tx{tx: tx, height: height, ftsDirty: map[string]bool{}, undo: keepUndo}
	if err := t.snapshot("blocks", fmt.Sprint(height)); err != nil {
		tx.Rollback()
		return nil, err
	}
	if _, err := tx.Exec(`INSERT OR REPLACE INTO blocks(height, hash) VALUES (?, ?)`, height, hash); err != nil {
		tx.Rollback()
		return nil, err
	}
	return t, nil
}

func (t *Tx) Rollback() error { return t.tx.Rollback() }

// Commit refreshes the full-text rows of changed claims and prunes old undo records.
func (t *Tx) Commit() error {
	for id := range t.ftsDirty {
		if err := refreshFTS(t.tx, id); err != nil {
			t.tx.Rollback()
			return err
		}
	}
	if _, err := t.tx.Exec(`DELETE FROM undo WHERE height <= ?`, t.height-UndoDepth); err != nil {
		t.tx.Rollback()
		return err
	}
	return t.tx.Commit()
}

var keyColumn = map[string]string{"blocks": "height", "claims": "claim_id", "outputs": "outpoint"}

// snapshot records a row's current state (with its rowid) before it changes.
func (t *Tx) snapshot(table, key string) error {
	if !t.undo {
		return nil
	}
	rows, err := t.tx.Query(fmt.Sprintf(`SELECT rowid AS _rowid, * FROM %s WHERE %s = ?`, table, keyColumn[table]), key)
	if err != nil {
		return err
	}
	defer rows.Close()
	var row any
	if rows.Next() {
		cols, _ := rows.Columns()
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return err
		}
		m := map[string]any{}
		for i, c := range cols {
			if b, ok := vals[i].([]byte); ok {
				vals[i] = string(b)
			}
			m[c] = vals[i]
		}
		row = m
	}
	rows.Close()
	var js any
	if row != nil {
		b, _ := json.Marshal(row)
		js = string(b)
	}
	_, err = t.tx.Exec(`INSERT INTO undo(height, tbl, key, row) VALUES (?, ?, ?, ?)`, t.height, table, key, js)
	return err
}

// Claim is a new claim or an update, as found in an output.
type Claim struct {
	ClaimID     string
	Name        string
	TxID        string
	Nout        uint32
	Amount      int64
	Type        string
	Version     int
	ChannelID   string
	Title       string
	Description string
	Author      string
	Tags        []string
	Languages   []string
	Thumbnail   string
	MediaType   string
	Duration    uint32
	ReleaseTime int64
	RepostOf    string
	FeeAmount   uint64
	FeeCurrency string
}

// PutClaim inserts a new claim or replaces an update's metadata, and records its output.
func (t *Tx) PutClaim(c Claim, isUpdate bool) error {
	if err := t.snapshot("claims", c.ClaimID); err != nil {
		return err
	}
	channelName := ""
	if c.Type == "channel" {
		channelName = c.Name
	} else if c.ChannelID != "" {
		_ = t.tx.QueryRow(`SELECT name FROM claims WHERE claim_id = ? AND type = 'channel'`, c.ChannelID).Scan(&channelName)
	}
	tags, langs := strings.Join(c.Tags, " "), strings.Join(c.Languages, " ")
	var exists bool
	_ = t.tx.QueryRow(`SELECT 1 FROM claims WHERE claim_id = ?`, c.ClaimID).Scan(&exists)
	var err error
	if exists {
		_, err = t.tx.Exec(`UPDATE claims SET name=?, txid=?, nout=?, amount=?, updated_height=?, type=?, version=?,
			channel_id=?, channel_name=?, title=?, description=?, author=?, tags=?, languages=?, thumbnail=?,
			media_type=?, duration=?, release_time=?, repost_of=?, fee_amount=?, fee_currency=?, abandoned=0
			WHERE claim_id=?`,
			c.Name, c.TxID, c.Nout, c.Amount, t.height, c.Type, c.Version, c.ChannelID, channelName, c.Title,
			c.Description, c.Author, tags, langs, c.Thumbnail, c.MediaType, c.Duration, c.ReleaseTime, c.RepostOf,
			c.FeeAmount, c.FeeCurrency, c.ClaimID)
	} else if !isUpdate {
		_, err = t.tx.Exec(`INSERT INTO claims(claim_id, name, txid, nout, amount, height, updated_height, type, version,
			channel_id, channel_name, title, description, author, tags, languages, thumbnail, media_type, duration,
			release_time, repost_of, fee_amount, fee_currency) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			c.ClaimID, c.Name, c.TxID, c.Nout, c.Amount, t.height, t.height, c.Type, c.Version, c.ChannelID, channelName,
			c.Title, c.Description, c.Author, tags, langs, c.Thumbnail, c.MediaType, c.Duration, c.ReleaseTime,
			c.RepostOf, c.FeeAmount, c.FeeCurrency)
	} else {
		return nil // an update of a claim we never saw (shouldn't happen from genesis)
	}
	if err != nil {
		return err
	}
	if c.Type == "channel" {
		// Streams signed by this channel pick up its (new) name.
		if _, err := t.tx.Exec(`UPDATE claims SET channel_name = ? WHERE channel_id = ? AND channel_name != ?`,
			c.Name, c.ClaimID, c.Name); err != nil {
			return err
		}
	}
	t.ftsDirty[c.ClaimID] = true
	return t.putOutput(fmt.Sprintf("%s:%d", c.TxID, c.Nout), c.ClaimID, "claim", c.Amount)
}

// PutSupport records a support output and adds it to the claim's stake.
func (t *Tx) PutSupport(outpoint, claimID string, amount int64) error {
	if err := t.putOutput(outpoint, claimID, "support", amount); err != nil {
		return err
	}
	return t.addSupport(claimID, amount)
}

func (t *Tx) addSupport(claimID string, delta int64) error {
	if err := t.snapshot("claims", claimID); err != nil {
		return err
	}
	_, err := t.tx.Exec(`UPDATE claims SET support_amount = support_amount + ? WHERE claim_id = ?`, delta, claimID)
	return err
}

func (t *Tx) putOutput(outpoint, claimID, kind string, amount int64) error {
	if err := t.snapshot("outputs", outpoint); err != nil {
		return err
	}
	_, err := t.tx.Exec(`INSERT OR REPLACE INTO outputs(outpoint, claim_id, kind, amount) VALUES (?, ?, ?, ?)`,
		outpoint, claimID, kind, amount)
	return err
}

// Spent is an input spending a claim or support output we track.
type Spent struct {
	ClaimID string
	Kind    string
	Amount  int64
}

// Spend removes a spent output; it returns what it was, or nil if it isn't ours.
func (t *Tx) Spend(outpoint string) (*Spent, error) {
	var sp Spent
	err := t.tx.QueryRow(`SELECT claim_id, kind, amount FROM outputs WHERE outpoint = ?`, outpoint).
		Scan(&sp.ClaimID, &sp.Kind, &sp.Amount)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := t.snapshot("outputs", outpoint); err != nil {
		return nil, err
	}
	if _, err := t.tx.Exec(`DELETE FROM outputs WHERE outpoint = ?`, outpoint); err != nil {
		return nil, err
	}
	if sp.Kind == "support" {
		return &sp, t.addSupport(sp.ClaimID, -sp.Amount)
	}
	return &sp, nil
}

// Abandon marks a claim whose output was spent without an update in the same transaction.
func (t *Tx) Abandon(claimID string) error {
	if err := t.snapshot("claims", claimID); err != nil {
		return err
	}
	if _, err := t.tx.Exec(`UPDATE claims SET abandoned = 1 WHERE claim_id = ?`, claimID); err != nil {
		return err
	}
	t.ftsDirty[claimID] = true
	return nil
}

// refreshFTS keeps one full-text row per live stream or channel, keyed by the claim's rowid.
func refreshFTS(tx *sql.Tx, claimID string) error {
	var rowid int64
	var typ, title, name, channel, author, tags, desc string
	var abandoned bool
	err := tx.QueryRow(`SELECT rowid, type, title, name, channel_name, author, tags, description, abandoned
		FROM claims WHERE claim_id = ?`, claimID).Scan(&rowid, &typ, &title, &name, &channel, &author, &tags, &desc, &abandoned)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM claims_fts WHERE rowid = ?`, rowid); err != nil {
		return err
	}
	if abandoned || (typ != "stream" && typ != "channel") {
		return nil
	}
	if len(desc) > 2000 {
		desc = desc[:2000]
	}
	_, err = tx.Exec(`INSERT INTO claims_fts(rowid, title, name, channel_name, author, tags, description)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, rowid, title, strings.TrimPrefix(name, "@"), strings.TrimPrefix(channel, "@"),
		author, tags, desc)
	return err
}

// RollbackTo undoes every block above height, newest first.
func (s *Store) RollbackTo(height int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	rows, err := tx.Query(`SELECT id, tbl, key, row FROM undo WHERE height > ? ORDER BY id DESC`, height)
	if err != nil {
		tx.Rollback()
		return err
	}
	type entry struct {
		tbl, key string
		row      sql.NullString
	}
	var entries []entry
	for rows.Next() {
		var id int64
		var e entry
		if err := rows.Scan(&id, &e.tbl, &e.key, &e.row); err != nil {
			rows.Close()
			tx.Rollback()
			return err
		}
		entries = append(entries, e)
	}
	rows.Close()
	dirty := map[string]bool{}
	for _, e := range entries {
		if e.tbl == "claims" {
			dirty[e.key] = true
		}
		if err := restore(tx, e.tbl, e.key, e.row); err != nil {
			tx.Rollback()
			return err
		}
	}
	// Claims deleted by the rollback lose their FTS row; the rowid was in the snapshot.
	for id := range dirty {
		if err := refreshFTS(tx, id); err != nil {
			tx.Rollback()
			return err
		}
	}
	if _, err := tx.Exec(`DELETE FROM undo WHERE height > ?`, height); err != nil {
		tx.Rollback()
		return err
	}
	if _, err := tx.Exec(`DELETE FROM blocks WHERE height > ?`, height); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

func restore(tx *sql.Tx, table, key string, row sql.NullString) error {
	if table == "claims" {
		// Drop the FTS row tied to the current rowid before the claim row changes.
		var rowid int64
		if tx.QueryRow(`SELECT rowid FROM claims WHERE claim_id = ?`, key).Scan(&rowid) == nil {
			if _, err := tx.Exec(`DELETE FROM claims_fts WHERE rowid = ?`, rowid); err != nil {
				return err
			}
		}
	}
	if _, err := tx.Exec(fmt.Sprintf(`DELETE FROM %s WHERE %s = ?`, table, keyColumn[table]), key); err != nil {
		return err
	}
	if !row.Valid {
		return nil
	}
	var m map[string]any
	dec := json.NewDecoder(strings.NewReader(row.String))
	dec.UseNumber() // dewies don't fit a float64 exactly
	if err := dec.Decode(&m); err != nil {
		return err
	}
	for k, v := range m {
		if n, ok := v.(json.Number); ok {
			if i, err := n.Int64(); err == nil {
				m[k] = i
			}
		}
	}
	cols, marks, args := []string{"rowid"}, []string{"?"}, []any{m["_rowid"]}
	for c, v := range m {
		if c == "_rowid" {
			continue
		}
		cols = append(cols, c)
		marks = append(marks, "?")
		args = append(args, v)
	}
	_, err := tx.Exec(fmt.Sprintf(`INSERT INTO %s(%s) VALUES (%s)`, table, strings.Join(cols, ","), strings.Join(marks, ",")), args...)
	return err
}
