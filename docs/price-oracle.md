# Oracle

## p2p

Subscribes to the topic `/vsc/mainet/oracle/v1`.

The method `OracleP2pSpec.HandleMessage` is a multiplexer for different types of
messages. For any incoming message, it will stamp the current time, and send
the data to any the appropriate channel, depends on the message type.

## Price Oracle

Channels:

- `pricePollTicker`: ticks every 10 seconds, issue concurrent requests to APIs 
  (Coinmarketcap, CoinGecko).
  - Incoming data is formatted and sent to `observePriceChan`.
- `broadcastPriceSignal`:
  - Emitted via `Oracle.blockTick` with a signal (which includes flags to 
    indicate if the current node is a witness/block producer).
  - From all the price point queried, calculate and broadcast the average price 
    and volume.
  - If the current node is a block producer (`Oracle.pollMedianPriceSignature`), 
    - Collects all the average price points, and find the median price/volume.
    - Make a new block:
      - ID construction: 
        - `hex(timestamp + sha256.Sum256(username + activeKey + json(data)))`
        - Results in 40 byte (8 byte time stamp, 32 byte sha256), or 80 char 
          hex string.
    - Listens in the network for 10 seconds for incoming signatures (room for 
      latency).
      - If not enough signatures collected, returns error.
      - Rejects any price points received outside the time 1 hour time window.
      - Collects and (TODO) verifies 2/3 of the witnesses' signatures.
    - TODO: Submit the signed block to the contract.
  - If the current node is a witness (TODO)
    - Verifies and signs any block received within the last 10 minutes.
  - Clear in-memory buffers (broadcastPriceBuf, newBlockBuf, and oracle price 
    map).
- `broadcastPriceBlockChan`: buffered channel, any new block sent by the block 
  producer will is stored in memory.
- `broadcastPriceChan`: to receive any incoming broadcasted price points from 
  other nodes, stored in memory.
- `msgChan`: for different submodules to broadcast any messages in the network.
- `observePriceChan`: incoming price point, stored in memory, for later
  processing (tick emitted via `broadcastPriceSignal`).
- `priceBlockSignatureChan`: buffered channel for incoming `VSCBlock` with 
  signatures.

## Blockchain Relay

TODO
