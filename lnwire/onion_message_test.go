package lnwire

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
)

// TestOnionMessageEncodeDecode tests encoding and decoding of OnionMessage.
func TestOnionMessageEncodeDecode(t *testing.T) {
	t.Parallel()

	// Generate a test path key.
	pathKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	testCases := []struct {
		name      string
		onionBlob []byte
	}{
		{
			name:      "empty onion blob",
			onionBlob: []byte{},
		},
		{
			name:      "small onion blob",
			onionBlob: []byte{1, 2, 3, 4, 5},
		},
		{
			name:      "typical onion blob",
			onionBlob: bytes.Repeat([]byte{0xAB}, 1366),
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			original := NewOnionMessage(
				pathKey.PubKey(), tc.onionBlob,
			)

			// Encode the message.
			var buf bytes.Buffer
			err := original.Encode(&buf, 0)
			require.NoError(t, err)

			// Decode the message.
			decoded := &OnionMessage{}
			err = decoded.Decode(&buf, 0)
			require.NoError(t, err)

			// Verify the decoded message matches the original.
			require.Equal(
				t, original.PathKey.SerializeCompressed(),
				decoded.PathKey.SerializeCompressed(),
			)
			require.Equal(t, original.OnionBlob, decoded.OnionBlob)
		})
	}
}

// TestOnionMessageMsgType tests that MsgType returns the correct type.
func TestOnionMessageMsgType(t *testing.T) {
	t.Parallel()

	msg := &OnionMessage{}
	msgType := msg.MsgType()
	require.Equal(t, MessageType(MsgOnionMessage), msgType)
}

// TestOnionMessageSerializedSize tests the SerializedSize method.
func TestOnionMessageSerializedSize(t *testing.T) {
	t.Parallel()

	pathKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	testCases := []struct {
		name         string
		onionBlob    []byte
		expectedSize uint32
	}{
		{
			name:      "empty onion blob",
			onionBlob: []byte{},
			// 2 byte msg type + 33 byte pubkey + 2 byte length
			// + 0 bytes onion blob.
			expectedSize: 2 + 33 + 2,
		},
		{
			name:      "with onion blob",
			onionBlob: []byte{1, 2, 3, 4, 5},
			// 2 byte msg type + 33 byte pubkey + 2 byte length + 5
			// bytes onion blob.
			expectedSize: 2 + 33 + 2 + 5,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			msg := NewOnionMessage(pathKey.PubKey(), tc.onionBlob)
			size, err := msg.SerializedSize()
			require.NoError(t, err)
			require.Equal(t, tc.expectedSize, size)
		})
	}
}

// TestOnionMessagePayloadEncodeDecode tests basic encoding and decoding
// of OnionMessagePayload.
func TestOnionMessagePayloadEncodeDecode(t *testing.T) {
	t.Parallel()

	t.Run("empty payload", func(t *testing.T) {
		t.Parallel()

		original := NewOnionMessagePayload()
		encoded, err := original.Encode()
		require.NoError(t, err)

		decoded := NewOnionMessagePayload()
		_, err = decoded.Decode(bytes.NewReader(encoded))
		require.NoError(t, err)

		require.Nil(t, decoded.ReplyPath)
		require.Empty(t, decoded.EncryptedData)
		require.Empty(t, decoded.FinalHopTLVs)
	})

	t.Run("with encrypted data", func(t *testing.T) {
		t.Parallel()

		original := NewOnionMessagePayload()
		original.EncryptedData = []byte{1, 2, 3, 4, 5}

		encoded, err := original.Encode()
		require.NoError(t, err)

		decoded := NewOnionMessagePayload()
		_, err = decoded.Decode(bytes.NewReader(encoded))
		require.NoError(t, err)

		require.Equal(t, original.EncryptedData, decoded.EncryptedData)
	})

	t.Run("with final hop tlvs", func(t *testing.T) {
		t.Parallel()

		original := NewOnionMessagePayload()
		original.FinalHopTLVs = []*FinalHopTLV{
			{
				TLVType: InvoiceRequestNamespaceType,
				Value:   []byte{0xDE, 0xAD, 0xBE, 0xEF},
			},
		}

		encoded, err := original.Encode()
		require.NoError(t, err)

		decoded := NewOnionMessagePayload()
		_, err = decoded.Decode(bytes.NewReader(encoded))
		require.NoError(t, err)

		require.Len(t, decoded.FinalHopTLVs, 1)
		require.Equal(
			t, InvoiceRequestNamespaceType,
			decoded.FinalHopTLVs[0].TLVType,
		)
		require.Equal(
			t, original.FinalHopTLVs[0].Value,
			decoded.FinalHopTLVs[0].Value,
		)
	})
}

// TestOnionMessagePayloadReplyPath tests encoding and decoding of
// OnionMessagePayload with a ReplyPath.
//
//nolint:ll
func TestOnionMessagePayloadReplyPath(t *testing.T) {
	t.Parallel()

	// Generate keys for the reply path.
	introKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	blindingKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	blindedNodeKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	t.Run("single hop reply path", func(t *testing.T) {
		t.Parallel()

		original := NewOnionMessagePayload()
		original.ReplyPath = &sphinx.BlindedPath{
			IntroductionPoint: introKey.PubKey(),
			BlindingPoint:     blindingKey.PubKey(),
			BlindedHops: []*sphinx.BlindedHopInfo{
				{
					BlindedNodePub: blindedNodeKey.PubKey(),
					CipherText:     []byte{1, 2, 3},
				},
			},
		}

		encoded, err := original.Encode()
		require.NoError(t, err)

		decoded := NewOnionMessagePayload()
		_, err = decoded.Decode(bytes.NewReader(encoded))
		require.NoError(t, err)

		require.NotNil(t, decoded.ReplyPath)
		require.Equal(
			t, introKey.PubKey().SerializeCompressed(),
			decoded.ReplyPath.IntroductionPoint.SerializeCompressed(),
		)
		require.Equal(
			t, blindingKey.PubKey().SerializeCompressed(),
			decoded.ReplyPath.BlindingPoint.SerializeCompressed(),
		)
		require.Len(t, decoded.ReplyPath.BlindedHops, 1)
		require.Equal(
			t, blindedNodeKey.PubKey().SerializeCompressed(),
			decoded.ReplyPath.BlindedHops[0].BlindedNodePub.SerializeCompressed(),
		)
		require.Equal(
			t, []byte{1, 2, 3},
			decoded.ReplyPath.BlindedHops[0].CipherText,
		)
	})

	t.Run("multi hop reply path", func(t *testing.T) {
		t.Parallel()

		blindedNodeKey2, err := btcec.NewPrivateKey()
		require.NoError(t, err)

		blindedNodeKey3, err := btcec.NewPrivateKey()
		require.NoError(t, err)

		original := NewOnionMessagePayload()
		original.ReplyPath = &sphinx.BlindedPath{
			IntroductionPoint: introKey.PubKey(),
			BlindingPoint:     blindingKey.PubKey(),
			BlindedHops: []*sphinx.BlindedHopInfo{
				{
					BlindedNodePub: blindedNodeKey.PubKey(),
					CipherText:     []byte{1, 2, 3},
				},
				{
					BlindedNodePub: blindedNodeKey2.PubKey(),
					CipherText:     []byte{4, 5, 6, 7, 8},
				},
				{
					BlindedNodePub: blindedNodeKey3.PubKey(),
					CipherText:     []byte{9, 10},
				},
			},
		}

		encoded, err := original.Encode()
		require.NoError(t, err)

		decoded := NewOnionMessagePayload()
		_, err = decoded.Decode(bytes.NewReader(encoded))
		require.NoError(t, err)

		require.NotNil(t, decoded.ReplyPath)
		require.Len(t, decoded.ReplyPath.BlindedHops, 3)

		// Verify each hop.
		for i, hop := range original.ReplyPath.BlindedHops {
			require.Equal(
				t, hop.BlindedNodePub.SerializeCompressed(),
				decoded.ReplyPath.BlindedHops[i].BlindedNodePub.SerializeCompressed(),
			)
			require.Equal(
				t, hop.CipherText,
				decoded.ReplyPath.BlindedHops[i].CipherText,
			)
		}
	})

	t.Run("reply path with all fields", func(t *testing.T) {
		t.Parallel()

		original := NewOnionMessagePayload()
		original.EncryptedData = []byte{0xCA, 0xFE}
		original.ReplyPath = &sphinx.BlindedPath{
			IntroductionPoint: introKey.PubKey(),
			BlindingPoint:     blindingKey.PubKey(),
			BlindedHops: []*sphinx.BlindedHopInfo{
				{
					BlindedNodePub: blindedNodeKey.PubKey(),
					CipherText:     []byte{1, 2, 3},
				},
			},
		}
		original.FinalHopTLVs = []*FinalHopTLV{
			{
				TLVType: InvoiceNamespaceType,
				Value:   []byte{0xBE, 0xEF},
			},
		}

		encoded, err := original.Encode()
		require.NoError(t, err)

		decoded := NewOnionMessagePayload()
		_, err = decoded.Decode(bytes.NewReader(encoded))
		require.NoError(t, err)

		// Verify all fields were decoded correctly.
		require.NotNil(t, decoded.ReplyPath)
		require.Equal(t, original.EncryptedData, decoded.EncryptedData)
		require.Len(t, decoded.FinalHopTLVs, 1)
		require.Equal(
			t, InvoiceNamespaceType,
			decoded.FinalHopTLVs[0].TLVType,
		)
	})

	t.Run("encode fails with zero hops", func(t *testing.T) {
		t.Parallel()

		payload := NewOnionMessagePayload()
		payload.ReplyPath = &sphinx.BlindedPath{
			IntroductionPoint: introKey.PubKey(),
			BlindingPoint:     blindingKey.PubKey(),
			BlindedHops:       []*sphinx.BlindedHopInfo{},
		}

		_, err := payload.Encode()
		require.ErrorIs(t, err, ErrNoHops)
	})
}

// TestFinalHopTLVValidate tests the Validate method of FinalHopTLV.
func TestFinalHopTLVValidate(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name      string
		tlvType   tlv.Type
		expectErr bool
	}{
		{
			name:      "valid type at boundary",
			tlvType:   64,
			expectErr: false,
		},
		{
			name:      "valid large type",
			tlvType:   1000,
			expectErr: false,
		},
		{
			name:      "invalid type below boundary",
			tlvType:   63,
			expectErr: true,
		},
		{
			name:      "invalid type zero",
			tlvType:   0,
			expectErr: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			finalHop := &FinalHopTLV{
				TLVType: tc.tlvType,
				Value:   []byte{1, 2, 3},
			}

			err := finalHop.Validate()
			if tc.expectErr {
				require.ErrorIs(t, err, ErrNotFinalPayload)
			} else {
				require.NoError(t, err)
			}
		})
	}
}
