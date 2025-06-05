package onion_message

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/msgmux"
	"github.com/lightningnetwork/lnd/record"
	"github.com/lightningnetwork/lnd/subscribe"
)

var (
	// ErrBadMessage is returned when we can't process an onion message.
	ErrBadMessage = errors.New("onion message processing failed")

	// ErrBadOnionMsg is returned when we receive a bad onion message.
	ErrBadOnionMsg = errors.New("invalid onion message")

	// ErrBadOnionBlob is returned when we receive a bad onion blob within
	// our onion message.
	ErrBadOnionBlob = errors.New("invalid onion blob")

	// ErrNoForwardingOnion is returned when we try to forward an onion
	// message but no next onion is provided.
	ErrNoForwardingOnion = errors.New("no next onion provided to forward")

	// ErrNoEncryptedData is returned when the encrypted data TLV is not
	// present when it is required.
	ErrNoEncryptedData = errors.New("encrypted data blob required")

	// ErrNoForwardingPayload is returned when no onion message payload
	// is provided to allow forwarding messages.
	ErrNoForwardingPayload = errors.New("no payload provided for " +
		"forwarding")

	// ErrNoNextNodeID is returned when we require a next node id in our
	// encrypted data blob and one was not provided.
	ErrNoNextNodeID = errors.New("next node ID required")
)

// parseOnionFunc is a function type that defines how to parse an onion
// message. It takes a byte slice representing the onion data and returns
type parseOnionFunc func([]byte) (*btcec.PublicKey, *sphinx.ProcessedPacket,
	error)

type forwardMessageFunc func(*record.BlindedRouteData, *btcec.PublicKey,
	*sphinx.OnionPacket) error

// OnionMessageSender an interface that allows for sending onion messages
// to a peer.
type OnionMessageSender func([33]byte, *btcec.PublicKey, []byte) error

// OnionEndpoint handles incoming onion messages.
type OnionEndpoint struct {
	// subscribe.Server is used for subscriptions to onion messages.
	onionMessageServer *subscribe.Server

	// decodePayload is used to decode the payload of the onion message.
	decodePayload func([]byte) (*lnwire.OnionMessagePayload, error)

	// router is the sphinx router used to process onion packets.
	router *sphinx.Router

	// MsgSender sends the target set of messages to the target peer.
	MsgSender OnionMessageSender
}

// OnionMessageUpdate is onion message update dispatched to any potential
// subscriber.
type OnionMessageUpdate struct {
	// Peer is the peer pubkey
	Peer [33]byte

	// BlindingPoint is the route blinding ephemeral pubkey to be used for
	// the onion message.
	BlindingPoint []byte

	// OnionBlob is the raw serialized mix header used to relay messages in
	// a privacy-preserving manner. This blob should be handled in the same
	// manner as onions used to route HTLCs, with the exception that it uses
	// blinded routes by default.
	OnionBlob []byte
}

// OnionEndpointOption defines a function that can be used to configure
// an OnionEndpoint. This allows for flexible configuration of the endpoint
// when creating a new instance.
type OnionEndpointOption func(*OnionEndpoint)

// WithMessageServer sets the subscribe.Server for the OnionEndpoint.
func WithMessageServer(server *subscribe.Server) OnionEndpointOption {
	return func(o *OnionEndpoint) {
		o.onionMessageServer = server
	}
}

// WithMessageServer sets the subscribe.Server for the OnionEndpoint.
func WithMessageSender(msgSender OnionMessageSender) OnionEndpointOption {
	return func(o *OnionEndpoint) {
		o.MsgSender = msgSender
	}
}

// WithSphinxRouter sets the subscribe.Server for the OnionEndpoint.
func WithSphinxRouter(router *sphinx.Router) OnionEndpointOption {
	return func(o *OnionEndpoint) {
		o.router = router
	}
}

// NewOnionEndpoint creates a new OnionEndpoint with the given options.
func NewOnionEndpoint(opts ...OnionEndpointOption) *OnionEndpoint {
	o := &OnionEndpoint{
		onionMessageServer: nil,
		decodePayload:      lnwire.DecodeOnionMessagePayload,
	}
	for _, opt := range opts {
		opt(o)
	}
	return o
}

// Name returns the unique name of the endpoint.
func (o *OnionEndpoint) Name() string {
	return "OnionMessageHandler"
}

// CanHandle checks if the endpoint can handle the incoming message.
// It returns true if the message is an lnwire.OnionMessage.
func (o *OnionEndpoint) CanHandle(msg msgmux.PeerMsg) bool {
	_, ok := msg.Message.(*lnwire.OnionMessage)
	return ok
}

// SendMessage processes the incoming onion message.
// It returns true if the message was successfully processed.
func (o *OnionEndpoint) SendMessage(ctx context.Context,
	msg msgmux.PeerMsg) bool {

	onionMsg, ok := msg.Message.(*lnwire.OnionMessage)
	if !ok {
		return false
	}

	peer := msg.PeerPub.SerializeCompressed()
	log.Debugf("OnionEndpoint received OnionMessage from peer %x: "+
		"BlindingPoint=%v, OnionPacket[:10]=%10x...", peer,
		onionMsg.BlindingPoint, onionMsg.OnionBlob)

	var peerArr [33]byte
	copy(peerArr[:], peer)
	err := o.onionMessageServer.SendUpdate(&OnionMessageUpdate{
		Peer:          peerArr,
		BlindingPoint: onionMsg.BlindingPoint.SerializeCompressed(),
		OnionBlob:     onionMsg.OnionBlob,
	})
	if err != nil {
		log.Errorf("Failed to send onion message update: %v", err)
		return false
	}

	err = o.handleOnionMessage(*onionMsg)
	if err != nil {
		log.Errorf("Failed to handle onion message: %v", err)
		return false
	}

	return true
}

// handleOnionMessage decodes and processes an onion message.
func (o *OnionEndpoint) handleOnionMessage(msg lnwire.OnionMessage) error {
	blinding, processedPacket, err := o.parseOnion(msg)
	if err != nil {
		return fmt.Errorf("%w: could not process onion packet: %v",
			ErrBadOnionBlob, err)
	}

	// Decode the TLV stream in our payload.
	payloadBytes := processedPacket.Payload.Payload
	payload, err := o.decodePayload(payloadBytes)
	if err != nil {
		return fmt.Errorf("%w: could not process payload: %v",
			ErrBadOnionBlob, err)
	}

	switch processedPacket.Action {
	// We only support forwarding at present, so we fail if we are the exit
	// node for an onion.
	case sphinx.ExitNode:
		return errors.New("receiving onion messages is not supported")

	// Forward the onion message if we are a forwarding node.
	case sphinx.MoreHops:
		// If we don't have a next packet, we can't forward the onion
		if processedPacket.NextPacket == nil {
			return ErrNoForwardingOnion
		}

		// Decrypt the data blob using the blinding point and payload.
		data, err := o.decryptDataBlob(blinding, payload)
		if err != nil {
			return fmt.Errorf("could not decrypt data blob: %w",
				err)
		}

		// Forward the message using the next packet.
		return o.forwardMessage(
			data, blinding, processedPacket.NextPacket,
		)

	// If we encounter a sphinx failure, just log the error and ignore the
	// packet.
	case sphinx.Failure:
		return ErrBadMessage
	}

	return nil
}

// parseOnion decodes onion messages and decrypts them using the endpoint's
// router.
func (o *OnionEndpoint) parseOnion(onionMsg lnwire.OnionMessage) (*btcec.PublicKey,
	*sphinx.ProcessedPacket, error) {

	// The onion blob portion of our message holds the actual onion.
	onionPktBytes := bytes.NewBuffer(onionMsg.OnionBlob)

	onionPkt := &sphinx.OnionPacket{}
	if err := onionPkt.Decode(onionPktBytes); err != nil {
		return nil, nil, fmt.Errorf("%w:%v", ErrBadOnionBlob, err)
	}

	processed, err := o.router.ProcessOnionPacket(
		onionPkt, nil, 0,
		sphinx.WithBlindingPoint(onionMsg.BlindingPoint),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("error process packet with blinding point %x: %w", onionMsg.BlindingPoint.SerializeCompressed(), err)
	}

	return onionMsg.BlindingPoint, processed, nil
}

// decryptBlobFunc returns a closure that can be used to decrypt an onion
// message's encrypted data blob and decode it.
func (o *OnionEndpoint) decryptDataBlob(blindingPoint *btcec.PublicKey,
	payload *lnwire.OnionMessagePayload) (*record.BlindedRouteData,
	error) {

	if payload == nil {
		return nil, ErrNoForwardingPayload
	}

	if len(payload.EncryptedData) == 0 {
		return nil, ErrNoEncryptedData
	}

	decrypted, err := o.router.DecryptBlindedHopData(
		blindingPoint, payload.EncryptedData,
	)
	if err != nil {
		return nil, fmt.Errorf("could not decrypt data "+
			"blob: %w", err)
	}

	buf := bytes.NewBuffer(decrypted)
	data, err := record.DecodeBlindedRouteData(buf)
	if err != nil {
		return nil, fmt.Errorf("could not decode data "+
			"blob: %w", err)
	}

	return data, nil
}

func (o *OnionEndpoint) forwardMessage(data *record.BlindedRouteData, blindingPoint *btcec.PublicKey,
	onionPacket *sphinx.OnionPacket) error {

	if data.NextNodeID.IsNone() {
		return ErrNoNextNodeID
	}

	nextBlinding, err := o.router.NextEphemeral(blindingPoint)
	if err != nil {
		return fmt.Errorf("could not calculate next ephemeral: %w", err)
	}

	// If we have a blinding override included in our encrypted data, it
	// should be directly switched out.
	if data.NextBlindingOverride.IsSome() {
		nextBlinding = data.NextBlindingOverride.UnsafeFromSome().Val

		log.Infof("Ephemeral switch out: %x for %x",
			blindingPoint.SerializeCompressed(),
			nextBlinding.SerializeCompressed())
	}

	buf := new(bytes.Buffer)
	if err := onionPacket.Encode(buf); err != nil {
		return fmt.Errorf("could not encode packet: %w", err)
	}

	nextNodeID := data.NextNodeID.UnsafeFromSome().Val

	log.Infof("Forwarding onion message to: %v, next blinding: %x",
		nextNodeID, nextBlinding.SerializeCompressed())

	var nextNodeIDBytes [33]byte
	copy(nextNodeIDBytes[:], nextNodeID.SerializeCompressed())

	err = o.MsgSender(
		nextNodeIDBytes, nextBlinding, buf.Bytes(),
	)

	if err != nil {
		return fmt.Errorf("could not send message: %w", err)
	}

	return nil
}

// A compile-time check to ensure OnionEndpoint implements the Endpoint
// interface.
var _ msgmux.Endpoint = (*OnionEndpoint)(nil)
