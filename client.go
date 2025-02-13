/*
Package gortsplib is a RTSP 1.0 library for the Go programming language,
written for rtsp-simple-server.

Examples are available at https://github.com/aler9/gortsplib/tree/master/examples

*/
package gortsplib

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"

	"github.com/aler9/gortsplib/pkg/auth"
	"github.com/aler9/gortsplib/pkg/base"
	"github.com/aler9/gortsplib/pkg/headers"
	"github.com/aler9/gortsplib/pkg/liberrors"
	"github.com/aler9/gortsplib/pkg/ringbuffer"
	"github.com/aler9/gortsplib/pkg/rtcpreceiver"
	"github.com/aler9/gortsplib/pkg/rtcpsender"
	"github.com/aler9/gortsplib/pkg/rtpcleaner"
	"github.com/aler9/gortsplib/pkg/sdp"
	"github.com/aler9/gortsplib/pkg/url"
)

func isAnyPort(p int) bool {
	return p == 0 || p == 1
}

func findBaseURL(sd *sdp.SessionDescription, res *base.Response, u *url.URL) (*url.URL, error) {
	// use global control attribute
	if control, ok := sd.Attribute("control"); ok && control != "*" {
		ret, err := url.Parse(control)
		if err != nil {
			return nil, fmt.Errorf("invalid control attribute: '%v'", control)
		}

		// add credentials
		ret.User = u.User

		return ret, nil
	}

	// use Content-Base
	if cb, ok := res.Header["Content-Base"]; ok {
		if len(cb) != 1 {
			return nil, fmt.Errorf("invalid Content-Base: '%v'", cb)
		}

		ret, err := url.Parse(cb[0])
		if err != nil {
			return nil, fmt.Errorf("invalid Content-Base: '%v'", cb)
		}

		// add credentials
		ret.User = u.User

		return ret, nil
	}

	// use URL of request
	return u, nil
}

type clientState int

const (
	clientStateInitial clientState = iota
	clientStatePrePlay
	clientStatePlay
	clientStatePreRecord
	clientStateRecord
)

type clientTrack struct {
	id              int
	track           Track
	tcpChannel      int
	udpRTPListener  *clientUDPListener
	udpRTCPListener *clientUDPListener

	// play
	udpRTPPacketBuffer *rtpPacketMultiBuffer
	udpRTCPReceiver    *rtcpreceiver.RTCPReceiver
	cleaner            *rtpcleaner.Cleaner

	// record
	udpRTCPSender *rtcpsender.RTCPSender
}

func (s clientState) String() string {
	switch s {
	case clientStateInitial:
		return "initial"
	case clientStatePrePlay:
		return "prePlay"
	case clientStatePlay:
		return "play"
	case clientStatePreRecord:
		return "preRecord"
	case clientStateRecord:
		return "record"
	}
	return "unknown"
}

type optionsReq struct {
	url *url.URL
	res chan clientRes
}

type describeReq struct {
	url *url.URL
	res chan clientRes
}

type announceReq struct {
	url    *url.URL
	tracks Tracks
	res    chan clientRes
}

type setupReq struct {
	forPlay  bool
	track    Track
	baseURL  *url.URL
	rtpPort  int
	rtcpPort int
	res      chan clientRes
}

type playReq struct {
	ra  *headers.Range
	res chan clientRes
}

type recordReq struct {
	res chan clientRes
}

type pauseReq struct {
	res chan clientRes
}

type clientRes struct {
	tracks  Tracks
	baseURL *url.URL
	res     *base.Response
	err     error
}

// ClientOnPacketRTPCtx is the context of a RTP packet.
type ClientOnPacketRTPCtx struct {
	TrackID      int
	Packet       *rtp.Packet
	PTSEqualsDTS bool
	H264NALUs    [][]byte
	H264PTS      time.Duration
}

// ClientOnPacketRTCPCtx is the context of a RTCP packet.
type ClientOnPacketRTCPCtx struct {
	TrackID int
	Packet  rtcp.Packet
}

// Client is a RTSP client.
type Client struct {
	//
	// callbacks
	//
	// called before every request.
	OnRequest func(*base.Request)
	// called after every response.
	OnResponse func(*base.Response)
	// called when a RTP packet arrives.
	OnPacketRTP func(*ClientOnPacketRTPCtx)
	// called when a RTCP packet arrives.
	OnPacketRTCP func(*ClientOnPacketRTCPCtx)

	//
	// RTSP parameters
	//
	// timeout of read operations.
	// It defaults to 10 seconds.
	ReadTimeout time.Duration
	// timeout of write operations.
	// It defaults to 10 seconds.
	WriteTimeout time.Duration
	// a TLS configuration to connect to TLS (RTSPS) servers.
	// It defaults to nil.
	TLSConfig *tls.Config
	// disable being redirected to other servers, that can happen during Describe().
	// It defaults to false.
	RedirectDisable bool
	// enable communication with servers which don't provide server ports or use
	// different server ports than the ones announced.
	// This can be a security issue.
	// It defaults to false.
	AnyPortEnable bool
	// the stream transport (UDP, Multicast or TCP).
	// If nil, it is chosen automatically (first UDP, then, if it fails, TCP).
	// It defaults to nil.
	Transport *Transport
	// If the client is reading with UDP, it must receive
	// at least a packet within this timeout.
	// It defaults to 3 seconds.
	InitialUDPReadTimeout time.Duration
	// read buffer count.
	// If greater than 1, allows to pass buffers to routines different than the one
	// that is reading frames.
	// It defaults to 256.
	ReadBufferCount int
	// write buffer count.
	// It allows to queue packets before sending them.
	// It defaults to 256.
	WriteBufferCount int
	// user agent header
	// It defaults to "gortsplib"
	UserAgent string

	//
	// system functions
	//
	// function used to initialize the TCP client.
	// It defaults to (&net.Dialer{}).DialContext.
	DialContext func(ctx context.Context, network, address string) (net.Conn, error)
	// function used to initialize UDP listeners.
	// It defaults to net.ListenPacket.
	ListenPacket func(network, address string) (net.PacketConn, error)

	//
	// private
	//

	udpSenderReportPeriod   time.Duration
	udpReceiverReportPeriod time.Duration
	checkStreamPeriod       time.Duration
	keepalivePeriod         time.Duration

	scheme             string
	host               string
	ctx                context.Context
	ctxCancel          func()
	state              clientState
	conn               net.Conn
	br                 *bufio.Reader
	session            string
	sender             *auth.Sender
	cseq               int
	optionsSent        bool
	useGetParameter    bool
	lastDescribeURL    *url.URL
	baseURL            *url.URL
	effectiveTransport *Transport
	tracks             []*clientTrack
	tcpTracksByChannel map[int]*clientTrack
	lastRange          *headers.Range
	writeMutex         sync.RWMutex // publish
	writeFrameAllowed  bool         // publish
	checkStreamTimer   *time.Timer
	checkStreamInitial bool
	tcpLastFrameTime   *int64
	keepaliveTimer     *time.Timer
	closeError         error
	writerRunning      bool
	writeBuffer        *ringbuffer.RingBuffer

	// connCloser channels
	connCloserTerminate chan struct{}
	connCloserDone      chan struct{}

	// reader channels
	readerErr chan error

	// writer channels
	writerDone chan struct{}

	// in
	options  chan optionsReq
	describe chan describeReq
	announce chan announceReq
	setup    chan setupReq
	play     chan playReq
	record   chan recordReq
	pause    chan pauseReq

	// out
	done chan struct{}
}

// Start initializes the connection to a server.
func (c *Client) Start(scheme string, host string) error {
	// callbacks
	if c.OnPacketRTP == nil {
		c.OnPacketRTP = func(ctx *ClientOnPacketRTPCtx) {
		}
	}
	if c.OnPacketRTCP == nil {
		c.OnPacketRTCP = func(ctx *ClientOnPacketRTCPCtx) {
		}
	}

	// RTSP parameters
	if c.ReadTimeout == 0 {
		c.ReadTimeout = 10 * time.Second
	}
	if c.WriteTimeout == 0 {
		c.WriteTimeout = 10 * time.Second
	}
	if c.InitialUDPReadTimeout == 0 {
		c.InitialUDPReadTimeout = 3 * time.Second
	}
	if c.ReadBufferCount == 0 {
		c.ReadBufferCount = 256
	}
	if c.WriteBufferCount == 0 {
		c.WriteBufferCount = 256
	}
	if c.UserAgent == "" {
		c.UserAgent = "gortsplib"
	}

	// system functions
	if c.DialContext == nil {
		c.DialContext = (&net.Dialer{}).DialContext
	}
	if c.ListenPacket == nil {
		c.ListenPacket = net.ListenPacket
	}

	// private
	if c.udpSenderReportPeriod == 0 {
		c.udpSenderReportPeriod = 10 * time.Second
	}
	if c.udpReceiverReportPeriod == 0 {
		c.udpReceiverReportPeriod = 10 * time.Second
	}
	if c.checkStreamPeriod == 0 {
		c.checkStreamPeriod = 1 * time.Second
	}
	if c.keepalivePeriod == 0 {
		c.keepalivePeriod = 30 * time.Second
	}

	ctx, ctxCancel := context.WithCancel(context.Background())

	c.scheme = scheme
	c.host = host
	c.ctx = ctx
	c.ctxCancel = ctxCancel
	c.checkStreamTimer = emptyTimer()
	c.keepaliveTimer = emptyTimer()
	c.options = make(chan optionsReq)
	c.describe = make(chan describeReq)
	c.announce = make(chan announceReq)
	c.setup = make(chan setupReq)
	c.play = make(chan playReq)
	c.record = make(chan recordReq)
	c.pause = make(chan pauseReq)
	c.done = make(chan struct{})

	go c.run()

	return nil
}

// StartReading connects to the address and starts reading all tracks.
func (c *Client) StartReading(address string) error {
	u, err := url.Parse(address)
	if err != nil {
		return err
	}

	err = c.Start(u.Scheme, u.Host)
	if err != nil {
		return err
	}

	tracks, baseURL, _, err := c.Describe(u)
	if err != nil {
		c.Close()
		return err
	}

	return c.SetupAndPlay(tracks, baseURL)
}

// StartReadingAndWait connects to the address, starts reading all tracks and waits
// until a read error.
func (c *Client) StartReadingAndWait(address string) error {
	err := c.StartReading(address)
	if err != nil {
		return err
	}

	return c.Wait()
}

// StartPublishing connects to the address and starts publishing the tracks.
func (c *Client) StartPublishing(address string, tracks Tracks) error {
	u, err := url.Parse(address)
	if err != nil {
		return err
	}

	err = c.Start(u.Scheme, u.Host)
	if err != nil {
		return err
	}

	// control attribute of tracks is overridden by Announce().
	// use a copy in order not to mess the client-read-republish example.
	tracks = tracks.clone()

	_, err = c.Announce(u, tracks)
	if err != nil {
		c.Close()
		return err
	}

	for _, track := range tracks {
		_, err := c.Setup(false, track, u, 0, 0)
		if err != nil {
			c.Close()
			return err
		}
	}

	_, err = c.Record()
	if err != nil {
		c.Close()
		return err
	}

	return nil
}

// Close closes all client resources and waits for them to close.
func (c *Client) Close() error {
	c.ctxCancel()
	<-c.done
	return c.closeError
}

// Wait waits until all client resources are closed.
// This can happen when a fatal error occurs or when Close() is called.
func (c *Client) Wait() error {
	<-c.done
	return c.closeError
}

// Tracks returns all the tracks that the client is reading or publishing.
func (c *Client) Tracks() Tracks {
	ret := make(Tracks, len(c.tracks))
	for i, track := range c.tracks {
		ret[i] = track.track
	}
	return ret
}

func (c *Client) run() {
	defer close(c.done)

	c.closeError = c.runInner()

	c.ctxCancel()

	c.doClose()
}

func (c *Client) runInner() error {
	for {
		select {
		case req := <-c.options:
			res, err := c.doOptions(req.url)
			req.res <- clientRes{res: res, err: err}

		case req := <-c.describe:
			tracks, baseURL, res, err := c.doDescribe(req.url)
			req.res <- clientRes{tracks: tracks, baseURL: baseURL, res: res, err: err}

		case req := <-c.announce:
			res, err := c.doAnnounce(req.url, req.tracks)
			req.res <- clientRes{res: res, err: err}

		case req := <-c.setup:
			res, err := c.doSetup(req.forPlay, req.track, req.baseURL, req.rtpPort, req.rtcpPort)
			req.res <- clientRes{res: res, err: err}

		case req := <-c.play:
			res, err := c.doPlay(req.ra, false)
			req.res <- clientRes{res: res, err: err}

		case req := <-c.record:
			res, err := c.doRecord()
			req.res <- clientRes{res: res, err: err}

		case req := <-c.pause:
			res, err := c.doPause()
			req.res <- clientRes{res: res, err: err}

		case <-c.checkStreamTimer.C:
			if *c.effectiveTransport == TransportUDP ||
				*c.effectiveTransport == TransportUDPMulticast {
				if c.checkStreamInitial {
					c.checkStreamInitial = false

					// check that at least one packet has been received
					inTimeout := func() bool {
						for _, ct := range c.tracks {
							lft := atomic.LoadInt64(ct.udpRTPListener.lastPacketTime)
							if lft != 0 {
								return false
							}

							lft = atomic.LoadInt64(ct.udpRTCPListener.lastPacketTime)
							if lft != 0 {
								return false
							}
						}
						return true
					}()
					if inTimeout {
						err := c.trySwitchingProtocol()
						if err != nil {
							return err
						}
					}
				} else {
					inTimeout := func() bool {
						now := time.Now()
						for _, ct := range c.tracks {
							lft := time.Unix(atomic.LoadInt64(ct.udpRTPListener.lastPacketTime), 0)
							if now.Sub(lft) < c.ReadTimeout {
								return false
							}

							lft = time.Unix(atomic.LoadInt64(ct.udpRTCPListener.lastPacketTime), 0)
							if now.Sub(lft) < c.ReadTimeout {
								return false
							}
						}
						return true
					}()
					if inTimeout {
						return liberrors.ErrClientUDPTimeout{}
					}
				}
			} else { // TCP
				inTimeout := func() bool {
					now := time.Now()
					lft := time.Unix(atomic.LoadInt64(c.tcpLastFrameTime), 0)
					return now.Sub(lft) >= c.ReadTimeout
				}()
				if inTimeout {
					return liberrors.ErrClientTCPTimeout{}
				}
			}

			c.checkStreamTimer = time.NewTimer(c.checkStreamPeriod)

		case <-c.keepaliveTimer.C:
			_, err := c.do(&base.Request{
				Method: func() base.Method {
					// the VLC integrated rtsp server requires GET_PARAMETER
					if c.useGetParameter {
						return base.GetParameter
					}
					return base.Options
				}(),
				// use the stream base URL, otherwise some cameras do not reply
				URL: c.baseURL,
			}, true, false)
			if err != nil {
				return err
			}

			c.keepaliveTimer = time.NewTimer(c.keepalivePeriod)

		case err := <-c.readerErr:
			c.readerErr = nil
			return err

		case <-c.ctx.Done():
			return liberrors.ErrClientTerminated{}
		}
	}
}

func (c *Client) doClose() {
	if c.state == clientStatePlay || c.state == clientStateRecord {
		c.playRecordStop(true)

		c.do(&base.Request{
			Method: base.Teardown,
			URL:    c.baseURL,
		}, true, false)

		c.conn.Close()
		c.conn = nil
	} else if c.conn != nil {
		c.connCloserStop()
		c.conn.Close()
		c.conn = nil
	}

	for _, track := range c.tracks {
		if track.udpRTPListener != nil {
			track.udpRTPListener.close()
			track.udpRTCPListener.close()
		}
	}
}

func (c *Client) reset() {
	c.doClose()

	c.state = clientStateInitial
	c.session = ""
	c.sender = nil
	c.cseq = 0
	c.optionsSent = false
	c.useGetParameter = false
	c.baseURL = nil
	c.effectiveTransport = nil
	c.tracks = nil
	c.tcpTracksByChannel = nil
}

func (c *Client) checkState(allowed map[clientState]struct{}) error {
	if _, ok := allowed[c.state]; ok {
		return nil
	}

	allowedList := make([]fmt.Stringer, len(allowed))
	i := 0
	for a := range allowed {
		allowedList[i] = a
		i++
	}

	return liberrors.ErrClientInvalidState{AllowedList: allowedList, State: c.state}
}

func (c *Client) trySwitchingProtocol() error {
	prevScheme := c.scheme
	prevHost := c.host
	prevBaseURL := c.baseURL
	oldUseGetParameter := c.useGetParameter
	prevTracks := c.tracks

	c.reset()

	v := TransportTCP
	c.effectiveTransport = &v
	c.useGetParameter = oldUseGetParameter
	c.scheme = prevScheme
	c.host = prevHost

	// some Hikvision cameras require a describe before a setup
	_, _, _, err := c.doDescribe(c.lastDescribeURL)
	if err != nil {
		return err
	}

	for _, track := range prevTracks {
		_, err := c.doSetup(true, track.track, prevBaseURL, 0, 0)
		if err != nil {
			return err
		}
	}

	_, err = c.doPlay(c.lastRange, true)
	if err != nil {
		return err
	}

	return nil
}

func (c *Client) playRecordStart() {
	// stop connCloser
	c.connCloserStop()

	// start writer
	if c.state == clientStatePlay {
		// when reading, writeBuffer is only used to send RTCP receiver reports,
		// that are much smaller than RTP packets and are sent at a fixed interval.
		// decrease RAM consumption by allocating less buffers.
		c.writeBuffer = ringbuffer.New(8)
	} else {
		c.writeBuffer = ringbuffer.New(uint64(c.WriteBufferCount))
	}
	c.writerRunning = true
	c.writerDone = make(chan struct{})
	go c.runWriter()

	// allow writing
	c.writeMutex.Lock()
	c.writeFrameAllowed = true
	c.writeMutex.Unlock()

	if c.state == clientStatePlay {
		for _, ct := range c.tracks {
			_, isH264 := ct.track.(*TrackH264)
			ct.cleaner = rtpcleaner.NewCleaner(isH264, *c.effectiveTransport == TransportTCP)
		}

		c.keepaliveTimer = time.NewTimer(c.keepalivePeriod)

		switch *c.effectiveTransport {
		case TransportUDP:
			for trackID, ct := range c.tracks {
				ctrackID := trackID
				ct.udpRTPPacketBuffer = newRTPPacketMultiBuffer(uint64(c.ReadBufferCount))
				ct.udpRTCPReceiver = rtcpreceiver.New(c.udpReceiverReportPeriod, nil,
					ct.track.ClockRate(), func(pkt rtcp.Packet) {
						c.WritePacketRTCP(ctrackID, pkt)
					})
			}

			c.checkStreamTimer = time.NewTimer(c.InitialUDPReadTimeout)
			c.checkStreamInitial = true

			for _, ct := range c.tracks {
				ct.udpRTPListener.start(true)
				ct.udpRTCPListener.start(true)
			}

		case TransportUDPMulticast:
			for trackID, ct := range c.tracks {
				ctrackID := trackID
				ct.udpRTPPacketBuffer = newRTPPacketMultiBuffer(uint64(c.ReadBufferCount))
				ct.udpRTCPReceiver = rtcpreceiver.New(c.udpReceiverReportPeriod, nil,
					ct.track.ClockRate(), func(pkt rtcp.Packet) {
						c.WritePacketRTCP(ctrackID, pkt)
					})
			}

			c.checkStreamTimer = time.NewTimer(c.checkStreamPeriod)

			for _, ct := range c.tracks {
				ct.udpRTPListener.start(true)
				ct.udpRTCPListener.start(true)
			}

		default: // TCP
			c.checkStreamTimer = time.NewTimer(c.checkStreamPeriod)
			v := time.Now().Unix()
			c.tcpLastFrameTime = &v
		}
	} else if *c.effectiveTransport == TransportUDP {
		for trackID, ct := range c.tracks {
			ctrackID := trackID

			ct.udpRTCPSender = rtcpsender.New(c.udpSenderReportPeriod,
				ct.track.ClockRate(), func(pkt rtcp.Packet) {
					c.WritePacketRTCP(ctrackID, pkt)
				})
		}

		for _, ct := range c.tracks {
			ct.udpRTPListener.start(false)
			ct.udpRTCPListener.start(false)
		}
	}

	// for some reason, SetReadDeadline() must always be called in the same
	// goroutine, otherwise Read() freezes.
	// therefore, we disable the deadline and perform a check with a ticker.
	c.conn.SetReadDeadline(time.Time{})

	// start reader
	c.readerErr = make(chan error)
	go c.runReader()
}

func (c *Client) runReader() {
	c.readerErr <- func() error {
		if *c.effectiveTransport == TransportUDP || *c.effectiveTransport == TransportUDPMulticast {
			for {
				var res base.Response
				err := res.Read(c.br)
				if err != nil {
					return err
				}
			}
		} else {
			var processFunc func(*clientTrack, bool, []byte) error

			if c.state == clientStatePlay {
				tcpRTPPacketBuffer := newRTPPacketMultiBuffer(uint64(c.ReadBufferCount))

				processFunc = func(track *clientTrack, isRTP bool, payload []byte) error {
					now := time.Now()
					atomic.StoreInt64(c.tcpLastFrameTime, now.Unix())

					if isRTP {
						pkt := tcpRTPPacketBuffer.next()
						err := pkt.Unmarshal(payload)
						if err != nil {
							return err
						}

						out, err := track.cleaner.Clear(pkt)
						if err != nil {
							return err
						}

						for _, entry := range out {
							c.OnPacketRTP(&ClientOnPacketRTPCtx{
								TrackID:      track.id,
								Packet:       entry.Packet,
								PTSEqualsDTS: entry.PTSEqualsDTS,
								H264NALUs:    entry.H264NALUs,
								H264PTS:      entry.H264PTS,
							})
						}
					} else {
						if len(payload) > maxPacketSize {
							return fmt.Errorf("payload size (%d) greater than maximum allowed (%d)",
								len(payload), maxPacketSize)
						}

						packets, err := rtcp.Unmarshal(payload)
						if err != nil {
							return err
						}

						for _, pkt := range packets {
							c.OnPacketRTCP(&ClientOnPacketRTCPCtx{
								TrackID: track.id,
								Packet:  pkt,
							})
						}
					}

					return nil
				}
			} else {
				processFunc = func(track *clientTrack, isRTP bool, payload []byte) error {
					if !isRTP {
						if len(payload) > maxPacketSize {
							return fmt.Errorf("payload size (%d) greater than maximum allowed (%d)",
								len(payload), maxPacketSize)
						}

						packets, err := rtcp.Unmarshal(payload)
						if err != nil {
							return err
						}

						for _, pkt := range packets {
							c.OnPacketRTCP(&ClientOnPacketRTCPCtx{
								TrackID: track.id,
								Packet:  pkt,
							})
						}
					}

					return nil
				}
			}

			var frame base.InterleavedFrame
			var res base.Response

			for {
				what, err := base.ReadInterleavedFrameOrResponse(&frame, tcpMaxFramePayloadSize, &res, c.br)
				if err != nil {
					return err
				}

				if _, ok := what.(*base.InterleavedFrame); ok {
					channel := frame.Channel
					isRTP := true
					if (channel % 2) != 0 {
						channel--
						isRTP = false
					}

					track, ok := c.tcpTracksByChannel[channel]
					if !ok {
						continue
					}

					err := processFunc(track, isRTP, frame.Payload)
					if err != nil {
						return err
					}
				}
			}
		}
	}()
}

func (c *Client) playRecordStop(isClosing bool) {
	// stop reader
	if c.readerErr != nil {
		c.conn.SetReadDeadline(time.Now())
		<-c.readerErr
	}

	// forbid writing
	c.writeMutex.Lock()
	c.writeFrameAllowed = false
	c.writeMutex.Unlock()

	if *c.effectiveTransport == TransportUDP ||
		*c.effectiveTransport == TransportUDPMulticast {
		for _, ct := range c.tracks {
			ct.udpRTPListener.stop()
			ct.udpRTCPListener.stop()
		}

		if c.state == clientStatePlay {
			for _, ct := range c.tracks {
				ct.udpRTPPacketBuffer = nil
				ct.udpRTCPReceiver.Close()
				ct.udpRTCPReceiver = nil
			}
		} else {
			for _, ct := range c.tracks {
				ct.udpRTCPSender.Close()
				ct.udpRTCPSender = nil
			}
		}
	}

	for _, ct := range c.tracks {
		ct.cleaner = nil
	}

	// stop timers
	c.checkStreamTimer = emptyTimer()
	c.keepaliveTimer = emptyTimer()

	// stop writer
	c.writeBuffer.Close()
	<-c.writerDone
	c.writerRunning = false
	c.writeBuffer = nil

	// start connCloser
	if !isClosing {
		c.connCloserStart()
	}
}

func (c *Client) connOpen() error {
	if c.scheme != "rtsp" && c.scheme != "rtsps" {
		return fmt.Errorf("unsupported scheme '%s'", c.scheme)
	}

	if c.scheme == "rtsps" && c.Transport != nil && *c.Transport != TransportTCP {
		return fmt.Errorf("RTSPS can be used only with TCP")
	}

	if !strings.Contains(c.host, ":") {
		c.host += ":554"
	}

	ctx, cancel := context.WithTimeout(c.ctx, c.ReadTimeout)
	defer cancel()

	nconn, err := c.DialContext(ctx, "tcp", c.host)
	if err != nil {
		return err
	}

	c.conn = func() net.Conn {
		if c.scheme == "rtsps" {
			tlsConfig := c.TLSConfig

			if tlsConfig == nil {
				tlsConfig = &tls.Config{}
			}

			host, _, _ := net.SplitHostPort(c.host)
			tlsConfig.ServerName = host

			return tls.Client(nconn, tlsConfig)
		}
		return nconn
	}()

	c.br = bufio.NewReaderSize(c.conn, tcpReadBufferSize)
	c.connCloserStart()
	return nil
}

func (c *Client) connCloserStart() {
	c.connCloserTerminate = make(chan struct{})
	c.connCloserDone = make(chan struct{})

	go func() {
		defer close(c.connCloserDone)

		select {
		case <-c.ctx.Done():
			c.conn.Close()

		case <-c.connCloserTerminate:
		}
	}()
}

func (c *Client) connCloserStop() {
	close(c.connCloserTerminate)
	<-c.connCloserDone
	c.connCloserDone = nil
}

func (c *Client) do(req *base.Request, skipResponse bool, allowFrames bool) (*base.Response, error) {
	if c.conn == nil {
		err := c.connOpen()
		if err != nil {
			return nil, err
		}
	}

	if !c.optionsSent && req.Method != base.Options {
		_, err := c.doOptions(req.URL)
		if err != nil {
			return nil, err
		}
	}

	if req.Header == nil {
		req.Header = make(base.Header)
	}

	if c.session != "" {
		req.Header["Session"] = base.HeaderValue{c.session}
	}

	c.cseq++
	req.Header["CSeq"] = base.HeaderValue{strconv.FormatInt(int64(c.cseq), 10)}

	req.Header["User-Agent"] = base.HeaderValue{c.UserAgent}

	if c.sender != nil {
		c.sender.AddAuthorization(req)
	}

	if c.OnRequest != nil {
		c.OnRequest(req)
	}

	byts, _ := req.Write()

	c.conn.SetWriteDeadline(time.Now().Add(c.WriteTimeout))
	_, err := c.conn.Write(byts)
	if err != nil {
		return nil, err
	}

	var res base.Response

	if !skipResponse {
		c.conn.SetReadDeadline(time.Now().Add(c.ReadTimeout))

		if allowFrames {
			// read the response and ignore interleaved frames in between;
			// interleaved frames are sent in two cases:
			// * when the server is v4lrtspserver, before the PLAY response
			// * when the stream is already playing
			err = res.ReadIgnoreFrames(tcpMaxFramePayloadSize, c.br)
			if err != nil {
				return nil, err
			}
		} else {
			err = res.Read(c.br)
			if err != nil {
				return nil, err
			}
		}

		if c.OnResponse != nil {
			c.OnResponse(&res)
		}

		// get session from response
		if v, ok := res.Header["Session"]; ok {
			var sx headers.Session
			err := sx.Read(v)
			if err != nil {
				return nil, liberrors.ErrClientSessionHeaderInvalid{Err: err}
			}
			c.session = sx.Session

			if sx.Timeout != nil && *sx.Timeout > 0 {
				c.keepalivePeriod = time.Duration(float64(*sx.Timeout)*0.8) * time.Second
			}
		}

		// if required, send request again with authentication
		if res.StatusCode == base.StatusUnauthorized && req.URL.User != nil && c.sender == nil {
			pass, _ := req.URL.User.Password()
			user := req.URL.User.Username()

			sender, err := auth.NewSender(res.Header["WWW-Authenticate"], user, pass)
			if err != nil {
				return nil, fmt.Errorf("unable to setup authentication: %s", err)
			}
			c.sender = sender

			return c.do(req, skipResponse, allowFrames)
		}
	}

	return &res, nil
}

func (c *Client) doOptions(u *url.URL) (*base.Response, error) {
	err := c.checkState(map[clientState]struct{}{
		clientStateInitial:   {},
		clientStatePrePlay:   {},
		clientStatePreRecord: {},
	})
	if err != nil {
		return nil, err
	}

	res, err := c.do(&base.Request{
		Method: base.Options,
		URL:    u,
	}, false, false)
	if err != nil {
		return nil, err
	}

	if res.StatusCode != base.StatusOK {
		// since this method is not implemented by every RTSP server,
		// return only if status code is not 404
		if res.StatusCode == base.StatusNotFound {
			return res, nil
		}
		return nil, liberrors.ErrClientBadStatusCode{Code: res.StatusCode, Message: res.StatusMessage}
	}

	c.optionsSent = true

	c.useGetParameter = func() bool {
		pub, ok := res.Header["Public"]
		if !ok || len(pub) != 1 {
			return false
		}

		for _, m := range strings.Split(pub[0], ",") {
			if base.Method(strings.Trim(m, " ")) == base.GetParameter {
				return true
			}
		}
		return false
	}()

	return res, nil
}

// Options writes an OPTIONS request and reads a response.
func (c *Client) Options(u *url.URL) (*base.Response, error) {
	cres := make(chan clientRes)
	select {
	case c.options <- optionsReq{url: u, res: cres}:
		res := <-cres
		return res.res, res.err

	case <-c.ctx.Done():
		return nil, liberrors.ErrClientTerminated{}
	}
}

func (c *Client) doDescribe(u *url.URL) (Tracks, *url.URL, *base.Response, error) {
	err := c.checkState(map[clientState]struct{}{
		clientStateInitial:   {},
		clientStatePrePlay:   {},
		clientStatePreRecord: {},
	})
	if err != nil {
		return nil, nil, nil, err
	}

	res, err := c.do(&base.Request{
		Method: base.Describe,
		URL:    u,
		Header: base.Header{
			"Accept": base.HeaderValue{"application/sdp"},
		},
	}, false, false)
	if err != nil {
		return nil, nil, nil, err
	}

	if res.StatusCode != base.StatusOK {
		// redirect
		if !c.RedirectDisable &&
			res.StatusCode >= base.StatusMovedPermanently &&
			res.StatusCode <= base.StatusUseProxy &&
			len(res.Header["Location"]) == 1 {
			c.reset()

			ru, err := url.Parse(res.Header["Location"][0])
			if err != nil {
				return nil, nil, nil, err
			}

			if u.User != nil {
				ru.User = u.User
			}

			c.scheme = ru.Scheme
			c.host = ru.Host

			return c.doDescribe(ru)
		}

		return nil, nil, res, liberrors.ErrClientBadStatusCode{Code: res.StatusCode, Message: res.StatusMessage}
	}

	ct, ok := res.Header["Content-Type"]
	if !ok || len(ct) != 1 {
		return nil, nil, nil, liberrors.ErrClientContentTypeMissing{}
	}

	// strip encoding information from Content-Type header
	ct = base.HeaderValue{strings.Split(ct[0], ";")[0]}

	if ct[0] != "application/sdp" {
		return nil, nil, nil, liberrors.ErrClientContentTypeUnsupported{CT: ct}
	}

	tracks, sd, err := ReadTracks(res.Body, true)
	if err != nil {
		return nil, nil, nil, err
	}

	baseURL, err := findBaseURL(sd, res, u)
	if err != nil {
		return nil, nil, nil, err
	}

	c.lastDescribeURL = u

	return tracks, baseURL, res, nil
}

// Describe writes a DESCRIBE request and reads a Response.
func (c *Client) Describe(u *url.URL) (Tracks, *url.URL, *base.Response, error) {
	cres := make(chan clientRes)
	select {
	case c.describe <- describeReq{url: u, res: cres}:
		res := <-cres
		return res.tracks, res.baseURL, res.res, res.err

	case <-c.ctx.Done():
		return nil, nil, nil, liberrors.ErrClientTerminated{}
	}
}

func (c *Client) doAnnounce(u *url.URL, tracks Tracks) (*base.Response, error) {
	err := c.checkState(map[clientState]struct{}{
		clientStateInitial: {},
	})
	if err != nil {
		return nil, err
	}

	tracks.setControls()

	res, err := c.do(&base.Request{
		Method: base.Announce,
		URL:    u,
		Header: base.Header{
			"Content-Type": base.HeaderValue{"application/sdp"},
		},
		Body: tracks.Write(false),
	}, false, false)
	if err != nil {
		return nil, err
	}

	if res.StatusCode != base.StatusOK {
		return nil, liberrors.ErrClientBadStatusCode{
			Code: res.StatusCode, Message: res.StatusMessage,
		}
	}

	c.baseURL = u.Clone()
	c.state = clientStatePreRecord

	return res, nil
}

// Announce writes an ANNOUNCE request and reads a Response.
func (c *Client) Announce(u *url.URL, tracks Tracks) (*base.Response, error) {
	cres := make(chan clientRes)
	select {
	case c.announce <- announceReq{url: u, tracks: tracks, res: cres}:
		res := <-cres
		return res.res, res.err

	case <-c.ctx.Done():
		return nil, liberrors.ErrClientTerminated{}
	}
}

func (c *Client) doSetup(
	forPlay bool,
	track Track,
	baseURL *url.URL,
	rtpPort int,
	rtcpPort int,
) (*base.Response, error) {
	err := c.checkState(map[clientState]struct{}{
		clientStateInitial:   {},
		clientStatePrePlay:   {},
		clientStatePreRecord: {},
	})
	if err != nil {
		return nil, err
	}

	if (!forPlay && c.state != clientStatePreRecord) ||
		(forPlay && c.state != clientStatePrePlay &&
			c.state != clientStateInitial) {
		return nil, liberrors.ErrClientCannotReadPublishAtSameTime{}
	}

	if c.baseURL != nil && *baseURL != *c.baseURL {
		return nil, liberrors.ErrClientCannotSetupTracksDifferentURLs{}
	}

	// always use TCP if encrypted
	if c.scheme == "rtsps" {
		v := TransportTCP
		c.effectiveTransport = &v
	}

	transport := func() Transport {
		// transport set by previous Setup() or trySwitchingProtocol()
		if c.effectiveTransport != nil {
			return *c.effectiveTransport
		}

		// transport set by conf
		if c.Transport != nil {
			return *c.Transport
		}

		// try UDP
		return TransportUDP
	}()

	mode := headers.TransportModePlay
	if !forPlay {
		mode = headers.TransportModeRecord
	}

	th := headers.Transport{
		Mode: &mode,
	}

	trackID := len(c.tracks)

	ct := &clientTrack{
		track: track,
	}

	switch transport {
	case TransportUDP:
		if (rtpPort == 0 && rtcpPort != 0) ||
			(rtpPort != 0 && rtcpPort == 0) {
			return nil, liberrors.ErrClientUDPPortsZero{}
		}

		if rtpPort != 0 && rtcpPort != (rtpPort+1) {
			return nil, liberrors.ErrClientUDPPortsNotConsecutive{}
		}

		if rtpPort != 0 {
			var err error
			ct.udpRTPListener, err = newClientUDPListener(
				c,
				false,
				":"+strconv.FormatInt(int64(rtpPort), 10),
				ct,
				true)
			if err != nil {
				return nil, err
			}

			ct.udpRTCPListener, err = newClientUDPListener(
				c,
				false,
				":"+strconv.FormatInt(int64(rtcpPort), 10),
				ct,
				true)
			if err != nil {
				ct.udpRTPListener.close()
				return nil, err
			}
		} else {
			ct.udpRTPListener, ct.udpRTCPListener = newClientUDPListenerPair(c, ct)
		}

		v1 := headers.TransportDeliveryUnicast
		th.Delivery = &v1
		th.Protocol = headers.TransportProtocolUDP
		th.ClientPorts = &[2]int{
			ct.udpRTPListener.port(),
			ct.udpRTCPListener.port(),
		}

	case TransportUDPMulticast:
		v1 := headers.TransportDeliveryMulticast
		th.Delivery = &v1
		th.Protocol = headers.TransportProtocolUDP

	case TransportTCP:
		v1 := headers.TransportDeliveryUnicast
		th.Delivery = &v1
		th.Protocol = headers.TransportProtocolTCP
		th.InterleavedIDs = &[2]int{(trackID * 2), (trackID * 2) + 1}
	}

	trackURL, err := track.url(baseURL)
	if err != nil {
		if transport == TransportUDP {
			ct.udpRTPListener.close()
			ct.udpRTCPListener.close()
		}
		return nil, err
	}

	res, err := c.do(&base.Request{
		Method: base.Setup,
		URL:    trackURL,
		Header: base.Header{
			"Transport": th.Write(),
		},
	}, false, false)
	if err != nil {
		if transport == TransportUDP {
			ct.udpRTPListener.close()
			ct.udpRTCPListener.close()
		}
		return nil, err
	}

	if res.StatusCode != base.StatusOK {
		if transport == TransportUDP {
			ct.udpRTPListener.close()
			ct.udpRTCPListener.close()
		}

		// switch transport automatically
		if res.StatusCode == base.StatusUnsupportedTransport &&
			c.effectiveTransport == nil &&
			c.Transport == nil {
			v := TransportTCP
			c.effectiveTransport = &v

			return c.doSetup(forPlay, track, baseURL, 0, 0)
		}

		return nil, liberrors.ErrClientBadStatusCode{Code: res.StatusCode, Message: res.StatusMessage}
	}

	var thRes headers.Transport
	err = thRes.Read(res.Header["Transport"])
	if err != nil {
		if transport == TransportUDP {
			ct.udpRTPListener.close()
			ct.udpRTCPListener.close()
		}
		return nil, liberrors.ErrClientTransportHeaderInvalid{Err: err}
	}

	switch transport {
	case TransportUDP:
		if thRes.Delivery != nil && *thRes.Delivery != headers.TransportDeliveryUnicast {
			return nil, liberrors.ErrClientTransportHeaderInvalidDelivery{}
		}

		if !forPlay || !c.AnyPortEnable {
			if thRes.ServerPorts == nil || isAnyPort(thRes.ServerPorts[0]) || isAnyPort(thRes.ServerPorts[1]) {
				ct.udpRTPListener.close()
				ct.udpRTCPListener.close()
				return nil, liberrors.ErrClientServerPortsNotProvided{}
			}
		}

		ct.udpRTPListener.readIP = func() net.IP {
			if thRes.Source != nil {
				return *thRes.Source
			}
			return c.conn.RemoteAddr().(*net.TCPAddr).IP
		}()
		if thRes.ServerPorts != nil {
			ct.udpRTPListener.readPort = thRes.ServerPorts[0]
			ct.udpRTPListener.writeAddr = &net.UDPAddr{
				IP:   c.conn.RemoteAddr().(*net.TCPAddr).IP,
				Zone: c.conn.RemoteAddr().(*net.TCPAddr).Zone,
				Port: thRes.ServerPorts[0],
			}
		}

		ct.udpRTCPListener.readIP = func() net.IP {
			if thRes.Source != nil {
				return *thRes.Source
			}
			return c.conn.RemoteAddr().(*net.TCPAddr).IP
		}()
		if thRes.ServerPorts != nil {
			ct.udpRTCPListener.readPort = thRes.ServerPorts[1]
			ct.udpRTCPListener.writeAddr = &net.UDPAddr{
				IP:   c.conn.RemoteAddr().(*net.TCPAddr).IP,
				Zone: c.conn.RemoteAddr().(*net.TCPAddr).Zone,
				Port: thRes.ServerPorts[1],
			}
		}

	case TransportUDPMulticast:
		if thRes.Delivery == nil || *thRes.Delivery != headers.TransportDeliveryMulticast {
			return nil, liberrors.ErrClientTransportHeaderInvalidDelivery{}
		}

		if thRes.Ports == nil {
			return nil, liberrors.ErrClientTransportHeaderNoPorts{}
		}

		if thRes.Destination == nil {
			return nil, liberrors.ErrClientTransportHeaderNoDestination{}
		}

		ct.udpRTPListener, err = newClientUDPListener(
			c,
			true,
			thRes.Destination.String()+":"+strconv.FormatInt(int64(thRes.Ports[0]), 10),
			ct,
			true)
		if err != nil {
			return nil, err
		}

		ct.udpRTCPListener, err = newClientUDPListener(
			c,
			true,
			thRes.Destination.String()+":"+strconv.FormatInt(int64(thRes.Ports[1]), 10),
			ct,
			false)
		if err != nil {
			ct.udpRTPListener.close()
			return nil, err
		}

		ct.udpRTPListener.readIP = c.conn.RemoteAddr().(*net.TCPAddr).IP
		ct.udpRTPListener.readPort = thRes.Ports[0]
		ct.udpRTPListener.writeAddr = &net.UDPAddr{
			IP:   *thRes.Destination,
			Port: thRes.Ports[0],
		}

		ct.udpRTCPListener.readIP = c.conn.RemoteAddr().(*net.TCPAddr).IP
		ct.udpRTCPListener.readPort = thRes.Ports[1]
		ct.udpRTCPListener.writeAddr = &net.UDPAddr{
			IP:   *thRes.Destination,
			Port: thRes.Ports[1],
		}

	case TransportTCP:
		if thRes.Delivery != nil && *thRes.Delivery != headers.TransportDeliveryUnicast {
			return nil, liberrors.ErrClientTransportHeaderInvalidDelivery{}
		}

		if thRes.InterleavedIDs == nil {
			return nil, liberrors.ErrClientTransportHeaderNoInterleavedIDs{}
		}

		if (thRes.InterleavedIDs[0]%2) != 0 ||
			(thRes.InterleavedIDs[0]+1) != thRes.InterleavedIDs[1] {
			return nil, liberrors.ErrClientTransportHeaderInvalidInterleavedIDs{}
		}

		if _, ok := c.tcpTracksByChannel[thRes.InterleavedIDs[0]]; ok {
			return &base.Response{
				StatusCode: base.StatusBadRequest,
			}, liberrors.ErrClientTransportHeaderInterleavedIDsAlreadyUsed{}
		}

		if c.tcpTracksByChannel == nil {
			c.tcpTracksByChannel = make(map[int]*clientTrack)
		}

		c.tcpTracksByChannel[thRes.InterleavedIDs[0]] = ct
		ct.tcpChannel = thRes.InterleavedIDs[0]
	}

	c.tracks = append(c.tracks, ct)
	ct.id = trackID

	c.baseURL = baseURL
	c.effectiveTransport = &transport

	if mode == headers.TransportModePlay {
		c.state = clientStatePrePlay
	} else {
		c.state = clientStatePreRecord
	}

	return res, nil
}

// Setup writes a SETUP request and reads a Response.
// rtpPort and rtcpPort are used only if transport is UDP.
// if rtpPort and rtcpPort are zero, they are chosen automatically.
func (c *Client) Setup(
	forPlay bool,
	track Track,
	baseURL *url.URL,
	rtpPort int,
	rtcpPort int,
) (*base.Response, error) {
	cres := make(chan clientRes)
	select {
	case c.setup <- setupReq{
		forPlay:  forPlay,
		track:    track,
		baseURL:  baseURL,
		rtpPort:  rtpPort,
		rtcpPort: rtcpPort,
		res:      cres,
	}:
		res := <-cres
		return res.res, res.err

	case <-c.ctx.Done():
		return nil, liberrors.ErrClientTerminated{}
	}
}

func (c *Client) doPlay(ra *headers.Range, isSwitchingProtocol bool) (*base.Response, error) {
	err := c.checkState(map[clientState]struct{}{
		clientStatePrePlay: {},
	})
	if err != nil {
		return nil, err
	}

	// open the firewall by sending packets to the counterpart.
	// do this before sending the request.
	if *c.effectiveTransport == TransportUDP || *c.effectiveTransport == TransportUDPMulticast {
		for _, ct := range c.tracks {
			byts, _ := (&rtp.Packet{Header: rtp.Header{Version: 2}}).Marshal()
			ct.udpRTPListener.write(byts)

			byts, _ = (&rtcp.ReceiverReport{}).Marshal()
			ct.udpRTCPListener.write(byts)
		}
	}

	// Range is mandatory in Parrot Streaming Server
	if ra == nil {
		ra = &headers.Range{
			Value: &headers.RangeNPT{
				Start: headers.RangeNPTTime(0),
			},
		}
	}

	res, err := c.do(&base.Request{
		Method: base.Play,
		URL:    c.baseURL,
		Header: base.Header{
			"Range": ra.Write(),
		},
	}, false, *c.effectiveTransport == TransportTCP)
	if err != nil {
		return nil, err
	}

	if res.StatusCode != base.StatusOK {
		return nil, liberrors.ErrClientBadStatusCode{
			Code: res.StatusCode, Message: res.StatusMessage,
		}
	}

	c.lastRange = ra
	c.state = clientStatePlay
	c.playRecordStart()

	return res, nil
}

// Play writes a PLAY request and reads a Response.
// This can be called only after Setup().
func (c *Client) Play(ra *headers.Range) (*base.Response, error) {
	cres := make(chan clientRes)
	select {
	case c.play <- playReq{ra: ra, res: cres}:
		res := <-cres
		return res.res, res.err

	case <-c.ctx.Done():
		return nil, liberrors.ErrClientTerminated{}
	}
}

// SetupAndPlay setups and play the given tracks.
func (c *Client) SetupAndPlay(tracks Tracks, baseURL *url.URL) error {
	for _, t := range tracks {
		_, err := c.Setup(true, t, baseURL, 0, 0)
		if err != nil {
			return err
		}
	}

	_, err := c.Play(nil)
	return err
}

func (c *Client) doRecord() (*base.Response, error) {
	err := c.checkState(map[clientState]struct{}{
		clientStatePreRecord: {},
	})
	if err != nil {
		return nil, err
	}

	res, err := c.do(&base.Request{
		Method: base.Record,
		URL:    c.baseURL,
	}, false, false)
	if err != nil {
		return nil, err
	}

	if res.StatusCode != base.StatusOK {
		return nil, liberrors.ErrClientBadStatusCode{
			Code: res.StatusCode, Message: res.StatusMessage,
		}
	}

	c.state = clientStateRecord
	c.playRecordStart()

	return nil, nil
}

// Record writes a RECORD request and reads a Response.
// This can be called only after Announce() and Setup().
func (c *Client) Record() (*base.Response, error) {
	cres := make(chan clientRes)
	select {
	case c.record <- recordReq{res: cres}:
		res := <-cres
		return res.res, res.err

	case <-c.ctx.Done():
		return nil, liberrors.ErrClientTerminated{}
	}
}

func (c *Client) doPause() (*base.Response, error) {
	err := c.checkState(map[clientState]struct{}{
		clientStatePlay:   {},
		clientStateRecord: {},
	})
	if err != nil {
		return nil, err
	}

	c.playRecordStop(false)

	// change state regardless of the response
	switch c.state {
	case clientStatePlay:
		c.state = clientStatePrePlay
	case clientStateRecord:
		c.state = clientStatePreRecord
	}

	res, err := c.do(&base.Request{
		Method: base.Pause,
		URL:    c.baseURL,
	}, false, *c.effectiveTransport == TransportTCP)
	if err != nil {
		return nil, err
	}

	if res.StatusCode != base.StatusOK {
		return nil, liberrors.ErrClientBadStatusCode{
			Code: res.StatusCode, Message: res.StatusMessage,
		}
	}

	return res, nil
}

// Pause writes a PAUSE request and reads a Response.
// This can be called only after Play() or Record().
func (c *Client) Pause() (*base.Response, error) {
	cres := make(chan clientRes)
	select {
	case c.pause <- pauseReq{res: cres}:
		res := <-cres
		return res.res, res.err

	case <-c.ctx.Done():
		return nil, liberrors.ErrClientTerminated{}
	}
}

// Seek asks the server to re-start the stream from a specific timestamp.
func (c *Client) Seek(ra *headers.Range) (*base.Response, error) {
	_, err := c.Pause()
	if err != nil {
		return nil, err
	}

	return c.Play(ra)
}

func (c *Client) runWriter() {
	defer close(c.writerDone)

	var writeFunc func(int, bool, []byte)

	switch *c.effectiveTransport {
	case TransportUDP, TransportUDPMulticast:
		writeFunc = func(trackID int, isRTP bool, payload []byte) {
			if isRTP {
				c.tracks[trackID].udpRTPListener.write(payload)
			} else {
				c.tracks[trackID].udpRTCPListener.write(payload)
			}
		}

	default: // TCP
		rtpFrames := make(map[int]*base.InterleavedFrame, len(c.tracks))
		rtcpFrames := make(map[int]*base.InterleavedFrame, len(c.tracks))

		for trackID, ct := range c.tracks {
			rtpFrames[trackID] = &base.InterleavedFrame{Channel: ct.tcpChannel}
			rtcpFrames[trackID] = &base.InterleavedFrame{Channel: ct.tcpChannel + 1}
		}

		buf := make([]byte, maxPacketSize+4)

		writeFunc = func(trackID int, isRTP bool, payload []byte) {
			if isRTP {
				f := rtpFrames[trackID]
				f.Payload = payload
				n, _ := f.WriteTo(buf)

				c.conn.SetWriteDeadline(time.Now().Add(c.WriteTimeout))
				c.conn.Write(buf[:n])
			} else {
				f := rtcpFrames[trackID]
				f.Payload = payload
				n, _ := f.WriteTo(buf)

				c.conn.SetWriteDeadline(time.Now().Add(c.WriteTimeout))
				c.conn.Write(buf[:n])
			}
		}
	}

	for {
		tmp, ok := c.writeBuffer.Pull()
		if !ok {
			return
		}
		data := tmp.(trackTypePayload)

		writeFunc(data.trackID, data.isRTP, data.payload)
	}
}

// WritePacketRTP writes a RTP packet.
func (c *Client) WritePacketRTP(trackID int, pkt *rtp.Packet, ptsEqualsDTS bool) error {
	c.writeMutex.RLock()
	defer c.writeMutex.RUnlock()

	if !c.writeFrameAllowed {
		select {
		case <-c.done:
			return c.closeError
		default:
			return nil
		}
	}

	byts := make([]byte, maxPacketSize)
	n, err := pkt.MarshalTo(byts)
	if err != nil {
		return err
	}
	byts = byts[:n]

	if c.tracks[trackID].udpRTCPSender != nil {
		c.tracks[trackID].udpRTCPSender.ProcessPacketRTP(time.Now(), pkt, ptsEqualsDTS)
	}

	c.writeBuffer.Push(trackTypePayload{
		trackID: trackID,
		isRTP:   true,
		payload: byts,
	})
	return nil
}

// WritePacketRTCP writes a RTCP packet.
func (c *Client) WritePacketRTCP(trackID int, pkt rtcp.Packet) error {
	c.writeMutex.RLock()
	defer c.writeMutex.RUnlock()

	if !c.writeFrameAllowed {
		select {
		case <-c.done:
			return c.closeError
		default:
			return nil
		}
	}

	byts, err := pkt.Marshal()
	if err != nil {
		return err
	}

	c.writeBuffer.Push(trackTypePayload{
		trackID: trackID,
		isRTP:   false,
		payload: byts,
	})
	return nil
}
