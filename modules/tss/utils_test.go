package tss

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestMakeEpochIdx(t *testing.T) {
	tests := []struct {
		epoch int
		want  int
	}{
		{0, 1},   // 0 -> 1 (ensure non-zero)
		{1, 1},
		{99, 99},
		{100, 100}, // no longer collides with epoch 0
		{101, 101},
		{199, 199},
		{200, 200}, // no longer collides with epoch 100
	}
	for _, tt := range tests {
		got := makeEpochIdx(tt.epoch)
		assert.Equal(t, tt.want, got, "makeEpochIdx(%d)", tt.epoch)
	}
}
