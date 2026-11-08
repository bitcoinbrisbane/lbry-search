// Package lbcd talks JSON-RPC to an LBRY full node (lbcd).
package lbcd

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

type Client struct {
	URL      string // http://127.0.0.1:9245
	User     string
	Password string
	HTTP     *http.Client
}

func New(url, user, password string) *Client {
	return &Client{URL: url, User: user, Password: password, HTTP: &http.Client{Timeout: 60 * time.Second}}
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("lbcd %d: %s", e.Code, e.Message) }

func (c *Client) call(ctx context.Context, method string, params []any, out any) error {
	body, _ := json.Marshal(map[string]any{"jsonrpc": "1.0", "id": 1, "method": method, "params": params})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.User != "" {
		req.SetBasicAuth(c.User, c.Password)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var r struct {
		Result json.RawMessage `json:"result"`
		Error  *rpcError       `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return fmt.Errorf("%s: HTTP %d: %w", method, resp.StatusCode, err)
	}
	if r.Error != nil {
		return r.Error
	}
	return json.Unmarshal(r.Result, out)
}

func (c *Client) BlockCount(ctx context.Context) (int64, error) {
	var n int64
	return n, c.call(ctx, "getblockcount", nil, &n)
}

func (c *Client) BlockHash(ctx context.Context, height int64) (string, error) {
	var h string
	return h, c.call(ctx, "getblockhash", []any{height}, &h)
}

// SetPruneHold tells a pruning lbcd not to delete blocks above height until
// we've read them (bitcoinbrisbane/lbcd's setprunehold). On a node without
// pruning, or without the RPC, this returns an error the caller can ignore.
func (c *Client) SetPruneHold(ctx context.Context, height int64) error {
	var out any
	return c.call(ctx, "setprunehold", []any{height}, &out)
}

// PruneHeight is the lowest height the node still has the block for (0 if not pruned).
func (c *Client) PruneHeight(ctx context.Context) (int64, error) {
	var info struct {
		Pruned      bool  `json:"pruned"`
		PruneHeight int64 `json:"pruneheight"`
	}
	if err := c.call(ctx, "getblockchaininfo", nil, &info); err != nil {
		return 0, err
	}
	if !info.Pruned {
		return 0, nil
	}
	return info.PruneHeight, nil
}

// RawBlock is getblock <hash> 0: the serialized block.
func (c *Client) RawBlock(ctx context.Context, hash string) ([]byte, error) {
	var h string
	if err := c.call(ctx, "getblock", []any{hash, 0}, &h); err != nil {
		return nil, err
	}
	return hex.DecodeString(h)
}
