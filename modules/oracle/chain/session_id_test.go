package chain

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseChainSessionID_Valid(t *testing.T) {
	symbol, start, end, err := parseChainSessionID("BTC-640000-640100")
	require.NoError(t, err)
	assert.Equal(t, "BTC", symbol)
	assert.Equal(t, uint64(640000), start)
	assert.Equal(t, uint64(640100), end)
}

func TestParseChainSessionID_SingleBlock(t *testing.T) {
	symbol, start, end, err := parseChainSessionID("DASH-12345-12345")
	require.NoError(t, err)
	assert.Equal(t, "DASH", symbol)
	assert.Equal(t, uint64(12345), start)
	assert.Equal(t, uint64(12345), end)
}

func TestParseChainSessionID_InvalidFormat(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{"empty", ""},
		{"no dashes", "BTC640000640100"},
		{"one dash", "BTC-640000"},
		{"four parts", "BTC-640-000-100"},
		{"non-numeric start", "BTC-abc-640100"},
		{"non-numeric end", "BTC-640000-xyz"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, _, err := parseChainSessionID(tt.input)
			assert.Error(t, err)
		})
	}
}

// mockChainBlock implements chainBlock for testing.
type mockChainBlock struct {
	height    uint64
	data      string
	chainType string // defaults to "TEST" if empty
}

func (m *mockChainBlock) Type() string {
	if m.chainType != "" {
		return m.chainType
	}
	return "TEST"
}
func (m *mockChainBlock) Serialize() (string, error) { return m.data, nil }
func (m *mockChainBlock) BlockHeight() uint64        { return m.height }

func TestMakeChainSessionID(t *testing.T) {
	session := &chainSession{
		symbol: "BTC",
		chainData: []chainBlock{
			&mockChainBlock{height: 640000},
			&mockChainBlock{height: 640050},
			&mockChainBlock{height: 640100},
		},
	}

	id, err := makeChainSessionID(session)
	require.NoError(t, err)
	assert.Equal(t, "BTC-640000-640100", id)
}

func TestMakeChainSessionID_EmptyChainData(t *testing.T) {
	session := &chainSession{
		symbol:    "BTC",
		chainData: []chainBlock{},
	}

	_, err := makeChainSessionID(session)
	assert.Error(t, err)
}

func TestSessionIDRoundTrip(t *testing.T) {
	session := &chainSession{
		symbol: "DASH",
		chainData: []chainBlock{
			&mockChainBlock{height: 100},
			&mockChainBlock{height: 200},
		},
	}

	id, err := makeChainSessionID(session)
	require.NoError(t, err)

	symbol, start, end, err := parseChainSessionID(id)
	require.NoError(t, err)
	assert.Equal(t, "DASH", symbol)
	assert.Equal(t, uint64(100), start)
	assert.Equal(t, uint64(200), end)
}
