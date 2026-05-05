package state_engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	state_checkpoint "vsc-node/modules/db/vsc/state_checkpoint"

	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/readconcern"
	"go.mongodb.org/mongo-driver/mongo/writeconcern"
)

// TxStarter is the minimum capability the state engine needs from the
// underlying Mongo client to drive slot-scoped transactions. The concrete
// db.Db (and *mongo.Client) satisfy it via the embedded mongo.Client.
type TxStarter interface {
	StartSession(opts ...*options.SessionOptions) (mongo.Session, error)
}

// CheckpointDb is the optional dependency used to persist last-committed-slot.
// May be nil in tests; when nil, commitSlot still commits the transaction but
// doesn't write a checkpoint document.
type CheckpointDb = state_checkpoint.StateCheckpoint

// slotTxn holds the in-flight Mongo session and transaction for one slot.
// se.slot is non-nil while a slot is being processed and nil after a clean
// shutdown or before the very first ProcessBlock call.
//
// session is nil when no TxStarter was supplied; in that case ctx is just a
// plain context.Background() and writes done with it behave exactly as they
// did before this refactor (auto-commit per operation).
type slotTxn struct {
	session    mongo.Session
	ctx        context.Context
	slotHeight uint64
}

// beginSlot opens a new Mongo session and starts a transaction that will
// span every ProcessBlock call for the slot starting at slotHeight. Reads
// done with se.SlotCtx() observe in-flight writes via snapshot read concern;
// writes join the transaction.
//
// In the no-TxStarter fallback (tests, paths wired without a Mongo client),
// se.slot still tracks slotHeight but ctx is context.Background() and there
// is no transactional boundary. This keeps the rest of the slot path
// unchanged for non-transactional callers.
func (se *StateEngine) beginSlot(slotHeight uint64) error {
	if se.slot != nil {
		return fmt.Errorf("beginSlot: slot %d already in flight", se.slot.slotHeight)
	}

	if se.txStarter == nil {
		se.slot = &slotTxn{
			ctx:        context.Background(),
			slotHeight: slotHeight,
		}
		return nil
	}

	sess, err := se.txStarter.StartSession()
	if err != nil {
		return fmt.Errorf("beginSlot: StartSession: %w", err)
	}

	txnOpts := options.Transaction().
		SetReadConcern(readconcern.Snapshot()).
		SetWriteConcern(writeconcern.Majority())
	if err := sess.StartTransaction(txnOpts); err != nil {
		sess.EndSession(context.Background())
		return fmt.Errorf("beginSlot: StartTransaction: %w", err)
	}

	se.slot = &slotTxn{
		session:    sess,
		ctx:        mongo.NewSessionContext(context.Background(), sess),
		slotHeight: slotHeight,
	}
	return nil
}

// commitSlot writes the state_checkpoint document inside the slot's still-open
// transaction and commits. On commit error the transaction is aborted so the
// checkpoint stays at the prior slot. The session is ended either way.
func (se *StateEngine) commitSlot() error {
	if se.slot == nil {
		return errors.New("commitSlot: no slot in flight")
	}
	defer func() {
		if se.slot != nil && se.slot.session != nil {
			se.slot.session.EndSession(context.Background())
		}
		se.slot = nil
	}()

	if se.slot.session == nil {
		// non-transactional fallback: nothing to commit
		return nil
	}

	if se.checkpointDb != nil {
		if err := se.checkpointDb.Set(se.slot.ctx, se.slot.slotHeight, time.Now().Unix()); err != nil {
			_ = se.slot.session.AbortTransaction(context.Background())
			return fmt.Errorf("commitSlot: write checkpoint: %w", err)
		}
	}

	if err := se.slot.session.CommitTransaction(context.Background()); err != nil {
		_ = se.slot.session.AbortTransaction(context.Background())
		return fmt.Errorf("commitSlot: CommitTransaction: %w", err)
	}
	return nil
}

// abortSlot best-effort aborts the in-flight transaction. Used on fatal
// errors / panic recovery where commit is not safe.
func (se *StateEngine) abortSlot() {
	if se.slot == nil {
		return
	}
	if se.slot.session != nil {
		_ = se.slot.session.AbortTransaction(context.Background())
		se.slot.session.EndSession(context.Background())
	}
	se.slot = nil
}

// SlotCtx returns the context that all DB operations done from the slot path
// must use. Writes join the slot's transaction (or run auto-commit when no
// TxStarter was wired). Reads see in-flight writes via snapshot read concern.
//
// Always safe to call: returns context.Background() when no slot is open.
func (se *StateEngine) SlotCtx() context.Context {
	if se.slot == nil || se.slot.ctx == nil {
		return context.Background()
	}
	return se.slot.ctx
}
