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
		{0, 1}, // 0 -> 1 (ensure non-zero)
		{1, 1},
		{99, 99},
		{100, 1},
		{101, 1},
		{199, 99},
		{200, 1},
	}
	for _, tt := range tests {
		got := makeEpochIdx(tt.epoch)
		assert.Equal(t, tt.want, got, "makeEpochIdx(%d)", tt.epoch)
	}
}
