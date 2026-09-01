package poaseats

import (
	"context"
	"errors"
	"fmt"

	"vsc-node/modules/db"
	"vsc-node/modules/db/vsc"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type poaSeats struct {
	*db.Collection
}

// New constructs the seat registry over the "poa_seats" collection.
func New(d *vsc.VscDb) PoaSeats {
	return &poaSeats{db.NewCollection(d.DbInstance, "poa_seats")}
}

func (p *poaSeats) Init() error {
	if err := p.Collection.Init(); err != nil {
		return err
	}

	// One seat per account. Unique at the STORAGE layer as well as in AdmitSeat:
	// the handler check is the deterministic consensus rule, this index is the
	// backstop that turns a logic slip into a loud write failure rather than a
	// silently duplicated seat (a duplicate would double an operator's votes and
	// break the one-operator-one-vote property the whole design rests on).
	if err := p.CreateIndexIfNotExist(mongo.IndexModel{
		Keys:    bson.D{{Key: "account", Value: 1}},
		Options: options.Index().SetUnique(true),
	}); err != nil {
		return fmt.Errorf("poa_seats: account index: %w", err)
	}

	// One seat per beneficial owner (A5). Sparse so bootstrap seats, which have
	// no vetted UBO yet, do not all collide on the empty string.
	if err := p.CreateIndexIfNotExist(mongo.IndexModel{
		Keys:    bson.D{{Key: "ubo_id", Value: 1}},
		Options: options.Index().SetUnique(true).SetSparse(true),
	}); err != nil {
		return fmt.Errorf("poa_seats: ubo index: %w", err)
	}

	// Height-addressed reads (the hot path, once per election on every node).
	if err := p.CreateIndexIfNotExist(mongo.IndexModel{
		Keys: bson.D{{Key: "admitted_height", Value: 1}},
	}); err != nil {
		return fmt.Errorf("poa_seats: height index: %w", err)
	}

	return nil
}

func (p *poaSeats) GetSeatsAtHeight(height uint64) ([]Seat, error) {
	ctx := context.Background()

	// Sorted by account so every node builds the identical ordered set from the
	// identical rows. Election generation is CID-committed; an unordered read
	// would be a latent determinism bug that only shows up under load.
	cursor, err := p.Find(ctx,
		bson.M{"admitted_height": bson.M{"$lte": height}},
		options.Find().SetSort(bson.D{{Key: "account", Value: 1}}),
	)
	if err != nil {
		return nil, fmt.Errorf("poa_seats: find at height %d: %w", height, err)
	}
	defer cursor.Close(ctx)

	seats := make([]Seat, 0)
	if err := cursor.All(ctx, &seats); err != nil {
		return nil, fmt.Errorf("poa_seats: decode at height %d: %w", height, err)
	}
	if err := cursor.Err(); err != nil {
		return nil, fmt.Errorf("poa_seats: cursor at height %d: %w", height, err)
	}
	return seats, nil
}

func (p *poaSeats) GetSeat(account string) (Seat, bool, error) {
	acct := NormalizeAccount(account)
	if acct == "" {
		return Seat{}, false, nil
	}

	seat := Seat{}
	err := p.FindOne(context.Background(), bson.M{"account": acct}).Decode(&seat)
	if errors.Is(err, mongo.ErrNoDocuments) {
		// Deterministic absence, not a failure: every node reading the same
		// state gets the same answer. Distinguished from a transient read error
		// (returned as err) precisely so consensus callers can fail-stop on the
		// latter instead of treating a Mongo blip as "no seat" and diverging.
		return Seat{}, false, nil
	}
	if err != nil {
		return Seat{}, false, fmt.Errorf("poa_seats: get %s: %w", acct, err)
	}
	return seat, true, nil
}

func (p *poaSeats) GetSeatByUbo(uboId string) (Seat, bool, error) {
	if uboId == "" {
		return Seat{}, false, nil
	}

	seat := Seat{}
	err := p.FindOne(context.Background(), bson.M{"ubo_id": uboId}).Decode(&seat)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return Seat{}, false, nil
	}
	if err != nil {
		return Seat{}, false, fmt.Errorf("poa_seats: get by ubo: %w", err)
	}
	return seat, true, nil
}

// Sentinel errors so callers can classify a refusal with errors.Is instead of
// matching message text. A duplicate seat/owner is a DETERMINISTIC refusal
// (every node replaying the same history reaches it), and the state engine must
// tell that apart from a transient infra error — retrying a deterministic
// refusal under blockingRetry wedges block processing forever. Message-substring
// classification silently breaks the moment a wrapper changes the text; a typed
// error does not.
var (
	// ErrSeatExists: the account already holds a seat.
	ErrSeatExists = errors.New("poa_seats: account already holds a seat")
	// ErrUboExists: the beneficial owner already holds a seat.
	ErrUboExists = errors.New("poa_seats: beneficial owner already holds a seat")
)

func (p *poaSeats) AdmitSeat(seat Seat) error {
	seat.Account = NormalizeAccount(seat.Account)
	if seat.Account == "" {
		return errors.New("poa_seats: refusing to admit an empty account")
	}
	if seat.AdmittedHeight == 0 {
		// 0 would make the seat match every height-addressed read including
		// heights before it existed, rewriting the past on reindex.
		return errors.New("poa_seats: refusing to admit at height 0")
	}

	if _, exists, err := p.GetSeat(seat.Account); err != nil {
		return err
	} else if exists {
		return fmt.Errorf("%w: %s", ErrSeatExists, seat.Account)
	}

	if seat.UboId != "" {
		if held, exists, err := p.GetSeatByUbo(seat.UboId); err != nil {
			return err
		} else if exists {
			return fmt.Errorf("%w (held by %s)", ErrUboExists, held.Account)
		}
	}

	// The unique indexes are the backstop: the checks above race against a
	// concurrent admission, so a duplicate can still reach InsertOne. Map the
	// storage-layer E11000 back to the same sentinels so the caller classifies
	// it identically to the pre-checks.
	if _, err := p.InsertOne(context.Background(), seat); err != nil {
		if mongo.IsDuplicateKeyError(err) {
			return fmt.Errorf("%w (insert race): %w", ErrSeatExists, err)
		}
		return fmt.Errorf("poa_seats: insert %s: %w", seat.Account, err)
	}
	return nil
}

func (p *poaSeats) SetSeating(account string, height uint64) error {
	acct := NormalizeAccount(account)
	if acct == "" {
		return nil
	}
	// Re-entry clears ExitHeight, which RE-ARMS the collateral halt rather than
	// releasing it: an operator back in the set holds keys again, so their bond
	// must be locked again. Grace restores the seat; it never accelerates a
	// withdrawal.
	//
	// ★ MONOTONIC GUARD (last_seated_height <= height). Clearing exit_height is
	// the one write in this package that RELEASES a collateral hold, so it must
	// only ever be driven by a NEWER election than the one already recorded.
	// Without the guard, any path that reprocesses an older election — a replay,
	// a re-ingested block, a reorg-driven re-execution — would clear a live exit
	// and let a departing operator's bond out early, which is precisely the
	// steal-then-withdraw escape the halt exists to close. Safety must not rest
	// on the caller always invoking this in increasing height order.
	_, err := p.UpdateOne(context.Background(),
		bson.M{"account": acct, "last_seated_height": bson.M{"$lte": height}},
		bson.M{"$set": bson.M{"last_seated_height": height, "exit_height": uint64(0)}},
	)
	if err != nil {
		return fmt.Errorf("poa_seats: set seating %s: %w", acct, err)
	}
	return nil
}

func (p *poaSeats) SetExit(account string, height uint64) error {
	acct := NormalizeAccount(account)
	if acct == "" {
		return nil
	}
	// Guarded on exit_height==0 and last_seated_height>0, which makes this
	// idempotent in the way that matters: once an exit is recorded, later
	// elections that also exclude the account cannot push the height forward and
	// restart the halt clock. Without the guard, an operator who exits and stays
	// out would have their 3-day clock reset every election interval — i.e. the
	// halt would never expire, turning a temporary lock into a permanent seizure.
	_, err := p.UpdateOne(context.Background(),
		bson.M{
			"account":            acct,
			"exit_height":        uint64(0),
			"last_seated_height": bson.M{"$gt": uint64(0)},
		},
		bson.M{"$set": bson.M{"exit_height": height}},
	)
	if err != nil {
		return fmt.Errorf("poa_seats: set exit %s: %w", acct, err)
	}
	return nil
}
