package price

import (
	"encoding/json"
	"testing"

	"github.com/go-playground/validator/v10"
	"github.com/stretchr/testify/assert"
)

func TestPricePointJsonUnmarshaler(t *testing.T) {
	t.Run("ummarshal data", func(t *testing.T) {
		testCases := [][]byte{
			[]byte(`{"current_price":1.0,"symbol":"A"}`),
			[]byte(`{"current_price":1.0,"symbol":"ABCDEFGHI"}`),
			[]byte(`{"current_price":1.0,"symbol":"ABC123"}`),

			[]byte(`{"symbol":"BTC","current_price":0.01}`),
			[]byte(`{"symbol":"BTC","current_price":123}`),
			[]byte(`{"symbol":"BTC","current_price":123.456}`),
		}

		for _, raw := range testCases {
			t.Logf("unmarshaling json: %s", string(raw))
			assert.NoError(t, json.Unmarshal(raw, &PricePoint{}))
		}
	})

	t.Run("nil byte", func(t *testing.T) {
		err := json.Unmarshal(nil, &PricePoint{})

		assert.Error(t, err)

		_, ok := err.(*json.SyntaxError)
		assert.True(t, ok)
	})

	t.Run("data validation", func(t *testing.T) {
		t.Run("with invalid json data", func(t *testing.T) {
			testCases := [][]byte{
				// empty json
				[]byte(`{}`),

				// invalid prices
				[]byte(`{"symbol":"BTC","current_price":0.0}`),
				[]byte(`{"symbol":"BTC","current_price":0}`),
				[]byte(`{"symbol":"BTC","current_price":-0.0}`),
				[]byte(`{"symbol":"BTC","current_price":-0.0000001}`),

				// invalid symbols
				[]byte(`{"current_price":123.45,"symbol":""}`),
				[]byte(`{"current_price":123.45,"symbol":"abc"}`),
				[]byte(`{"current_price":123.45,"symbol":"ABCDEFGHIJ"}`),
				[]byte(`{"current_price":123.45,"symbol":"ABC;"}`),

				// valid symbols, invalid prices
				[]byte(`{"symbol":"BTC","current_price":0}`),
				[]byte(`{"symbol":"BTC","current_price":0.0}`),
				[]byte(`{"symbol":"BTC","current_price":-0.0}`),
				[]byte(`{"symbol":"BTC","current_price":-0.00001}`),

				// invalid symbols, valid prices
				[]byte(`{"current_price":1234.56,"symbol":"abc"}`),
				[]byte(`{"current_price":1234.56,"symbol":"ABC;"}`),
				[]byte(`{"current_price":1234.56,"symbol":""}`),
			}

			for _, raw := range testCases {
				t.Logf("unmarshaling json: %s", string(raw))

				err := json.Unmarshal(raw, &PricePoint{})
				assert.Error(t, err)

				_, ok := err.(validator.ValidationErrors)
				assert.True(t, ok)
			}
		})
	})
}
