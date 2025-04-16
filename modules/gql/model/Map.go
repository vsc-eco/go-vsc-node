package model

import (
	"encoding/json"
	"io"
)

type Map map[string]interface{}

// MarshalGQL writes the map to the writer as JSON.
func (m Map) MarshalGQL(w io.Writer) {
	json.NewEncoder(w).Encode(m)
}

// UnmarshalGQL reads JSON into the map.
func (m *Map) UnmarshalGQL(v interface{}) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, m)
}
