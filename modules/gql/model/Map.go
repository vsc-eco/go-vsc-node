package model

import (
	"encoding/json"
	"errors"
	"io"
)

var errMap = errors.New("must be a JSON object")

type Map map[string]interface{}

// MarshalGQL writes the map to the writer as JSON.
func (m Map) MarshalGQL(w io.Writer) {
	json.NewEncoder(w).Encode(m)
}

// UnmarshalGQL reads JSON into the map. encoding/json's errors name the Go
// type ("into Go value of type model.Map"), so a fixed message is returned.
func (m *Map) UnmarshalGQL(v interface{}) error {
	data, err := json.Marshal(v)
	if err != nil {
		return errMap
	}
	if err := json.Unmarshal(data, m); err != nil {
		return errMap
	}
	return nil
}
