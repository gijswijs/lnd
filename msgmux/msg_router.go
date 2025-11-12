package msgmux

import (
	"context"
	"fmt"
	"maps"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/actor"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lnwire"
)

var (
	// ErrDuplicateEndpoint is returned when an endpoint is registered with
	// a name that already exists.
	ErrDuplicateEndpoint = fmt.Errorf("endpoint already registered")

	// ErrUnableToRouteMsg is returned when a message is unable to be
	// routed to any endpoints.
	ErrUnableToRouteMsg = fmt.Errorf("unable to route message")
)

// EndpointName is the name of a given endpoint. This MUST be unique across all
// registered endpoints.
type EndpointName = string

// PeerMsg is a wire message that includes the public key of the peer that sent
// it.
type PeerMsg struct {
	lnwire.Message

	// PeerPub is the public key of the peer that sent this message.
	PeerPub btcec.PublicKey
}

// Endpoint is an interface that represents a message endpoint, or the
// sub-system that will handle processing an incoming wire message.
type Endpoint interface {
	// Name returns the name of this endpoint. This MUST be unique across
	// all registered endpoints.
	Name() EndpointName

	// CanHandle returns true if the target message can be routed to this
	// endpoint.
	CanHandle(msg PeerMsg) bool

	// SendMessage handles the target message, and returns true if the
	// message was able being processed.
	SendMessage(ctx context.Context, msg PeerMsg) bool
}

// MsgRouter is an interface that represents a message router, which is generic
// sub-system capable of routing any incoming wire message to a set of
// registered endpoints.
type Router interface {
	// RegisterEndpoint registers a new endpoint with the router. If a
	// duplicate endpoint exists, an error is returned.
	RegisterEndpoint(Endpoint) error

	// UnregisterEndpoint unregisters the target endpoint from the router.
	UnregisterEndpoint(EndpointName) error

	// RouteMsg attempts to route the target message to a registered
	// endpoint. If ANY endpoint could handle the message, then nil is
	// returned. Otherwise, ErrUnableToRouteMsg is returned.
	RouteMsg(PeerMsg) error

	// Start starts the peer message router.
	Start(ctx context.Context)

	// Stop stops the peer message router.
	Stop()
}

// EndpointsMap is a map of all registered endpoints.
type EndpointsMap map[EndpointName]Endpoint

// routerActorMsg is a union interface for all router actor messages.
type routerActorMsg interface {
	actor.Message
	isRouterActorMsg()
}

// registerEndpointMsg is a message to register an endpoint.
type registerEndpointMsg struct {
	actor.BaseMessage
	endpoint Endpoint
}

// MessageType returns the type name of the message.
func (m *registerEndpointMsg) MessageType() string { return "registerEndpointMsg" }
func (m *registerEndpointMsg) isRouterActorMsg()   {}

// unregisterEndpointMsg is a message to unregister an endpoint.
type unregisterEndpointMsg struct {
	actor.BaseMessage
	name EndpointName
}

// MessageType returns the type name of the message.
func (m *unregisterEndpointMsg) MessageType() string { return "unregisterEndpointMsg" }
func (m *unregisterEndpointMsg) isRouterActorMsg()   {}

// routeMsg is a message to route a peer message.
type routeMsg struct {
	actor.BaseMessage
	peerMsg PeerMsg
}

// MessageType returns the type name of the message.
func (m *routeMsg) MessageType() string { return "routeMsg" }
func (m *routeMsg) isRouterActorMsg()   {}

// getEndpointsMsg is a message to get all endpoints.
type getEndpointsMsg struct {
	actor.BaseMessage
}

// MessageType returns the type name of the message.
func (m *getEndpointsMsg) MessageType() string { return "getEndpointsMsg" }
func (m *getEndpointsMsg) isRouterActorMsg()   {}

// routerBehavior is the actor behavior for the message router.
type routerBehavior struct {
	endpoints map[EndpointName]Endpoint
}

// Receive handles incoming messages for the router actor.
func (b *routerBehavior) Receive(ctx context.Context,
	msg routerActorMsg) fn.Result[any] {

	switch m := msg.(type) {
	case *registerEndpointMsg:
		log.Infof("MsgRouter: registering new Endpoint(%s)",
			m.endpoint.Name())

		if _, ok := b.endpoints[m.endpoint.Name()]; ok {
			log.Errorf("MsgRouter: rejecting duplicate endpoint: %v",
				m.endpoint.Name())
			return fn.Ok[any](ErrDuplicateEndpoint)
		}
		b.endpoints[m.endpoint.Name()] = m.endpoint
		return fn.Ok[any](nil) // nil error

	case *unregisterEndpointMsg:
		log.Infof("MsgRouter: unregistering Endpoint(%s)", m.name)
		delete(b.endpoints, m.name)
		return fn.Ok[any](nil) // nil error

	case *routeMsg:
		var couldSend bool
		for _, endpoint := range b.endpoints {
			if endpoint.CanHandle(m.peerMsg) {
				log.Tracef("MsgRouter: sending msg %T to endpoint %s",
					m.peerMsg, endpoint.Name())

				sent := endpoint.SendMessage(ctx, m.peerMsg)
				couldSend = couldSend || sent
			}
		}

		var err error
		if !couldSend {
			log.Tracef("MsgRouter: unable to route msg %T",
				m.peerMsg.Message)
			err = ErrUnableToRouteMsg
		}
		return fn.Ok[any](err)

	case *getEndpointsMsg:
		endpointsCopy := make(EndpointsMap, len(b.endpoints))
		maps.Copy(b.endpoints, endpointsCopy)
		return fn.Ok[any](endpointsCopy)

	default:
		return fn.Err[any](fmt.Errorf("unknown message type: %T", m))
	}
}

// MultiMsgRouter is a type of message router that is capable of routing new
// incoming messages, permitting a message to be routed to multiple registered
// endpoints.
type MultiMsgRouter struct {
	actor *actor.Actor[routerActorMsg, any]
}

// NewMultiMsgRouter creates a new instance of a peer message router.
func NewMultiMsgRouter() *MultiMsgRouter {
	beh := &routerBehavior{
		endpoints: make(map[EndpointName]Endpoint),
	}
	actorCfg := actor.ActorConfig[routerActorMsg, any]{
		ID:          "msg-router",
		Behavior:    beh,
		MailboxSize: 100,
	}
	routerActor := actor.NewActor(actorCfg)

	return &MultiMsgRouter{
		actor: routerActor,
	}
}

// Start starts the peer message router.
func (p *MultiMsgRouter) Start(ctx context.Context) {
	log.Infof("Starting Router")

	p.actor.Start()
}

// Stop stops the peer message router.
func (p *MultiMsgRouter) Stop() {
	log.Infof("Stopping Router")

	p.actor.Stop()
}

// RegisterEndpoint registers a new endpoint with the router. If a duplicate
// endpoint exists, an error is returned.
func (p *MultiMsgRouter) RegisterEndpoint(endpoint Endpoint) error {
	msg := &registerEndpointMsg{endpoint: endpoint}
	future := p.actor.Ref().Ask(context.Background(), msg)
	res := future.Await(context.Background())

	val, err := res.Unpack()
	if err != nil {
		return err
	}

	if typedErr, ok := val.(error); ok {
		return typedErr
	}

	return nil
}

// UnregisterEndpoint unregisters the target endpoint from the router.
func (p *MultiMsgRouter) UnregisterEndpoint(name EndpointName) error {
	msg := &unregisterEndpointMsg{name: name}
	future := p.actor.Ref().Ask(context.Background(), msg)
	res := future.Await(context.Background())

	val, err := res.Unpack()
	if err != nil {
		return err
	}

	if typedErr, ok := val.(error); ok {
		return typedErr
	}

	return nil
}

// RouteMsg attempts to route the target message to a registered endpoint. If
// ANY endpoint could handle the message, then nil is returned.
func (p *MultiMsgRouter) RouteMsg(msg PeerMsg) error {
	future := p.actor.Ref().Ask(context.Background(), &routeMsg{peerMsg: msg})
	res := future.Await(context.Background())

	val, err := res.Unpack()
	if err != nil {
		return err
	}

	if typedErr, ok := val.(error); ok {
		return typedErr
	}

	return nil
}

// Endpoints returns a list of all registered endpoints.
func (p *MultiMsgRouter) endpoints() fn.Result[EndpointsMap] {
	future := p.actor.Ref().Ask(context.Background(), &getEndpointsMsg{})
	res := future.Await(context.Background())

	val, err := res.Unpack()
	if err != nil {
		return fn.Err[EndpointsMap](err)
	}

	if endpoints, ok := val.(EndpointsMap); ok {
		return fn.Ok(endpoints)
	}

	return fn.Err[EndpointsMap](fmt.Errorf("unexpected response type: %T", val))
}

// A compile time check to ensure MultiMsgRouter implements the MsgRouter
// interface.
var _ Router = (*MultiMsgRouter)(nil)
