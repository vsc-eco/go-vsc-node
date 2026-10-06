package haf

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// blockFetcher is the read-only HAF surface the pull loop and the HiveBlocks
// read methods need. *Client implements it against a live HAF database; tests
// substitute a stub.
type blockFetcher interface {
	Head(ctx context.Context) (uint64, error)
	// FetchRange returns the headers of every block in [start, end] (even
	// blocks with no tracked operations) plus every tracked operation row in
	// that range, ordered by (block_num, trx_in_block, op_pos_real) so real
	// operations arrive in consensus order per transaction and virtual
	// operations (op_pos_real NULL) sort to the tail.
	FetchRange(ctx context.Context, start, end uint64) ([]BlockRow, []OpRow, error)
}

// BlockRow is one row of hive.irreversible_blocks_view joined with
// hive.irreversible_accounts_view for the producer name.
type BlockRow struct {
	Num          uint64
	Hash         []byte // block id; hex-encode for hivego's BlockID form
	CreatedAt    time.Time
	MerkleRoot   []byte // hex-encode for hivego's TransactionMerkleRoot form
	ProducerName string
}

// OpRow is one row of hive.irreversible_operations_view left-joined with
// hive.irreversible_transactions_view for the transaction hash.
type OpRow struct {
	BlockNum   uint64
	TrxInBlock int32
	// OpPosReal is the operation's position among the transaction's real
	// (non-virtual) operations. It is NULL for virtual operations, which is
	// also how virtual ops are distinguished here.
	OpPosReal *int32
	// Body is the jsonb {"type": "..._operation", "value": {...}} operation
	// payload as stored by HAF.
	Body []byte
	// TrxHash is the raw transaction hash (transaction_id when hex-encoded).
	// Nil for block-level virtual operations (trx_in_block = -1).
	TrxHash []byte
}

// Client is a read-only HAF database client. All queries hit the public
// hive.irreversible_*_view views — never the hafd schema directly.
type Client struct {
	pool *pgxpool.Pool
}

// NewClient connects to a HAF database using a PostgreSQL connection string
// and verifies connectivity.
func NewClient(ctx context.Context, connString string) (*Client, error) {
	pool, err := pgxpool.New(ctx, connString)
	if err != nil {
		return nil, fmt.Errorf("haf: connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("haf: ping: %w", err)
	}
	return &Client{pool: pool}, nil
}

func (c *Client) Close() {
	if c != nil && c.pool != nil {
		c.pool.Close()
	}
}

// Head returns the highest irreversible block height. Verified safe as the
// ingestion cursor horizon: hive.set_irreversible copies blocks, transactions
// and operations in a single transaction, so a visible block always has its
// operations visible too.
func (c *Client) Head(ctx context.Context) (uint64, error) {
	var num *int64
	err := c.pool.QueryRow(ctx, `SELECT MAX(num) FROM hive.irreversible_blocks_view`).Scan(&num)
	if err != nil {
		return 0, fmt.Errorf("haf: head: %w", err)
	}
	if num == nil {
		return 0, nil
	}
	return uint64(*num), nil
}

// FetchRange returns the headers of every block in [start, end] plus every
// tracked operation row in that range. Blocks without tracked operations are
// still returned (the state engine must tick every height).
func (c *Client) FetchRange(ctx context.Context, start, end uint64) ([]BlockRow, []OpRow, error) {
	headers, err := c.fetchHeaders(ctx, start, end)
	if err != nil {
		return nil, nil, err
	}
	ops, err := c.fetchOps(ctx, start, end)
	if err != nil {
		return nil, nil, err
	}
	return headers, ops, nil
}

func (c *Client) fetchHeaders(ctx context.Context, start, end uint64) ([]BlockRow, error) {
	rows, err := c.pool.Query(ctx, `
		SELECT b.num, b.hash, b.created_at, b.transaction_merkle_root, a.name
		FROM hive.irreversible_blocks_view b
		JOIN hive.irreversible_accounts_view a ON a.id = b.producer_account_id
		WHERE b.num BETWEEN $1 AND $2
		ORDER BY b.num`,
		int64(start), int64(end))
	if err != nil {
		return nil, fmt.Errorf("haf: fetch headers %d-%d: %w", start, end, err)
	}
	defer rows.Close()

	var out []BlockRow
	for rows.Next() {
		var r BlockRow
		var num int64
		if err := rows.Scan(&num, &r.Hash, &r.CreatedAt, &r.MerkleRoot, &r.ProducerName); err != nil {
			return nil, fmt.Errorf("haf: scan header: %w", err)
		}
		r.Num = uint64(num)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("haf: headers cursor: %w", err)
	}
	return out, nil
}

func (c *Client) fetchOps(ctx context.Context, start, end uint64) ([]OpRow, error) {
	rows, err := c.pool.Query(ctx, `
		SELECT o.block_num, o.trx_in_block, o.op_pos_real, o.body, t.trx_hash
		FROM hive.irreversible_operations_view o
		LEFT JOIN hive.irreversible_transactions_view t
			ON t.block_num = o.block_num AND t.trx_in_block = o.trx_in_block
		WHERE o.block_num BETWEEN $1 AND $2
		ORDER BY o.block_num, o.trx_in_block, o.op_pos_real`,
		int64(start), int64(end))
	if err != nil {
		return nil, fmt.Errorf("haf: fetch ops %d-%d: %w", start, end, err)
	}
	defer rows.Close()

	var out []OpRow
	for rows.Next() {
		var r OpRow
		var num int64
		if err := rows.Scan(&num, &r.TrxInBlock, &r.OpPosReal, &r.Body, &r.TrxHash); err != nil {
			return nil, fmt.Errorf("haf: scan op: %w", err)
		}
		r.BlockNum = uint64(num)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("haf: ops cursor: %w", err)
	}
	return out, nil
}
