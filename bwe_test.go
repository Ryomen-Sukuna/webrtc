//go:build bwe_test
// +build bwe_test

package webrtc

import (
	"context"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/packetdump"
	"github.com/pion/logging"
	"github.com/pion/rtp"
	"github.com/pion/transport/test"
	"github.com/pion/transport/vnet"
	"github.com/pion/webrtc/v3/pkg/media"
	"github.com/stretchr/testify/assert"
)

const (
	defaultReferenceCapacity = 1 * vnet.MBit
	defaultMaxBurst          = 100 * vnet.KBit

	leftCIDR = "10.0.1.0/24"

	leftPublicIP1  = "10.0.1.1"
	leftPrivateIP1 = "10.0.1.101"

	leftPublicIP2  = "10.0.1.2"
	leftPrivateIP2 = "10.0.1.102"

	leftPublicIP3  = "10.0.1.3"
	leftPrivateIP3 = "10.0.1.103"

	rightCIDR = "10.0.2.0/24"

	rightPublicIP1  = "10.0.2.1"
	rightPrivateIP1 = "10.0.2.101"

	rightPublicIP2  = "10.0.2.2"
	rightPrivateIP2 = "10.0.2.102"

	rightPublicIP3  = "10.0.2.3"
	rightPrivateIP3 = "10.0.2.103"
)

var (
	defaultVideotrack = trackConfig{
		capability: RTPCodecCapability{MimeType: MimeTypeVP8},
		id:         "video1",
		streamID:   "pion",
		codec:      newSimpleFPSBasedCodec(150 * vnet.KBit),
		startAfter: 0,
	}
	defaultAudioTrack = trackConfig{
		capability: RTPCodecCapability{MimeType: MimeTypeOpus},
		id:         "audio1",
		streamID:   "pion",
		codec:      newSimpleFPSBasedCodec(20 * vnet.KBit),
		startAfter: 0,
	}
)

type mediaSender struct {
	pc  *PeerConnection
	log logging.LeveledLogger
}

func (s *mediaSender) start(ctx context.Context, track *TrackLocalStaticSample, codec syntheticCodec) error {
	defer s.log.Info("mediaSender.start done")

	frame := codec.nextPacketOrFrame()
	if err := track.WriteSample(media.Sample{
		Data:     frame.content,
		Duration: frame.secondsToNextFrame,
	}); err != nil {
		return err
	}

	metricsTicker := time.NewTicker(1000 * time.Millisecond)
	defer metricsTicker.Stop()

	sendTimer := time.NewTimer(frame.secondsToNextFrame)
	defer sendTimer.Stop()

	bytesSent := 0
	lastLog := time.Now()
	for {
		select {
		case <-ctx.Done():
			return nil
		case now := <-metricsTicker.C:
			d := now.Sub(lastLog)
			lastLog = now
			bits := float64(bytesSent) * 8
			rate := bits / d.Seconds()
			rateInMbit := rate / float64(vnet.MBit)
			bytesSent = 0
			s.log.Infof("[track %v] sending rate: %.2f Mb/s\n", track.ID(), rateInMbit)

		case <-sendTimer.C:
			frame = codec.nextPacketOrFrame()
			err := track.WriteSample(media.Sample{
				Data:     frame.content,
				Duration: frame.secondsToNextFrame,
			})
			if err != nil {
				s.log.Infof("received error on track.WriteSample: %v\n", err)
				return err
			}

			// fmt.Printf("written sample, size=%v, duration=%v\n", len(frame.content), frame.secondsToNextFrame)
			sendTimer.Reset(frame.secondsToNextFrame)
			bytesSent += len(frame.content)
		}
	}
}

type mediaReceiver struct {
	pc  *PeerConnection
	log logging.LeveledLogger
}

func (r *mediaReceiver) record(ctx context.Context, c <-chan *rtp.Packet, trackID string) {
	ticker := time.NewTicker(1000 * time.Millisecond)
	bytesReceived := 0
	lastLog := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			d := now.Sub(lastLog)
			lastLog = now
			bits := float64(bytesReceived) * 8
			rate := bits / d.Seconds()
			rateInMbit := rate / float64(vnet.MBit)
			r.log.Infof("[track %v] receiving rate: %.2f Mb/s\n", trackID, rateInMbit)
			bytesReceived = 0

		case pkt := <-c:
			bytesReceived += pkt.MarshalSize()
		}
	}
}

func (r *mediaReceiver) onTrack(trackRemote *TrackRemote, rtpReceiver *RTPReceiver) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := make(chan *rtp.Packet)
	go r.record(ctx, c, trackRemote.ID())
	for {
		if err := rtpReceiver.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
			r.log.Errorf("failed to SetReadDeadline for rtpReceiver: %v", err)
		}
		if err := trackRemote.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
			r.log.Errorf("failed to SetReadDeadline for trackRemote: %v", err)
		}

		rtpPacket, _, err := trackRemote.ReadRTP()
		if err == io.EOF {
			r.log.Info("trackRemote.ReatRTP received EOF")
			return
		}
		if err != nil {
			r.log.Errorf("trackRemote.ReatRTP returned unexpected error: %v", err)
			return
		}

		c <- rtpPacket
	}
}

func TestBandwidthEstimation(t *testing.T) {
	subtest := func(t *testing.T, tc testcase) {
		lim := test.TimeOut(2 * tc.totalDuration)
		defer lim.Stop()

		report := test.CheckRoutines(t)
		defer report()

		log := logging.NewDefaultLoggerFactory().NewLogger("test")

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		leftRouter, rightRouter, wan, err := createNetwork(&tc.left, &tc.right)
		assert.NoError(t, err)

		leftRouter.tbf.Set(vnet.TBFRate(tc.referenceCapacity), vnet.TBFMaxBurst(defaultMaxBurst))
		rightRouter.tbf.Set(vnet.TBFRate(tc.referenceCapacity), vnet.TBFMaxBurst(defaultMaxBurst))

		startTrackFns := []func(){}
		for i := range tc.forward {
			var startTrack []func()
			startTrack, err = createPeers(ctx, &tc.forward[i], leftRouter, rightRouter)
			assert.NoError(t, err)
			startTrackFns = append(startTrackFns, startTrack...)
		}
		for i := range tc.backward {
			var startTrack []func()
			startTrack, err = createPeers(ctx, &tc.backward[i], rightRouter, leftRouter)
			assert.NoError(t, err)
			startTrackFns = append(startTrackFns, startTrack...)
		}

		for _, startTrack := range startTrackFns {
			go startTrack()
		}

		go func() {
			for _, phase := range tc.forwardPhases {
				nextRate := int64(float64(tc.referenceCapacity) * phase.capacityRatio)
				rightRouter.tbf.Set(vnet.TBFRate(nextRate), vnet.TBFMaxBurst(defaultMaxBurst))
				log.Infof("updated forward link capacity to %v", nextRate)
				select {
				case <-ctx.Done():
					return
				case <-time.After(phase.duration):
				}
			}
		}()
		go func() {
			for _, phase := range tc.backwardPhases {
				nextRate := int64(float64(tc.referenceCapacity) * phase.capacityRatio)
				leftRouter.tbf.Set(vnet.TBFRate(nextRate), vnet.TBFMaxBurst(defaultMaxBurst))
				log.Infof("updated backward link capacity to %v", nextRate)
				select {
				case <-ctx.Done():
					return
				case <-time.After(phase.duration):
				}
			}
		}()

		time.Sleep(tc.totalDuration)

		for _, p := range tc.forward {
			closePairNow(t, p.sender.pc, p.receiver.pc)
		}
		for _, p := range tc.backward {
			closePairNow(t, p.sender.pc, p.receiver.pc)
		}

		assert.NoError(t, wan.Stop())
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			subtest(t, tc)
		})
	}
}

type router struct {
	*vnet.Router
	tbf *vnet.TokenBucketFilter
}

type routerConfig struct {
	cidr      string
	staticIPs []string
}

// TODO(mathis): Add parameters for network condition
func createNetwork(left, right *routerConfig) (*router, *router, *vnet.Router, error) {
	wan, err := vnet.NewRouter(&vnet.RouterConfig{
		CIDR:          "0.0.0.0/0",
		LoggerFactory: logging.NewDefaultLoggerFactory(),
		MinDelay:      50 * time.Millisecond,
		MaxJitter:     30 * time.Millisecond,
	})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to create WAN router: %w", err)
	}

	leftRouter, err := vnet.NewRouter(&vnet.RouterConfig{
		CIDR:          left.cidr,
		StaticIPs:     left.staticIPs,
		LoggerFactory: logging.NewDefaultLoggerFactory(),
		NATType: &vnet.NATType{
			Mode: vnet.NATModeNAT1To1,
		},
	})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to create leftRouter: %w", err)
	}

	// TODO(mathis): replace TBF by more general Traffic Controller which does
	// rate limitting, min delay, jitter, packet loss
	leftTBF, err := vnet.NewTokenBucketFilter(leftRouter)
	if err != nil {
		return nil, nil, nil, err
	}
	if err = wan.AddNet(leftTBF); err != nil {
		return nil, nil, nil, err
	}
	if err = wan.AddChildRouter(leftRouter); err != nil {
		return nil, nil, nil, err
	}

	rightRouter, err := vnet.NewRouter(&vnet.RouterConfig{
		CIDR:          right.cidr,
		StaticIPs:     right.staticIPs,
		LoggerFactory: logging.NewDefaultLoggerFactory(),
		NATType: &vnet.NATType{
			Mode: vnet.NATModeNAT1To1,
		},
	})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to create rightRouter: %w", err)
	}

	// TODO(mathis): replace TBF by more general Traffic Controller which does
	// rate limitting, min delay, jitter, packet loss
	rightTBF, err := vnet.NewTokenBucketFilter(rightRouter)
	if err != nil {
		return nil, nil, nil, err
	}

	// rightLossFilter, err := vnet.NewLossFilter(rightTBF, 30)
	// if err != nil {
	// 	return nil, nil, nil, err
	// }

	if err = wan.AddNet(rightTBF); err != nil {
		return nil, nil, nil, err
	}
	if err = wan.AddChildRouter(rightRouter); err != nil {
		return nil, nil, nil, err
	}

	if err = wan.Start(); err != nil {
		return nil, nil, nil, err
	}

	return &router{
			Router: leftRouter,
			tbf:    leftTBF,
		}, &router{
			Router: rightRouter,
			tbf:    rightTBF,
		},
		wan,
		nil
}

func createPeers(ctx context.Context, pair *SenderReceiverPair, routerA, routerB *router) ([]func(), error) {
	log := logging.NewDefaultLoggerFactory().NewLogger("test")

	sendNet := vnet.NewNet(&vnet.NetConfig{
		StaticIPs: []string{pair.senderPrivateIP},
	})

	// TODO(mathis): Create Delay network conditioner
	// if pair.oneWayPropagationDelay != 0 {
	// }

	if err := routerA.AddNet(sendNet); err != nil {
		return nil, fmt.Errorf("failed to add sendNet to routerA: %w", err)
	}

	receiveNet := vnet.NewNet(&vnet.NetConfig{
		StaticIPs: []string{pair.receiverPrivateIP},
	})
	if err := routerB.AddNet(receiveNet); err != nil {
		return nil, fmt.Errorf("failed to add receiveNet to routerB: %w", err)
	}

	offerSettingEngine := SettingEngine{}
	offerSettingEngine.SetVNet(sendNet)
	offerSettingEngine.SetICETimeouts(time.Second, time.Second, 200*time.Millisecond)
	offerSettingEngine.SetNAT1To1IPs([]string{pair.senderPublicIP}, ICECandidateTypeHost)

	offerMediaEngine := &MediaEngine{}
	if err := offerMediaEngine.RegisterDefaultCodecs(); err != nil {
		return nil, err
	}

	offerDumperInterceptor, err := packetdump.NewSenderInterceptor(
		// packetdump.SenderRTPFormatter(rtpFormat),
		// packetdump.SenderRTCPFormatter(rtcpFormat),
		packetdump.SenderRTPWriter(io.Discard),
		packetdump.SenderRTCPWriter(io.Discard),
	)
	if err != nil {
		return nil, err
	}
	offerInterceptorRegistry := &interceptor.Registry{}
	offerInterceptorRegistry.Add(offerDumperInterceptor)

	offerPeerConnection, err := NewAPI(
		WithSettingEngine(offerSettingEngine),
		WithMediaEngine(offerMediaEngine),
		WithInterceptorRegistry(offerInterceptorRegistry),
	).NewPeerConnection(Configuration{})
	if err != nil {
		return nil, err
	}

	answerSettingEngine := SettingEngine{}
	answerSettingEngine.SetVNet(receiveNet)
	answerSettingEngine.SetICETimeouts(time.Second, time.Second, 200*time.Millisecond)
	answerSettingEngine.SetNAT1To1IPs([]string{pair.receiverPublicIP}, ICECandidateTypeHost)

	answerMediaEngine := &MediaEngine{}
	if err = answerMediaEngine.RegisterDefaultCodecs(); err != nil {
		return nil, err
	}

	answerDumperInterceptor, err := packetdump.NewReceiverInterceptor(
		packetdump.ReceiverRTPFormatter(rtpFormat),
		// packetdump.ReceiverRTCPFormatter(rtcpFormat),
		packetdump.ReceiverRTCPWriter(io.Discard),
		packetdump.ReceiverRTPWriter(io.Discard),
	)
	if err != nil {
		return nil, err
	}
	answerInterceptorRegistry := &interceptor.Registry{}
	answerInterceptorRegistry.Add(answerDumperInterceptor)

	answerPeerConnection, err := NewAPI(
		WithSettingEngine(answerSettingEngine),
		WithMediaEngine(answerMediaEngine),
		WithInterceptorRegistry(answerInterceptorRegistry),
	).NewPeerConnection(Configuration{})
	if err != nil {
		return nil, err
	}

	pair.sender = &mediaSender{
		pc:  offerPeerConnection,
		log: log,
	}

	startTrackFunctions := []func(){}
	for _, t := range pair.tracks {
		track, err := NewTrackLocalStaticSample(t.capability, t.id, t.streamID)
		if err != nil {
			return nil, err
		}
		if _, err := offerPeerConnection.AddTrack(track); err != nil {
			return nil, err
		}
		startFn := func(wait time.Duration, codec syntheticCodec) func() {
			return func() {
				time.AfterFunc(wait, func() {
					if err := pair.sender.start(ctx, track, codec); err != nil {
						log.Errorf("failed to start sender: %v", err)
					}
				})
			}
		}(t.startAfter, t.codec)
		startTrackFunctions = append(startTrackFunctions, startFn)
	}

	pair.receiver = &mediaReceiver{
		pc:  answerPeerConnection,
		log: log,
	}
	answerPeerConnection.OnTrack(pair.receiver.onTrack)

	peerConnectionConnected := untilConnectionState(PeerConnectionStateConnected, offerPeerConnection, answerPeerConnection)

	if err := signalPair(offerPeerConnection, answerPeerConnection); err != nil {
		return nil, err
	}

	peerConnectionConnected.Wait()

	return startTrackFunctions, nil
}

func rtpFormat(pkt *rtp.Packet, attributes interceptor.Attributes) string {
	return fmt.Sprintf("%v, %v, %v, %v, %v, %v, %v\n",
		0, // TODO: Get send/receive time from attributes
		pkt.PayloadType,
		pkt.SSRC,
		pkt.SequenceNumber,
		pkt.Timestamp,
		pkt.Marker,
		pkt.MarshalSize(),
	)
}

type bandwidthVariationPhase struct {
	duration      time.Duration
	capacityRatio float64
}

type trackConfig struct {
	capability RTPCodecCapability
	id         string
	streamID   string
	codec      syntheticCodec
	startAfter time.Duration
}

type SenderReceiverPair struct {
	senderPublicIP    string
	senderPrivateIP   string
	receiverPublicIP  string
	receiverPrivateIP string

	tracks []trackConfig

	sender   *mediaSender
	receiver *mediaReceiver
}

type testcase struct {
	name              string
	referenceCapacity int64
	totalDuration     time.Duration
	left              routerConfig
	right             routerConfig
	forward           []SenderReceiverPair
	backward          []SenderReceiverPair
	forwardPhases     []bandwidthVariationPhase
	backwardPhases    []bandwidthVariationPhase
}

var testCases = []testcase{
	{
		name:              "TestVariableAvailableCapacitySingleFlow",
		referenceCapacity: defaultReferenceCapacity,
		totalDuration:     100 * time.Second,
		left: routerConfig{
			cidr: leftCIDR,
			staticIPs: []string{
				fmt.Sprintf("%v/%v", leftPublicIP1, leftPrivateIP1),
			},
		},
		right: routerConfig{
			cidr: rightCIDR,
			staticIPs: []string{
				fmt.Sprintf("%v/%v", rightPublicIP1, rightPrivateIP1),
			},
		},
		forward: []SenderReceiverPair{
			{
				senderPublicIP:    leftPublicIP1,
				senderPrivateIP:   leftPrivateIP1,
				receiverPublicIP:  rightPublicIP1,
				receiverPrivateIP: rightPrivateIP1,
				tracks:            []trackConfig{defaultVideotrack, defaultAudioTrack},
			},
		},
		forwardPhases: []bandwidthVariationPhase{
			{
				duration:      40 * time.Second,
				capacityRatio: 1,
			},
			{
				duration:      20 * time.Second,
				capacityRatio: 2.5,
			},
			{
				duration:      20 * time.Second,
				capacityRatio: 0.6,
			},
			{
				duration:      20 * time.Second,
				capacityRatio: 1.0,
			},
		},
	},
	{
		name:              "TestVariableAvailableCapacityMultipleFlow",
		referenceCapacity: defaultReferenceCapacity,
		totalDuration:     125 * time.Second,
		left: routerConfig{
			cidr: leftCIDR,
			staticIPs: []string{
				fmt.Sprintf("%v/%v", leftPublicIP1, leftPrivateIP1),
				fmt.Sprintf("%v/%v", leftPublicIP2, leftPrivateIP2),
			},
		},
		right: routerConfig{
			cidr: rightCIDR,
			staticIPs: []string{
				fmt.Sprintf("%v/%v", rightPublicIP1, rightPrivateIP1),
				fmt.Sprintf("%v/%v", rightPublicIP2, rightPrivateIP2),
			},
		},
		forward: []SenderReceiverPair{
			{
				senderPublicIP:    leftPublicIP1,
				senderPrivateIP:   leftPrivateIP1,
				receiverPublicIP:  rightPublicIP1,
				receiverPrivateIP: rightPrivateIP1,
				tracks:            []trackConfig{defaultVideotrack, defaultAudioTrack},
			},
			{
				senderPublicIP:    leftPublicIP2,
				senderPrivateIP:   leftPrivateIP2,
				receiverPublicIP:  rightPublicIP2,
				receiverPrivateIP: rightPrivateIP2,
				tracks:            []trackConfig{defaultVideotrack, defaultAudioTrack},
			},
		},
		forwardPhases: []bandwidthVariationPhase{
			{
				duration:      25 * time.Second,
				capacityRatio: 2.0,
			},
			{
				duration:      25 * time.Second,
				capacityRatio: 1.0,
			},
			{
				duration:      25 * time.Second,
				capacityRatio: 1.75,
			},
			{
				duration:      25 * time.Second,
				capacityRatio: 0.5,
			},
			{
				duration:      25 * time.Second,
				capacityRatio: 1.0,
			},
		},
	},
	{
		name:              "TestCongestedFeedbackLinkWithBiDirectionalMediaFlows",
		referenceCapacity: defaultReferenceCapacity,
		totalDuration:     100 * time.Second,
		left: routerConfig{
			cidr: leftCIDR,
			staticIPs: []string{
				fmt.Sprintf("%v/%v", leftPublicIP1, leftPrivateIP1),
				fmt.Sprintf("%v/%v", leftPublicIP2, leftPrivateIP2),
			},
		},
		right: routerConfig{
			cidr: rightCIDR,
			staticIPs: []string{
				fmt.Sprintf("%v/%v", rightPublicIP1, rightPrivateIP1),
				fmt.Sprintf("%v/%v", rightPublicIP2, rightPrivateIP2),
			},
		},
		forward: []SenderReceiverPair{
			{
				senderPublicIP:    leftPublicIP1,
				senderPrivateIP:   leftPrivateIP1,
				receiverPublicIP:  rightPublicIP1,
				receiverPrivateIP: rightPrivateIP1,
				tracks:            []trackConfig{defaultVideotrack, defaultAudioTrack},
			},
		},
		backward: []SenderReceiverPair{
			{
				senderPublicIP:    rightPublicIP2,
				senderPrivateIP:   rightPrivateIP2,
				receiverPublicIP:  leftPublicIP2,
				receiverPrivateIP: leftPrivateIP2,
				tracks:            []trackConfig{defaultVideotrack, defaultAudioTrack},
			},
		},
		forwardPhases: []bandwidthVariationPhase{
			{
				duration:      20 * time.Second,
				capacityRatio: 2.0,
			},
			{
				duration:      20 * time.Second,
				capacityRatio: 1.0,
			},
			{
				duration:      20 * time.Second,
				capacityRatio: 0.5,
			},
			{
				duration:      40 * time.Second,
				capacityRatio: 2.0,
			},
		},
		backwardPhases: []bandwidthVariationPhase{
			{
				duration:      35 * time.Second,
				capacityRatio: 2.0,
			},
			{
				duration:      35 * time.Second,
				capacityRatio: 0.8,
			},
			{
				duration:      30 * time.Second,
				capacityRatio: 2.0,
			},
		},
	},
	{
		// TODO(mathis): Update this testcase, with RTT values
		name:              "TestRoundTripTimeFairness",
		referenceCapacity: 4 * vnet.MBit,
		totalDuration:     300 * time.Second,
		left: routerConfig{
			cidr: leftCIDR,
			staticIPs: []string{
				fmt.Sprintf("%v/%v", leftPublicIP1, leftPrivateIP1),
				fmt.Sprintf("%v/%v", leftPublicIP2, leftPrivateIP2),
				fmt.Sprintf("%v/%v", leftPublicIP3, leftPrivateIP3),
			},
		},
		right: routerConfig{
			cidr: rightCIDR,
			staticIPs: []string{
				fmt.Sprintf("%v/%v", rightPublicIP1, rightPrivateIP1),
				fmt.Sprintf("%v/%v", rightPublicIP2, rightPrivateIP2),
				fmt.Sprintf("%v/%v", rightPublicIP3, rightPrivateIP3),
			},
		},
		forward: []SenderReceiverPair{
			{
				senderPublicIP:    leftPublicIP1,
				senderPrivateIP:   leftPrivateIP1,
				receiverPublicIP:  rightPublicIP1,
				receiverPrivateIP: rightPrivateIP1,
				tracks:            []trackConfig{defaultVideotrack, defaultAudioTrack},
			},
			{
				senderPublicIP:    leftPublicIP2,
				senderPrivateIP:   leftPrivateIP2,
				receiverPublicIP:  rightPublicIP2,
				receiverPrivateIP: rightPrivateIP2,
				tracks: []trackConfig{
					{
						capability: RTPCodecCapability{MimeType: MimeTypeVP8},
						id:         "video2",
						streamID:   "pion",
						codec:      newSimpleFPSBasedCodec(150 * vnet.KBit),
						startAfter: 20 * time.Second,
					},
					{
						capability: RTPCodecCapability{MimeType: MimeTypeOpus},
						id:         "audio2",
						streamID:   "pion",
						codec:      newSimpleFPSBasedCodec(20 * vnet.KBit),
						startAfter: 20 * time.Second,
					},
				},
			},
			{
				senderPublicIP:    leftPublicIP3,
				senderPrivateIP:   leftPrivateIP3,
				receiverPublicIP:  rightPublicIP3,
				receiverPrivateIP: rightPrivateIP3,
				tracks: []trackConfig{
					{
						capability: RTPCodecCapability{MimeType: MimeTypeVP8},
						id:         "video3",
						streamID:   "pion",
						codec:      newSimpleFPSBasedCodec(150 * vnet.KBit),
						startAfter: 40 * time.Second,
					},
					{
						capability: RTPCodecCapability{MimeType: MimeTypeOpus},
						id:         "audio3",
						streamID:   "pion",
						codec:      newSimpleFPSBasedCodec(20 * vnet.KBit),
						startAfter: 40 * time.Second,
					},
				},
			},
		},
	},
}
