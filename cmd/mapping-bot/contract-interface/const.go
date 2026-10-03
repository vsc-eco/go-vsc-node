package contractinterface

const DirPathDelimiter = "-"

const BalancePrefix = "a" + DirPathDelimiter
const ObservedPrefix = "o" + DirPathDelimiter
const UtxoPrefix = "u" + DirPathDelimiter
const UtxoRegistryKey = "r"
const UtxoLastIdKey = "i"
const TxSpendsRegistryKey = "p"
const TxSpendsPrefix = "d" + DirPathDelimiter
const SupplyKey = "s"

const LastHeightKey = "h"

const PrimaryPublicKeyStateKey = "pubkey"
const BackupPublicKeyStateKey = "backupkey"

// ---------------------------------------------------------------------------
// BTC vault-rotation-v2 (must stay byte-identical to the contract's
// btc-mapping-contract/contract/constants: a mismatch silently reads the wrong
// state key and the rotation driver goes blind).
// ---------------------------------------------------------------------------

// VaultRegistryKey holds the packed vault-generation registry (the "v" list).
// Absent/empty on a non-vault contract and on a pre-rotation deploy — the
// rotation driver treats both as "nothing to do".
const VaultRegistryKey = "v"

// MigrationSweepPrefix keys the per-sweep migration record ("ms-<txid>"). Its
// presence means a migration sweep is in flight and not yet settled.
const MigrationSweepPrefix = "ms" + DirPathDelimiter

// PendingUnmapPrefix keys the per-unmap record ("us-<txid>", delete-at-confirm).
const PendingUnmapPrefix = "us" + DirPathDelimiter
