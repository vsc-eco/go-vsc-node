package price

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestPricePointJsonUnmarshaler(t *testing.T) {
	var (
		raw = []byte(`{"symbol": "BTC","price": 1234.56}`)
		buf PricePoint
	)

	assert.NoError(t, json.Unmarshal(raw, &buf))
	assert.Equal(t, "BTC", buf.Symbol)
	assert.Equal(t, 1234.56, buf.Price)
}
