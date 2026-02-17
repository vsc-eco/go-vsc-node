package tss_helpers

import (
	"math/big"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetThreshold(t *testing.T) {
	tests := []struct {
		name      string
		n         int
		want      int
		wantError bool
	}{
		{"negative", -1, 0, true},
		{"zero", 0, -1, false},  // ceil(0*2/3)-1 = -1
		{"one", 1, 0, false},    // ceil(2/3)-1 = 1-1 = 0
		{"two", 2, 1, false},    // ceil(4/3)-1 = 2-1 = 1
		{"three", 3, 1, false},  // ceil(2)-1 = 2-1 = 1
		{"four", 4, 2, false},   // ceil(8/3)-1 = 3-1 = 2
		{"five", 5, 3, false},  // ceil(10/3)-1 = 4-1 = 3
		{"six", 6, 3, false},   // ceil(4)-1 = 3
		{"nine", 9, 5, false},  // ceil(6)-1 = 5
		{"ten", 10, 6, false},  // ceil(20/3)-1 = 7-1 = 6
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := GetThreshold(tt.n)
			if tt.wantError {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got, "GetThreshold(%d)", tt.n)
		})
	}
}

func TestMsgToHashInt_InvalidAlgo(t *testing.T) {
	_, err := MsgToHashInt([]byte("msg"), SigningAlgo("invalid"))
	assert.Error(t, err)
}

func TestMsgToHashInt_Eddsa(t *testing.T) {
	msg := []byte{0x01, 0x02, 0x03}
	got, err := MsgToHashInt(msg, SigningAlgoEddsa)
	require.NoError(t, err)
	want := new(big.Int).SetBytes(msg)
	assert.Equal(t, want.Cmp(got), 0, "Eddsa: bytes as big.Int")
}

func TestMsgToHashInt_Ecdsa(t *testing.T) {
	msg := make([]byte, 32)
	for i := range msg {
		msg[i] = byte(i)
	}
	got, err := MsgToHashInt(msg, SigningAlgoEcdsa)
	require.NoError(t, err)
	require.NotNil(t, got)
	// ECDSA path uses hashToInt which truncates to curve order; just check we get a valid non-nil result
	assert.True(t, got.Sign() >= 0)
}
