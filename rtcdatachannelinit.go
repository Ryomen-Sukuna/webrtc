package webrtc

// RTCDataChannelInit can be used to configure properties of the underlying
// channel such as data reliability.
type RTCDataChannelInit struct {
	// Ordered indicates if data is allowed to be delivered out of order. The
	// default value of true, guarantees that data will be delivered in order.
	// TODO make sure defaults to true
	Ordered bool

	// MaxPacketLifeTime limits the time (in milliseconds) during which the
	// channel will transmit or retransmit data if not acknowledged. This value
	// may be clamped if it exceeds the maximum value supported.
	MaxPacketLifeTime *uint16

	// MaxRetransmits limits the number of times a channel will retransmit data
	// if not successfully delivered. This value may be clamped if it exceeds
	// the maximum value supported.
	MaxRetransmits *uint16

	// Protocol describes the subprotocol name used for this channel.
	Protocol string

	// Negotiated describes if the data channel is created by the local peer or
	// the remote peer. The default value of false tells the user agent to
	// announce the channel in-band and instruct the other peer to dispatch a
	// corresponding RTCDataChannel. If set to true, it is up to the application
	// to negotiate the channel and create an RTCDataChannel with the same id
	// at the other peer.
	Negotiated bool

	// ID overrides the default selection of ID for this channel.
	ID uint16

	// Priority describes the priority of this channel.
	Priority RTCPriorityType
}