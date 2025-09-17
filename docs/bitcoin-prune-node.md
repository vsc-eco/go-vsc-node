# Running a local bitcoin prune node

Run it with `docker compose up btcd`

```conf
# ./btc-data/bitcoin.conf
rpcuser=rpcuser
rpcpassword=rpcpassword
rpcallowip=0.0.0.0/0
rpcbind=0.0.0.0
prune=550  # Keep only 550MB of blocks
txindex=0  # Disable transaction indexing
```
