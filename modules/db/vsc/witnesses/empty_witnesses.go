package witnesses

type EmptyWitnesses struct {
	witnesses []Witness
}

func NewEmptyWitnesses() *EmptyWitnesses {
	return &EmptyWitnesses{
		witnesses: make([]Witness, 0),
	}
}

func (w *EmptyWitnesses) GetLastestWitnesses() ([]Witness, error) {
	return w.witnesses, nil
}
