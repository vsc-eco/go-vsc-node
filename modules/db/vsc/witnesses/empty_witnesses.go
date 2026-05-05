package witnesses

import "context"

type EmptyWitnesses struct {
	witnesses []Witness
}

func NewEmptyWitnesses() *EmptyWitnesses {
	return &EmptyWitnesses{
		witnesses: make([]Witness, 0),
	}
}

func (w *EmptyWitnesses) GetLastestWitnesses(_ context.Context, _ ...SearchOption) ([]Witness, error) {
	return w.witnesses, nil
}

func (w *EmptyWitnesses) GetWitnessAtHeight(_ context.Context, _ string, _ *uint64) (*Witness, error) {
	return nil, nil
}
