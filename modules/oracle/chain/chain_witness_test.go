package chain

import (
	"crypto/rand"
	"encoding/base64"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestTTTT(t *testing.T) {
	var buf [96]byte
	t.Log(base64.RawStdEncoding.EncodedLen(96))

	_, err := io.ReadFull(rand.Reader, buf[:])
	if err != nil {
		t.Fatal(err)
	}

	sigMsg := signatureMessage{
		Signature: base64.RawStdEncoding.EncodeToString(buf[:]),
	}

	assert.NoError(t, signatureMessageValidator.Struct(&sigMsg))

	sigMsg = signatureMessage{
		Signature: base64.RawStdEncoding.EncodeToString(buf[:90]),
	}
	assert.Error(t, signatureMessageValidator.Struct(&sigMsg))
}
