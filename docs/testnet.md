# Accessing The Testnet

### L1 Details:

- API endpoint: https://testnet.techcoderx.com/
- Explorer: https://testnet.techcoderx.com/explorer
- Chain ID: `18dcf0a285365fc58b71f18b3d3fec954aa0c141c44e4e5cb4cf777b9eab274e`
- Image: https://gitlab.syncad.com/hive/haf/container_registry/668
- Version: 1.28.6-alpha6

### Magi Details:

- GraphQL endpoint: https://magi-test.techcoderx.com/api/v1/graphql
- Explorer: https://magi-test.techcoderx.com/
- Network ID: `vsc-testnet`
- Node arg: `-network testnet`

### Wallets

Metamask Snap and Ledger respects the chain ID config on Aioha. Keychain requires setting up the network through adding a custom API and specifying the chain ID, then having the node selected. Clive is recommended for a CLI/TUI wallet, and may be the easiest way to send custom json operations pertaining to Magi.

#### Clive CLI Setup

If you choose to use Clive CLI, follow this setup guide!

1. Grab the CLI or TUI start script from this Git Gist: https://gist.github.com/miloridenour/eb2477b0ca3f0694fcb92d49d19064e7  
   The official scripts are here, but currently (02/25/2026) have a deprecated image name: https://gtg.openhive.network/get/clive/  
   The remaining instructions pertain specifically to the CLI. If you want to use the TUI, it should be a bit more intuitive.
2. Create a profile. Keys and configuration options will be specific to this profile, so you can create one profile for testnet and one for mainnet, for example.
3. Configure for L1 testnet: `clive configure chain-id set 18dcf0a285365fc58b71f18b3d3fec954aa0c141c44e4e5cb4cf777b9eab274e`
4. Set RPC Node: `clive configure node set https://testnet.techcoderx.com`
5. Add your keys: `clive configure key add [OPTIONS] [KEY] [ALIAS]`
