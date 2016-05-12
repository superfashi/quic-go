package quic

import (
	"bytes"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/lucas-clemente/quic-go/ackhandler"
	"github.com/lucas-clemente/quic-go/congestion"
	"github.com/lucas-clemente/quic-go/errorcodes"
	"github.com/lucas-clemente/quic-go/frames"
	"github.com/lucas-clemente/quic-go/handshake"
	"github.com/lucas-clemente/quic-go/protocol"
	"github.com/lucas-clemente/quic-go/utils"
)

type receivedPacket struct {
	remoteAddr   interface{}
	publicHeader *PublicHeader
	r            *bytes.Reader
}

var (
	errInvalidStreamID             = errors.New("STREAM_FRAME with invalid StreamID received")
	errReopeningStreamsNotAllowed  = errors.New("Reopening Streams not allowed")
	errRstStreamOnInvalidStream    = errors.New("RST_STREAM received for unknown stream")
	errWindowUpdateOnInvalidStream = errors.New("WINDOW_UPDATE received for unknown stream")
)

// StreamCallback gets a stream frame and returns a reply frame
type StreamCallback func(*Session, utils.Stream)

// CloseCallback is called when a session is closed
type CloseCallback func(id protocol.ConnectionID)

// A Session is a QUIC session
type Session struct {
	connectionID protocol.ConnectionID

	streamCallback StreamCallback
	closeCallback  CloseCallback

	conn connection

	streams      map[protocol.StreamID]*stream
	streamsMutex sync.RWMutex

	sentPacketHandler     ackhandler.SentPacketHandler
	receivedPacketHandler ackhandler.ReceivedPacketHandler
	stopWaitingManager    ackhandler.StopWaitingManager

	unpacker *packetUnpacker
	packer   *packetPacker

	receivedPackets  chan receivedPacket
	sendingScheduled chan struct{}
	closeChan        chan struct{}
	closed           bool

	connectionParametersManager *handshake.ConnectionParametersManager

	// Used to calculate the next packet number from the truncated wire
	// representation, and sent back in public reset packets
	lastRcvdPacketNumber protocol.PacketNumber

	rttStats   congestion.RTTStats
	congestion congestion.SendAlgorithm
}

// NewSession makes a new session
func NewSession(conn connection, v protocol.VersionNumber, connectionID protocol.ConnectionID, sCfg *handshake.ServerConfig, streamCallback StreamCallback, closeCallback CloseCallback) PacketHandler {
	stopWaitingManager := ackhandler.NewStopWaitingManager()
	session := &Session{
		connectionID:                connectionID,
		conn:                        conn,
		streamCallback:              streamCallback,
		closeCallback:               closeCallback,
		streams:                     make(map[protocol.StreamID]*stream),
		sentPacketHandler:           ackhandler.NewSentPacketHandler(stopWaitingManager),
		receivedPacketHandler:       ackhandler.NewReceivedPacketHandler(),
		stopWaitingManager:          stopWaitingManager,
		receivedPackets:             make(chan receivedPacket, 1000), // TODO: What if server receives many packets and connection is already closed?!
		closeChan:                   make(chan struct{}, 1),
		sendingScheduled:            make(chan struct{}, 1),
		rttStats:                    congestion.RTTStats{},
		connectionParametersManager: handshake.NewConnectionParamatersManager(),
	}

	cryptoStream, _ := session.NewStream(1)
	cryptoSetup := handshake.NewCryptoSetup(connectionID, v, sCfg, cryptoStream, session.connectionParametersManager)

	go func() {
		if err := cryptoSetup.HandleCryptoStream(); err != nil {
			session.Close(err, true)
		}
	}()

	session.packer = &packetPacker{
		aead: cryptoSetup,
		connectionParametersManager: session.connectionParametersManager,
		sentPacketHandler:           session.sentPacketHandler,
		connectionID:                connectionID,
		version:                     v,
	}
	session.unpacker = &packetUnpacker{aead: cryptoSetup, version: v}

	session.congestion = congestion.NewCubicSender(
		congestion.DefaultClock{},
		&session.rttStats,
		false, /* don't use reno since chromium doesn't (why?) */
		protocol.InitialCongestionWindow,
		protocol.DefaultMaxCongestionWindow,
	)

	return session
}

// Run the session main loop
func (s *Session) Run() {
	for {
		// Close immediately if requested
		select {
		case <-s.closeChan:
			return
		default:
		}

		var err error
		select {
		case <-s.closeChan:
			return
		case p := <-s.receivedPackets:
			err = s.handlePacket(p.remoteAddr, p.publicHeader, p.r)
			s.scheduleSending()
		case <-s.sendingScheduled:
			err = s.sendPacket()
		case <-time.After(s.connectionParametersManager.GetIdleConnectionStateLifetime()):
			s.Close(protocol.NewQuicError(errorcodes.QUIC_NETWORK_IDLE_TIMEOUT, "No recent network activity."), true)
		}

		if err != nil {
			switch err {
			// Can happen e.g. when packets thought missing arrive late
			case ackhandler.ErrDuplicateOrOutOfOrderAck:
			// Can happen when RST_STREAMs arrive early or late (?)
			case ackhandler.ErrMapAccess:
				s.Close(err, true) // TODO: sent correct error code here
			case errRstStreamOnInvalidStream:
				utils.Errorf("Ignoring error in session: %s", err.Error())
			default:
				s.Close(err, true)
			}
		}

		s.garbageCollectStreams()
	}
}

func (s *Session) handlePacket(remoteAddr interface{}, publicHeader *PublicHeader, r *bytes.Reader) error {
	// Calcualate packet number
	publicHeader.PacketNumber = protocol.InferPacketNumber(
		publicHeader.PacketNumberLen,
		s.lastRcvdPacketNumber,
		publicHeader.PacketNumber,
	)
	s.lastRcvdPacketNumber = publicHeader.PacketNumber
	utils.Debugf("<- Reading packet 0x%x (%d bytes) for connection %x", publicHeader.PacketNumber, r.Size(), publicHeader.ConnectionID)

	// TODO: Only do this after authenticating
	s.conn.setCurrentRemoteAddr(remoteAddr)

	packet, err := s.unpacker.Unpack(publicHeader.Raw, publicHeader, r)
	if err != nil {
		// TODO: We currently treat un-decryptable packets as lost. We should
		// instead save them to a queue and retry later.
		// See issue https://github.com/lucas-clemente/quic-go/issues/38
		if qErr, ok := err.(*protocol.QuicError); ok && qErr.ErrorCode == errorcodes.QUIC_DECRYPTION_FAILURE {
			utils.Infof("Discarding packet due to decryption failure.")
			return nil // Discard packet
		}
		return err
	}

	s.receivedPacketHandler.ReceivedPacket(publicHeader.PacketNumber, packet.entropyBit)

	for _, ff := range packet.frames {
		var err error
		switch frame := ff.(type) {
		case *frames.StreamFrame:
			utils.Debugf("\t<- &frames.StreamFrame{StreamID: %d, FinBit: %t, Offset: %d}", frame.StreamID, frame.FinBit, frame.Offset)
			err = s.handleStreamFrame(frame)
			// TODO: send RstStreamFrame
		case *frames.AckFrame:
			err = s.handleAckFrame(frame)
		case *frames.ConnectionCloseFrame:
			// ToDo: send right error in ConnectionClose frame
			utils.Debugf("\t<- %#v", frame)
			s.Close(nil, false)
		case *frames.StopWaitingFrame:
			utils.Debugf("\t<- %#v", frame)
			err = s.receivedPacketHandler.ReceivedStopWaiting(frame)
		case *frames.RstStreamFrame:
			err = s.handleRstStreamFrame(frame)
			utils.Debugf("\t<- %#v", frame)
		case *frames.WindowUpdateFrame:
			utils.Debugf("\t<- %#v", frame)
			err = s.handleWindowUpdateFrame(frame)
		case *frames.BlockedFrame:
			utils.Infof("BLOCKED frame received for connection %x stream %d", s.connectionID, frame.StreamID)
		case *frames.PingFrame:
			utils.Debugf("\t<- %#v", frame)
		default:
			panic("unexpected frame type")
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// HandlePacket handles a packet
func (s *Session) HandlePacket(remoteAddr interface{}, publicHeader *PublicHeader, r *bytes.Reader) {
	s.receivedPackets <- receivedPacket{remoteAddr: remoteAddr, publicHeader: publicHeader, r: r}
}

// TODO: Ignore data for closed streams
func (s *Session) handleStreamFrame(frame *frames.StreamFrame) error {
	s.streamsMutex.RLock()
	str, streamExists := s.streams[frame.StreamID]
	s.streamsMutex.RUnlock()

	if !streamExists {
		if !s.isValidStreamID(frame.StreamID) {
			return errInvalidStreamID
		}

		ss, _ := s.NewStream(frame.StreamID)
		str = ss.(*stream)
	}
	if str == nil {
		return errReopeningStreamsNotAllowed
	}
	err := str.AddStreamFrame(frame)
	if err != nil {
		return err
	}
	if !streamExists {
		s.streamCallback(s, str)
	}
	return nil
}

func (s *Session) isValidStreamID(streamID protocol.StreamID) bool {
	if streamID%2 != 1 {
		return false
	}
	return true
}

func (s *Session) handleWindowUpdateFrame(frame *frames.WindowUpdateFrame) error {
	if frame.StreamID == 0 {
		// TODO: handle connection level WindowUpdateFrames
		// return errors.New("Connection level flow control not yet implemented")
		return nil
	}
	s.streamsMutex.RLock()
	stream, ok := s.streams[frame.StreamID]
	s.streamsMutex.RUnlock()

	if !ok {
		return errWindowUpdateOnInvalidStream
	}

	stream.UpdateFlowControlWindow(frame.ByteOffset)

	return nil
}

// TODO: Handle frame.byteOffset
func (s *Session) handleRstStreamFrame(frame *frames.RstStreamFrame) error {
	s.streamsMutex.RLock()
	str, streamExists := s.streams[frame.StreamID]
	s.streamsMutex.RUnlock()
	if !streamExists || str == nil {
		return errRstStreamOnInvalidStream
	}
	str.RegisterError(fmt.Errorf("RST_STREAM received with code %d", frame.ErrorCode))
	return nil
}

func (s *Session) handleAckFrame(frame *frames.AckFrame) error {
	duration, acked, lost, err := s.sentPacketHandler.ReceivedAck(frame)
	if err != nil {
		return err
	}

	// TODO: Don't always update RTT
	s.rttStats.UpdateRTT(duration, frame.DelayTime, time.Now())

	cAcked := make(congestion.PacketVector, len(acked))
	for i, v := range acked {
		cAcked[i].Number = v.PacketNumber
		cAcked[i].Length = v.Length
	}
	cLost := make(congestion.PacketVector, len(lost))
	for i, v := range lost {
		cLost[i].Number = v.PacketNumber
		cLost[i].Length = v.Length
	}
	s.congestion.OnCongestionEvent(
		true, /* rtt updated */
		s.sentPacketHandler.BytesInFlight(),
		cAcked,
		cLost,
	)

	utils.Debugf("\t<- %#v", frame)
	utils.Debugf("\tEstimated RTT: %dms", s.rttStats.SmoothedRTT()/time.Millisecond)
	return nil
}

// Close the connection
func (s *Session) Close(e error, sendConnectionClose bool) error {
	if s.closed {
		return nil
	}
	s.closed = true
	s.closeChan <- struct{}{}

	s.closeCallback(s.connectionID)

	if !sendConnectionClose {
		return nil
	}

	if e == nil {
		e = protocol.NewQuicError(errorcodes.QUIC_PEER_GOING_AWAY, "peer going away")
	}
	utils.Errorf("Closing session with error: %s", e.Error())
	errorCode := protocol.ErrorCode(1)
	reasonPhrase := e.Error()
	quicError, ok := e.(*protocol.QuicError)
	if ok {
		errorCode = quicError.ErrorCode
	}
	s.closeStreamsWithError(e)

	if errorCode == errorcodes.QUIC_DECRYPTION_FAILURE {
		return s.sendPublicReset(s.lastRcvdPacketNumber)
	}

	packet, err := s.packer.PackPacket(nil, []frames.Frame{
		&frames.ConnectionCloseFrame{ErrorCode: errorCode, ReasonPhrase: reasonPhrase},
	}, false)
	if err != nil {
		return err
	}
	if packet == nil {
		panic("Session: internal inconsistency: expected packet not to be nil")
	}
	return s.conn.write(packet.raw)
}

func (s *Session) closeStreamsWithError(err error) {
	s.streamsMutex.Lock()
	defer s.streamsMutex.Unlock()
	for _, s := range s.streams {
		if s == nil {
			continue
		}
		s.RegisterError(err)
	}
}

func (s *Session) sendPacket() error {
	if !s.congestionAllowsSending() {
		return nil
	}

	var controlFrames []frames.Frame

	// check for retransmissions first
	// TODO: handle multiple packets retransmissions
	retransmitPacket := s.sentPacketHandler.DequeuePacketForRetransmission()
	if retransmitPacket != nil {
		utils.Debugf("\tDequeueing retransmission for packet 0x%x", retransmitPacket.PacketNumber)
		s.stopWaitingManager.RegisterPacketForRetransmission(retransmitPacket)
		// resend the frames that were in the packet
		controlFrames = append(controlFrames, retransmitPacket.GetControlFramesForRetransmission()...)
		for _, streamFrame := range retransmitPacket.GetStreamFramesForRetransmission() {
			s.packer.AddHighPrioStreamFrame(*streamFrame)
		}
	}

	stopWaitingFrame := s.stopWaitingManager.GetStopWaitingFrame()

	ack, err := s.receivedPacketHandler.DequeueAckFrame()
	if err != nil {
		return err
	}
	if ack != nil {
		controlFrames = append(controlFrames, ack)
	}
	packet, err := s.packer.PackPacket(stopWaitingFrame, controlFrames, true)

	if err != nil {
		return err
	}
	if packet == nil {
		return nil
	}

	err = s.sentPacketHandler.SentPacket(&ackhandler.Packet{
		PacketNumber: packet.number,
		Frames:       packet.frames,
		EntropyBit:   packet.entropyBit,
		Length:       protocol.ByteCount(len(packet.raw)),
	})
	if err != nil {
		return err
	}

	s.congestion.OnPacketSent(
		time.Now(),
		s.sentPacketHandler.BytesInFlight(),
		packet.number,
		protocol.ByteCount(len(packet.raw)),
		true, /* TODO: is retransmittable */
	)

	s.stopWaitingManager.SentStopWaitingWithPacket(packet.number)

	utils.Debugf("-> Sending packet 0x%x (%d bytes)", packet.number, len(packet.raw))
	for _, frame := range packet.frames {
		if streamFrame, isStreamFrame := frame.(*frames.StreamFrame); isStreamFrame {
			utils.Debugf("\t-> &frames.StreamFrame{StreamID: %d, FinBit: %t, Offset: 0x%x, Data length: 0x%x, Offset + Data length: 0x%x}", streamFrame.StreamID, streamFrame.FinBit, streamFrame.Offset, len(streamFrame.Data), streamFrame.Offset+protocol.ByteCount(len(streamFrame.Data)))
		} else {
			utils.Debugf("\t-> %#v", frame)
		}
	}

	err = s.conn.write(packet.raw)
	if err != nil {
		return err
	}

	if !s.packer.Empty() {
		s.scheduleSending()
	}

	return nil
}

// QueueStreamFrame queues a frame for sending to the client
func (s *Session) QueueStreamFrame(frame *frames.StreamFrame) error {
	s.packer.AddStreamFrame(*frame)
	s.scheduleSending()
	return nil
}

// NewStream creates a new stream open for reading and writing
func (s *Session) NewStream(id protocol.StreamID) (utils.Stream, error) {
	s.streamsMutex.Lock()
	defer s.streamsMutex.Unlock()
	stream, err := newStream(s, s.connectionParametersManager, id)

	if err != nil {
		return nil, err
	}

	if s.streams[id] != nil {
		return nil, fmt.Errorf("Session: stream with ID %d already exists", id)
	}
	s.streams[id] = stream
	return stream, nil
}

// garbageCollectStreams goes through all streams and removes EOF'ed streams
// from the streams map.
func (s *Session) garbageCollectStreams() {
	s.streamsMutex.Lock()
	defer s.streamsMutex.Unlock()
	for k, v := range s.streams {
		if v == nil {
			continue
		}
		if v.finishedReading() {
			s.streams[k] = nil
		}
	}
}

func (s *Session) sendPublicReset(rejectedPacketNumber protocol.PacketNumber) error {
	utils.Infof("Sending public reset for connection %x, packet number %d", s.connectionID, rejectedPacketNumber)
	packet := &publicResetPacket{
		connectionID:         s.connectionID,
		rejectedPacketNumber: rejectedPacketNumber,
		nonceProof:           0, // TODO: Currently ignored by chrome.
	}
	var b bytes.Buffer
	packet.Write(&b)
	return s.conn.write(b.Bytes())
}

// scheduleSending signals that we have data for sending
func (s *Session) scheduleSending() {
	select {
	case s.sendingScheduled <- struct{}{}:
	default:
	}
}

func (s *Session) congestionAllowsSending() bool {
	return s.sentPacketHandler.BytesInFlight() <= s.congestion.GetCongestionWindow()
}
