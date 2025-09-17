# Running a local bitcoin prune node

Before running the Bitcoin Prune node, create the file `./.btc-data/bitcoin.conf`
with the following

```conf
# ./.btc-data/bitcoin.conf
rpcuser=rpcuser
rpcpassword=rpcpassword
rpcallowip=0.0.0.0/0
rpcbind=0.0.0.0
prune=550  # Keep only 550MB of blocks
txindex=0  # Disable transaction indexing
```

Run it with `docker compose up btcd`

Export the environment variables `BTCD_RPC_USERNAME` and `BTCD_RPC_PASSWORD` 
with the values of `rpcuser` and `rpcpassword` before running a vsc-node.
