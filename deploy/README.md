# Running the stack on one server

```
lbcd ──► lbry-indexer ──► lbry-search.db ◄── lbry-search :8080 ◄── lbry-daemon (lbryd) :5279 ◄── apps
```

Tested layout for Ubuntu 24.04 on a single droplet. lbcd holds the whole chain, so size the disk
for it plus the index; check the current chain size before choosing (the index is a few GB).

1. **User and folders**
   ```sh
   sudo useradd -m -s /bin/bash lbry
   sudo mkdir -p /var/lib/lbry-search && sudo chown lbry /var/lib/lbry-search
   ```
2. **lbcd** — install the latest release binary from https://github.com/lbryio/lbcd/releases to
   `/usr/local/bin/lbcd`, copy `lbcd.conf` to `/home/lbry/.lbcd/lbcd.conf` (set `rpcpass`), and
   enable `lbcd.service`. First sync takes a while; the indexer can start alongside it and follows.
3. **lbry-search** — `make` (needs Go and a C compiler), copy `bin/lbry-indexer` and
   `bin/lbry-search` to `/usr/local/bin`, put the RPC login in `/etc/lbry-search.env`:
   ```sh
   LBCD_RPCUSER=lbry
   LBCD_RPCPASS=…
   ```
   then enable `lbry-indexer.service` and `lbry-search.service`.
4. **lbry-daemon** (optional, for streaming and the SDK API) — build `lbryd` from
   https://github.com/LBRYFoundation/lbry-daemon with `LBRY_SEARCH_URL` support, install to
   `/usr/local/bin/lbryd`, enable `lbryd.service`.

```sh
sudo cp deploy/*.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now lbcd lbry-indexer lbry-search
journalctl -u lbry-indexer -f          # "indexed 123456 / 2300000"
curl '127.0.0.1:8080/search?s=linux&size=5'
```

Put a reverse proxy (Caddy, nginx) with TLS in front of `:8080` to expose search publicly.
