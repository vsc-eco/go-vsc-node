package witnesses

type EmptyWitnesses struct {
	witnesses []Witness
}

func NewEmptyWitnesses() *EmptyWitnesses {
	return &EmptyWitnesses{
		witnesses: make([]Witness, 0),
	}
}

func (w *EmptyWitnesses) GetLastestWitnesses(...SearchOption) ([]Witness, error) {
	return w.witnesses, nil
}

func (w *EmptyWitnesses) GetWitnessAtHeight(string, *uint64) (*Witness, error) {
	return nil, nil
}
