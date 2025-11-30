package itest

import (
	"bytes"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/record"
	"github.com/stretchr/testify/require"
)

// testOnionMessage tests forwarding of onion messages.
func testOnionMessageForwarding(ht *lntest.HarnessTest) {
	// Spin up a three node because we will need a three-hop network for
	// this test.
	alice := ht.NewNodeWithCoins("Alice", nil)
	bob := ht.NewNodeWithCoins("Bob", nil)
	carol := ht.NewNode("Carol", nil)

	// Create a session key for the blinded path.
	blindingKey, err := btcec.NewPrivateKey()
	require.NoError(ht.T, err)

	sessionKey, err := btcec.NewPrivateKey()
	require.NoError(ht.T, err)

	// Connect nodes before channel opening so that they can share gossip.
	ht.ConnectNodesPerm(alice, bob)
	ht.ConnectNodesPerm(bob, carol)

	// Open channels: Alice --- Bob --- Carol and wait for each node to
	// sync the network graph.
	aliceBobChanPoint := ht.OpenChannel(
		alice, bob, lntest.OpenChannelParams{
			Amt: 500_000,
		},
	)
	ht.AssertNumChannelUpdates(carol, aliceBobChanPoint, 2)

	bobCarolChanPoint := ht.OpenChannel(
		bob, carol, lntest.OpenChannelParams{
			Amt: 500_000,
		},
	)
	ht.AssertNumChannelUpdates(alice, bobCarolChanPoint, 2)

	// Create a blinded route

	// Create a set of 2 blinded hops for our path.
	hopsToBlind := make([]*sphinx.HopInfo, 2)

	// Our path is: Alice -> Bob -> Carol
	// So Bob needs to receive the public Key of Carol.

	carolPubKey, err := btcec.ParsePubKey(carol.PubKey[:])
	require.NoError(ht.T, err)
	data0 := record.NewNonFinalBlindedRouteDataOnionMessage(
		carolPubKey, nil, nil, nil,
	)
	encoded0, err := record.EncodeBlindedRouteData(data0)
	require.NoError(ht.T, err)

	data1 := &record.BlindedRouteData{}
	encoded1, err := record.EncodeBlindedRouteData(data1)
	require.NoError(ht.T, err)

	bobPubKey, err := btcec.ParsePubKey(bob.PubKey[:])
	require.NoError(ht.T, err)

	// The first hop is for Bob. This will be blinded at a later stage.
	hopsToBlind[0] = &sphinx.HopInfo{
		NodePub:   bobPubKey,
		PlainText: encoded0,
	}
	// The second hop is to Carol.
	hopsToBlind[1] = &sphinx.HopInfo{
		NodePub:   carolPubKey,
		PlainText: encoded1,
	}

	blindedPath, err := sphinx.BuildBlindedPath(blindingKey, hopsToBlind)
	require.NoError(ht.T, err)

	finalHopPayload := &lnwire.FinalHopPayload{
		TLVType: lnwire.InvoiceRequestNamespaceType,
		Value:   []byte{1, 2, 3},
	}

	// Convert that blinded path to a sphinx path and add a final payload.
	sphinxPath, err := blindedToSphinx(
		blindedPath.Path, nil, nil, []*lnwire.FinalHopPayload{
			finalHopPayload,
		},
	)
	require.NoError(ht.T, err)

	// Create an onion packet with no associated data.
	onionPacket, err := sphinx.NewOnionPacket(
		sphinxPath, sessionKey, nil, sphinx.DeterministicPacketFiller,
		sphinx.WithMaxPayloadSize(sphinx.MaxRoutingPayloadSize),
	)
	require.NoError(ht.T, err, "new onion packet")

	buf := new(bytes.Buffer)
	err = onionPacket.Encode(buf)
	require.NoError(ht.T, err, "encode onion packet")

	// Subscribe Carol to onion messages before we send any, so that we
	// don't miss any.
	msgClient, cancel := carol.RPC.SubscribeOnionMessages()
	defer cancel()

	// Create a channel to receive onion messages on.
	messages := make(chan *lnrpc.OnionMessageUpdate)
	go func() {
		for {
			// If we fail to receive, just exit. The test should
			// fail elsewhere if it doesn't get a message that it
			// was expecting.
			msg, err := msgClient.Recv()
			if err != nil {
				return
			}

			// Deliver the message into our channel or exit if the
			// test is shutting down.
			select {
			case messages <- msg:
			case <-ht.Context().Done():
				return
			}
		}
	}()

	pathKey := blindingKey.PubKey().SerializeCompressed()

	// Send it from Alice to Bob.
	aliceMsg := &lnrpc.SendOnionMessageRequest{
		Peer:    bob.PubKey[:],
		PathKey: pathKey,
		Onion:   buf.Bytes(),
	}
	alice.RPC.SendOnionMessage(aliceMsg)

	msg, err := assertOnionMessageReceived(ht, messages, bob.PubKey[:])
	require.NoError(ht, err)

	require.NotEmpty(ht, msg.CustomRecords)
	customRecordsKey := uint64(lnwire.InvoiceRequestNamespaceType)
	require.NotNil(ht, msg.CustomRecords[customRecordsKey])
	require.Equal(
		ht, []byte{1, 2, 3},
		msg.CustomRecords[customRecordsKey],
	)

	ht.CloseChannel(alice, aliceBobChanPoint)
	ht.CloseChannel(bob, bobCarolChanPoint)
}

// testOnionMessageForwardingPeerOffline tests forwarding of onion messages
// when a peer is offline.
func testOnionMessageForwardingPeerOffline(ht *lntest.HarnessTest) {
	// Start up a 4 node network: Alice -> Bob -> Carol -> Dave.
	alice := ht.NewNodeWithCoins("Alice", nil)
	bob := ht.NewNodeWithCoins("Bob", nil)
	carol := ht.NewNode("Carol", nil)
	dave := ht.NewNode("Dave", nil)

	// This will be used to blind the node IDs in the path.
	blindingKey, err := btcec.NewPrivateKey()
	require.NoError(ht.T, err)

	sessionKey, err := btcec.NewPrivateKey()
	require.NoError(ht.T, err)

	// Connect nodes but not bob and carol so that bob cannot forward the
	// message to carol.
	ht.ConnectNodesPerm(alice, bob)
	ht.ConnectNodesPerm(carol, dave)

	// Create a set of 3 blinded hops for our path.
	hopsToBlind := make([]*sphinx.HopInfo, 3)
	
	// Create the route data for each hop.
	carolPubKey, err := btcec.ParsePubKey(carol.PubKey[:])
	require.NoError(ht.T, err)
	data0 := record.NewNonFinalBlindedRouteDataOnionMessage(
		carolPubKey, nil, nil, nil,
	)
	encoded0, err := record.EncodeBlindedRouteData(data0)
	require.NoError(ht.T, err)

	davePubKey, err := btcec.ParsePubKey(dave.PubKey[:])
	require.NoError(ht.T, err)
	data1 := record.NewNonFinalBlindedRouteDataOnionMessage(
		davePubKey, nil, nil, nil,
	)
	encoded1, err := record.EncodeBlindedRouteData(data1)
	require.NoError(ht.T, err)

	data2 := &record.BlindedRouteData{}
	encoded2, err := record.EncodeBlindedRouteData(data2)
	require.NoError(ht.T, err)

	bobPubKey, err := btcec.ParsePubKey(bob.PubKey[:])
	require.NoError(ht.T, err)
	

	// The first hop is to Bob from Alice.
	hopsToBlind[0] = &sphinx.HopInfo{
		NodePub:   bobPubKey,
		PlainText: encoded0,
	}

	// The second hop is to Carol.
	hopsToBlind[1] = &sphinx.HopInfo{
		NodePub:   carolPubKey,
		PlainText: encoded1,
	}

	// The third hop is to Dave.
	hopsToBlind[2] = &sphinx.HopInfo{
		NodePub:   davePubKey,
		PlainText: encoded2,
	}

	blindedPath, err := sphinx.BuildBlindedPath(blindingKey, hopsToBlind)
	require.NoError(ht.T, err)

	finalHopPayload := &lnwire.FinalHopPayload{
		TLVType: lnwire.InvoiceRequestNamespaceType,
		Value:   []byte{1, 2, 3},
	}

	// Convert that blinded path to a sphinx path and add a final payload.
	sphinxPath, err := blindedToSphinx(
		blindedPath.Path, nil, nil, []*lnwire.FinalHopPayload{
			finalHopPayload,
		},
	)
	require.NoError(ht.T, err)

	// Create an onion packet with no associated data.
	onionPacket, err := sphinx.NewOnionPacket(
		sphinxPath, sessionKey, nil, sphinx.DeterministicPacketFiller,
		sphinx.WithMaxPayloadSize(sphinx.MaxRoutingPayloadSize),
	)
	require.NoError(ht.T, err, "new onion packet")

	buf := new(bytes.Buffer)
	err = onionPacket.Encode(buf)
	require.NoError(ht.T, err, "encode onion packet")

	// Subscribe Dave to onion messages before we send any, so that we
	// don't miss any.
	msgClient, cancel := dave.RPC.SubscribeOnionMessages()
	defer cancel()

	// Create a channel to receive onion messages on.
	messages := make(chan *lnrpc.OnionMessageUpdate)
	go func() {
		for {
			// If we fail to receive, just exit. The test should
			// fail elsewhere if it doesn't get a message that it
			// was expecting.
			msg, err := msgClient.Recv()
			if err != nil {
				return
			}

			// Deliver the message into our channel or exit if the
			// test is shutting down.
			select {
			case messages <- msg:
			case <-ht.Context().Done():
				return
			}
		}
	}()

	pathKey := blindingKey.PubKey().SerializeCompressed()

	// Send it from Alice to Bob.
	aliceMsg := &lnrpc.SendOnionMessageRequest{
		Peer:    bob.PubKey[:],
		PathKey: pathKey,
		Onion:   buf.Bytes(),
	}
	alice.RPC.SendOnionMessage(aliceMsg)

	// Dave should not receive the message because Carol is offline.
	msg, err := assertOnionMessageReceived(ht, messages, carol.PubKey[:])
	require.Error(ht, err)
	require.Nil(ht, msg)
}

// testOnionMessageReplyPath tests that a reply path is correctly included in
// an onion message and received by the final hop. We also test that the reply
// path is used to send a reply message back to the sender node.
//
// nolint:ll
func testOnionMessageReplyPath(ht *lntest.HarnessTest) {
	// Spin up a three node network: Alice -> Bob -> Carol.
	alice := ht.NewNodeWithCoins("Alice", nil)
	bob := ht.NewNodeWithCoins("Bob", nil)
	carol := ht.NewNode("Carol", nil)

	// Connect nodes.
	ht.ConnectNodesPerm(alice, bob)
	ht.ConnectNodesPerm(bob, carol)

	// Create a session key for the blinded path.
	blindingKey, err := btcec.NewPrivateKey()
	require.NoError(ht.T, err)

	// Create a reply path. The reply path will be from Bob to Alice.
	// So we need to create a blinded path that starts with Bob as the
	// introduction node and ends with Alice as the final blinded hop.
	replyBlindingKey, err := btcec.NewPrivateKey()
	require.NoError(ht, err)

	alicePubKey, err := btcec.ParsePubKey(alice.PubKey[:])
	require.NoError(ht, err)

	bobPubKey, err := btcec.ParsePubKey(bob.PubKey[:])
	require.NoError(ht, err)

	// Data for Bob (Introduction Node). Bob needs to know to forward to
	// Alice.
	replyDataBob := record.NewNonFinalBlindedRouteDataOnionMessage(
		alicePubKey, nil, nil, nil,
	)
	encodedReplyDataBob, err := record.EncodeBlindedRouteData(replyDataBob)
	require.NoError(ht, err)

	// Data for Alice (final hop of reply path).
	replyDataAlice := &record.BlindedRouteData{}
	encReplyDataAlice, err := record.EncodeBlindedRouteData(replyDataAlice)
	require.NoError(ht, err)

	// Create the blinded hops.
	hopsToBlindReply := []*sphinx.HopInfo{
		{
			NodePub:   bobPubKey,
			PlainText: encodedReplyDataBob,
		},
		{
			NodePub:   alicePubKey,
			PlainText: encReplyDataAlice,
		},
	}

	replyBlindedPath, err := sphinx.BuildBlindedPath(
		replyBlindingKey, hopsToBlindReply,
	)
	require.NoError(ht, err)

	// Convert sphinx.BlindedPath to lnwire.ReplyPath.
	var replyHops []*lnwire.BlindedHop
	for _, h := range replyBlindedPath.Path.BlindedHops {
		replyHops = append(replyHops, &lnwire.BlindedHop{
			BlindedNodeID: h.BlindedNodePub,
			EncryptedData: h.CipherText,
		})
	}

	bobPubKey, err = btcec.ParsePubKey(bob.PubKey[:])
	require.NoError(ht, err)

	replyPath := &lnwire.ReplyPath{
		FirstNodeID:   bobPubKey,
		BlindingPoint: replyBlindedPath.Path.BlindingPoint,
		Hops:          replyHops,
	}

	// Create the forward path (Alice -> Bob -> Carol).
	hopsToBlind := make([]*sphinx.HopInfo, 2)

	carolPubKey, err := btcec.ParsePubKey(carol.PubKey[:])
	require.NoError(ht, err)

	data0 := record.NewNonFinalBlindedRouteDataOnionMessage(
		carolPubKey, nil, nil, nil,
	)
	encoded0, err := record.EncodeBlindedRouteData(data0)
	require.NoError(ht, err)

	data1 := &record.BlindedRouteData{}
	encoded1, err := record.EncodeBlindedRouteData(data1)
	require.NoError(ht, err)

	// The first hop is for Bob.
	hopsToBlind[0] = &sphinx.HopInfo{
		NodePub:   bobPubKey,
		PlainText: encoded0,
	}

	// The second hop is to Carol.
	hopsToBlind[1] = &sphinx.HopInfo{
		NodePub:   carolPubKey,
		PlainText: encoded1,
	}

	blindedPath, err := sphinx.BuildBlindedPath(blindingKey, hopsToBlind)
	require.NoError(ht, err)

	finalHopPayload := &lnwire.FinalHopPayload{
		TLVType: lnwire.InvoiceRequestNamespaceType,
		Value:   []byte{1, 2, 3},
	}

	// Embed Reply Path and Send. Convert the blinded path to a sphinx path,
	// including the reply path.
	sphinxPath, err := blindedToSphinx(
		blindedPath.Path, nil, replyPath, []*lnwire.FinalHopPayload{
			finalHopPayload,
		},
	)
	require.NoError(ht, err)

	// Create a session key for the onion packet.
	sessionKey, err := btcec.NewPrivateKey()
	require.NoError(ht, err)

	// Create an onion packet with no associated data.
	onionPacket, err := sphinx.NewOnionPacket(
		sphinxPath, sessionKey, nil, sphinx.DeterministicPacketFiller,
		sphinx.WithMaxPayloadSize(sphinx.MaxRoutingPayloadSize),
	)
	require.NoError(ht, err)

	buf := new(bytes.Buffer)
	err = onionPacket.Encode(buf)
	require.NoError(ht, err)

	// Subscribe Carol to onion messages.
	msgClient, cancel := carol.RPC.SubscribeOnionMessages()
	defer cancel()

	// Create a channel to receive onion messages on.
	messages := make(chan *lnrpc.OnionMessageUpdate)
	go func() {
		for {
			msg, err := msgClient.Recv()
			if err != nil {
				return
			}

			select {
			case messages <- msg:
			case <-ht.Context().Done():
				return
			}
		}
	}()

	pathKey := blindingKey.PubKey().SerializeCompressed()

	// Send it from Alice to Bob.
	aliceMsg := &lnrpc.SendOnionMessageRequest{
		Peer:    bob.PubKey[:],
		PathKey: pathKey,
		Onion:   buf.Bytes(),
	}
	alice.RPC.SendOnionMessage(aliceMsg)

	msg, err := assertOnionMessageReceived(ht, messages, bob.PubKey[:])
	require.NoError(ht, err)

	require.Equal(ht, bob.PubKey[:], msg.Peer)
	require.NotNil(ht, msg.ReplyPath)

	// Construct the sphinx blinded path from the reply path.
	_, err = btcec.ParsePubKey(msg.ReplyPath.IntroductionNode)
	require.NoError(ht, err)

	blindingPointPub, err := btcec.ParsePubKey(msg.ReplyPath.BlindingPoint)
	require.NoError(ht, err)

	blindedHops := make([]*sphinx.BlindedHopInfo, len(msg.ReplyPath.BlindedHops))
	for i, h := range msg.ReplyPath.BlindedHops {
		pub, err := btcec.ParsePubKey(h.BlindedNode)
		require.NoError(ht, err)

		blindedHops[i] = &sphinx.BlindedHopInfo{
			BlindedNodePub: pub,
			CipherText:     h.EncryptedData,
		}
	}

	sphinxReplyPath := &sphinx.BlindedPath{
		BlindingPoint:    blindingPointPub,
		BlindedHops:      blindedHops,
	}

	// Use the reply path to send a message back to Alice.
	replyPayload := &lnwire.FinalHopPayload{
		TLVType: lnwire.InvoiceRequestNamespaceType,
		Value:   []byte{4, 5, 6},
	}

	replySphinxPath, err := blindedToSphinx(
		sphinxReplyPath, nil, nil, []*lnwire.FinalHopPayload{
			replyPayload,
		},
	)
	require.NoError(ht, err)

	// Create a new session key for the reply.
	replySessionKey, err := btcec.NewPrivateKey()
	require.NoError(ht, err)

	replyOnionPacket, err := sphinx.NewOnionPacket(
		replySphinxPath, replySessionKey, nil, sphinx.DeterministicPacketFiller,
		sphinx.WithMaxPayloadSize(sphinx.MaxRoutingPayloadSize),
	)
	require.NoError(ht, err)

	replyBuf := new(bytes.Buffer)
	err = replyOnionPacket.Encode(replyBuf)
	require.NoError(ht, err)

	// Subscribe Alice to onion messages.
	aliceMsgClient, aliceCancel := alice.RPC.SubscribeOnionMessages()
	defer aliceCancel()

	aliceMessages := make(chan *lnrpc.OnionMessageUpdate)
	go func() {
		for {
			msg, err := aliceMsgClient.Recv()
			if err != nil {
				return
			}
			select {
			case aliceMessages <- msg:
			case <-ht.Context().Done():
				return
			}
		}
	}()

	// Carol sends the reply to the introduction node (Bob) using the
	// blinding point.
	carolMsg := &lnrpc.SendOnionMessageRequest{
		Peer:    msg.ReplyPath.IntroductionNode,
		PathKey: msg.ReplyPath.BlindingPoint,
		Onion:   replyBuf.Bytes(),
	}
	carol.RPC.SendOnionMessage(carolMsg)

	// Verify Alice receives the reply.
	replyMsg, err := assertOnionMessageReceived(ht, aliceMessages, bob.PubKey[:])
	require.NoError(ht, err)

	require.Equal(ht, bob.PubKey[:], replyMsg.Peer)
	require.NotEmpty(ht, replyMsg.CustomRecords)
	require.Equal(ht, []byte{4, 5, 6}, replyMsg.CustomRecords[uint64(lnwire.InvoiceRequestNamespaceType)])
}

// blindedToSphinx converts the blinded path provided to a sphinx path that can
// be wrapped up in an onion, encoding the TLV payload for each hop along the
// way.
func blindedToSphinx(blindedRoute *sphinx.BlindedPath,
	extraHops []*lnwire.BlindedHop, replyPath *lnwire.ReplyPath,
	finalPayloads []*lnwire.FinalHopPayload) (
	*sphinx.PaymentPath, error) {

	var (
		sphinxPath sphinx.PaymentPath

		ourHopCount   = len(blindedRoute.BlindedHops)
		extraHopCount = len(extraHops)
	)

	// Fill in the blinded node id and encrypted data for all hops. This
	// requirement differs from blinded hops used for payments, where we
	// don't use the blinded introduction node id. However, since onion
	// messages are fully blinded by default, we use the blinded
	// introduction node id.
	for i := 0; i < ourHopCount; i++ {
		// Create an onion message payload with the encrypted data for
		// this hop.
		payload := &lnwire.OnionMessagePayload{
			EncryptedData: blindedRoute.BlindedHops[i].CipherText,
		}

		// If we're on the final hop and there are no extra hops to add
		// onto our path, include the tlvs intended for the final hop
		// and the reply path (if provided).
		if i == ourHopCount-1 && extraHopCount == 0 {
			payload.FinalHopPayloads = finalPayloads
			payload.ReplyPath = replyPath
		}

		// Encode the tlv stream for inclusion in our message.
		hop, err := createSphinxHop(
			*blindedRoute.BlindedHops[i].BlindedNodePub, payload,
		)
		if err != nil {
			return nil, fmt.Errorf("sphinx hop %v: %w", i, err)
		}
		sphinxPath[i] = *hop
	}

	// If we don't have any more hops to append to our path, just return
	// it as-is here.
	if extraHopCount == 0 {
		return &sphinxPath, nil
	}

	for i := 0; i < extraHopCount; i++ {
		payload := &lnwire.OnionMessagePayload{
			EncryptedData: extraHops[i].EncryptedData,
		}

		// If we're on the last hop, add our optional final payload
		// and reply path.
		if i == extraHopCount-1 {
			payload.FinalHopPayloads = finalPayloads
			payload.ReplyPath = replyPath
		}

		hop, err := createSphinxHop(
			*extraHops[i].BlindedNodeID, payload,
		)
		if err != nil {
			return nil, fmt.Errorf("sphinx hop %v: %w", i, err)
		}

		// We need to offset our index in the sphinx path by the
		// number of hops that we added in the loop above.
		sphinxIndex := i + ourHopCount
		sphinxPath[sphinxIndex] = *hop
	}

	return &sphinxPath, nil
}

// createSphinxHop encodes an onion message payload and produces a sphinx
// onion hop for it.
func createSphinxHop(nodeID btcec.PublicKey,
	payload *lnwire.OnionMessagePayload) (*sphinx.OnionHop, error) {

	payloadTLVs, err := payload.Encode()
	if err != nil {
		return nil, fmt.Errorf("payload: encode: %w", err)
	}

	return &sphinx.OnionHop{
		NodePub: nodeID,
		HopPayload: sphinx.HopPayload{
			Type:    sphinx.PayloadTLV,
			Payload: payloadTLVs,
		},
	}, nil
}

// assertOnionMessageReceived asserts that an onion message is received on the
// provided channel and returns the message if it is received.
func assertOnionMessageReceived(ht *lntest.HarnessTest,
	messages <-chan *lnrpc.OnionMessageUpdate,
	expectedPeer []byte) (*lnrpc.OnionMessageUpdate, error) {

	ht.Helper()

	select {
	case msg := <-messages:
		// Check that we're receiving the message from the expected
		// peer.
		require.Equal(ht, expectedPeer, msg.Peer)
		return msg, nil

	case <-time.After(lntest.DefaultTimeout):
		return nil, fmt.Errorf("did not receive onion message")
	}	
}
