/*
 * Copyright (c) 2014, Yawning Angel <yawning at schwanenlied dot me>
 * All rights reserved.
 *
 * Redistribution and use in source and binary forms, with or without
 * modification, are permitted provided that the following conditions are met:
 *
 *  * Redistributions of source code must retain the above copyright notice,
 *    this list of conditions and the following disclaimer.
 *
 *  * Redistributions in binary form must reproduce the above copyright notice,
 *    this list of conditions and the following disclaimer in the documentation
 *    and/or other materials provided with the distribution.
 *
 * THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS "AS IS"
 * AND ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT LIMITED TO, THE
 * IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS FOR A PARTICULAR PURPOSE
 * ARE DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT HOLDER OR CONTRIBUTORS BE
 * LIABLE FOR ANY DIRECT, INDIRECT, INCIDENTAL, SPECIAL, EXEMPLARY, OR
 * CONSEQUENTIAL DAMAGES (INCLUDING, BUT NOT LIMITED TO, PROCUREMENT OF
 * SUBSTITUTE GOODS OR SERVICES; LOSS OF USE, DATA, OR PROFITS; OR BUSINESS
 * INTERRUPTION) HOWEVER CAUSED AND ON ANY THEORY OF LIABILITY, WHETHER IN
 * CONTRACT, STRICT LIABILITY, OR TORT (INCLUDING NEGLIGENCE OR OTHERWISE)
 * ARISING IN ANY WAY OUT OF THE USE OF THIS SOFTWARE, EVEN IF ADVISED OF THE
 * POSSIBILITY OF SUCH DAMAGE.
 */

// package front provides an implementation of the Tor Project's front
// obfuscation protocol.
package front // import "github.com/websitefingerprinting/wfdef.git/transports/front"

import (
	"bytes"
	"context"
	"fmt"
	queue "github.com/enriquebris/goconcurrentqueue"
	"github.com/websitefingerprinting/wfdef.git/common/log"
	"github.com/websitefingerprinting/wfdef.git/common/utils"
	"github.com/websitefingerprinting/wfdef.git/transports/pb"
	expRand "golang.org/x/exp/rand"
	"gonum.org/v1/gonum/stat/distuv"
	"google.golang.org/grpc"
	"io"
	"io/ioutil"
	"math"
	"math/rand"
	"net"
	"sort"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"

	"git.torproject.org/pluggable-transports/goptlib.git"
	"github.com/websitefingerprinting/wfdef.git/common/drbg"
	"github.com/websitefingerprinting/wfdef.git/common/ntor"
	"github.com/websitefingerprinting/wfdef.git/common/probdist"
	"github.com/websitefingerprinting/wfdef.git/common/replayfilter"
	"github.com/websitefingerprinting/wfdef.git/transports/base"
	"github.com/websitefingerprinting/wfdef.git/transports/front/framing"
)

const (
	transportName = "front"

	nodeIDArg     = "node-id"
	publicKeyArg  = "public-key"
	privateKeyArg = "private-key"
	seedArg       = "drbg-seed"
	certArg       = "cert"
	wMinArg       = "w-min"
	wMaxArg       = "w-max"
	nServerArg    = "n-server"
	nClientArg    = "n-client"



	seedLength             = drbg.SeedLength
	headerLength           = framing.FrameOverhead + packetOverhead
	clientHandshakeTimeout = time.Duration(60) * time.Second
	serverHandshakeTimeout = time.Duration(30) * time.Second
	replayTTL              = time.Duration(3) * time.Hour

	maxCloseDelay      = 60
	tWindow            = 1000 * time.Millisecond

	gRPCAddr           = "localhost:10086"
	traceLogEnabled    = false
	logEnabled         = true
)

type frontClientArgs struct {
	nodeID     *ntor.NodeID
	publicKey  *ntor.PublicKey
	sessionKey *ntor.Keypair
	wMin       float32     // in seconds
	wMax       float32     // in seconds
	nServer    int
	nClient    int
}

// Transport is the front implementation of the base.Transport interface.
type Transport struct{}

// Name returns the name of the front transport protocol.
func (t *Transport) Name() string {
	return transportName
}

// ClientFactory returns a new frontClientFactory instance.
func (t *Transport) ClientFactory(stateDir string) (base.ClientFactory, error) {
	cf := &frontClientFactory{transport: t}
	return cf, nil
}

// ServerFactory returns a new frontServerFactory instance.
func (t *Transport) ServerFactory(stateDir string, args *pt.Args) (base.ServerFactory, error) {
	st, err := serverStateFromArgs(stateDir, args)
	if err != nil {
		return nil, err
	}


	// Store the arguments that should appear in our descriptor for the clients.
	ptArgs := pt.Args{}
	ptArgs.Add(certArg, st.cert.String())
	ptArgs.Add(wMinArg, strconv.FormatFloat(float64(st.wMin), 'f', -1, 32))
	ptArgs.Add(wMaxArg, strconv.FormatFloat(float64(st.wMax), 'f', -1, 32))
	ptArgs.Add(nServerArg, strconv.Itoa(st.nServer))
	ptArgs.Add(nClientArg, strconv.Itoa(st.nClient))


	// Initialize the replay filter.
	filter, err := replayfilter.New(replayTTL)
	if err != nil {
		return nil, err
	}

	// Initialize the close thresholds for failed connections.
	drbg, err := drbg.NewHashDrbg(st.drbgSeed)
	if err != nil {
		return nil, err
	}
	rng := rand.New(drbg)

	sf := &frontServerFactory{t, &ptArgs, st.nodeID, st.identityKey, st.drbgSeed, st.wMin, st.wMax, st.nServer, st.nClient, filter, rng.Intn(maxCloseDelay)}
	return sf, nil
}

type frontClientFactory struct {
	transport base.Transport
}

func (cf *frontClientFactory) Transport() base.Transport {
	return cf.transport
}

func (cf *frontClientFactory) ParseArgs(args *pt.Args) (interface{}, error) {
	var nodeID *ntor.NodeID
	var publicKey *ntor.PublicKey

	// The "new" (version >= 0.0.3) bridge lines use a unified "cert" argument
	// for the Node ID and Public Key.
	certStr, ok := args.Get(certArg)
	if ok {
		cert, err := serverCertFromString(certStr)
		if err != nil {
			return nil, err
		}
		nodeID, publicKey = cert.unpack()
	} else {
		// The "old" style (version <= 0.0.2) bridge lines use separate Node ID
		// and Public Key arguments in Base16 encoding and are a UX disaster.
		nodeIDStr, ok := args.Get(nodeIDArg)
		if !ok {
			return nil, fmt.Errorf("missing argument '%s'", nodeIDArg)
		}
		var err error
		if nodeID, err = ntor.NodeIDFromHex(nodeIDStr); err != nil {
			return nil, err
		}

		publicKeyStr, ok := args.Get(publicKeyArg)
		if !ok {
			return nil, fmt.Errorf("missing argument '%s'", publicKeyArg)
		}
		if publicKey, err = ntor.PublicKeyFromHex(publicKeyStr); err != nil {
			return nil, err
		}
	}


	nClientStr, nClientOk := args.Get(nClientArg)
	if !nClientOk {
		return nil, fmt.Errorf("missing argument '%s'", nClientArg)
	}
	nClient, err := strconv.Atoi(nClientStr)
	if err != nil {
		return nil, fmt.Errorf("malformed n-client '%s'", nClientStr)
	}
	nServerStr, nServerOk := args.Get(nServerArg)
	if !nServerOk {
		return nil, fmt.Errorf("missing argument '%s'", nServerArg)
	}
	nServer, err := strconv.Atoi(nServerStr)
	if err != nil {
		return nil, fmt.Errorf("malformed n-server '%s'", nServerStr)
	}

	wMinStr, wMinOk := args.Get(wMinArg)
	if !wMinOk {
		return nil, fmt.Errorf("missing argument '%s'", wMinArg)
	}
	wMin, err := strconv.ParseFloat(wMinStr, 32)
	if err != nil {
		return nil, fmt.Errorf("malformed w-min '%s'", wMinStr)
	}

	wMaxStr, wMaxOk := args.Get(wMaxArg)
	if !wMaxOk {
		return nil, fmt.Errorf("missing argument '%s'", wMaxArg)
	}
	wMax, err := strconv.ParseFloat(wMaxStr, 32)
	if err != nil {
		return nil, fmt.Errorf("malformed w-max '%s'", wMaxStr)
	}

	// Generate the session key pair before connectiong to hide the Elligator2
	// rejection sampling from network observers.
	sessionKey, err := ntor.NewKeypair(true)
	if err != nil {
		return nil, err
	}

	return &frontClientArgs{nodeID, publicKey, sessionKey, float32(wMin), float32(wMax),nServer, nClient}, nil
}

func (cf *frontClientFactory) Dial(network, addr string, dialFn base.DialFunc, args interface{}) (net.Conn, error) {
	// Validate args before bothering to open connection.
	ca, ok := args.(*frontClientArgs)
	if !ok {
		return nil, fmt.Errorf("invalid argument type for args")
	}
	conn, err := dialFn(network, addr)
	if err != nil {
		return nil, err
	}
	dialConn := conn
	if conn, err = newfrontClientConn(conn, ca); err != nil {
		dialConn.Close()
		return nil, err
	}
	return conn, nil
}

type frontServerFactory struct {
	transport base.Transport
	args      *pt.Args

	nodeID       *ntor.NodeID
	identityKey  *ntor.Keypair
	lenSeed      *drbg.Seed

	wMin       float32     // in seconds
	wMax       float32     // in seconds
	nServer    int
	nClient    int
	
	replayFilter *replayfilter.ReplayFilter
	closeDelay int
}

func (sf *frontServerFactory) Transport() base.Transport {
	return sf.transport
}

func (sf *frontServerFactory) Args() *pt.Args {
	return sf.args
}

func (sf *frontServerFactory) WrapConn(conn net.Conn) (net.Conn, error) {
	// Not much point in having a separate newFrontServerConn routine when
	// wrapping requires using values from the factory instance.

	// Generate the session keypair *before* consuming data from the peer, to
	// attempt to mask the rejection sampling due to use of Elligator2.  This
	// might be futile, but the timing differential isn't very large on modern
	// hardware, and there are far easier statistical attacks that can be
	// mounted as a distinguisher.
	sessionKey, err := ntor.NewKeypair(true)
	if err != nil {
		return nil, err
	}

	paddingChan := make(chan bool)

	lenDist := probdist.New(sf.lenSeed, 0, framing.MaximumSegmentLength, false)
	logger := &traceLogger{gRPCServer: grpc.NewServer(), logOn: nil, logPath: nil}
	// The server's initial state is intentionally set to stateStart at the very beginning to obfuscate the RTT between client and server
	c := &frontConn{conn, true, lenDist,  sf.wMin, sf.wMax, sf.nServer, sf.nClient,logger, stateStop, paddingChan, nil, bytes.NewBuffer(nil), bytes.NewBuffer(nil), make([]byte, consumeReadSize), nil, nil}
	log.Debugf("Server pt con status: isServer: %v, w-min: %.1f, w-max: %.1f, n-server: %d, n-client: %d", c.isServer, c.wMin, c.wMax, c.nServer, c.nClient)
	startTime := time.Now()

	if err = c.serverHandshake(sf, sessionKey); err != nil {
		log.Errorf("Handshake err %v", err)
		c.closeAfterDelay(sf, startTime)
		return nil, err
	}

	return c, nil
}

type frontConn struct {
	net.Conn

	isServer  bool

	lenDist   *probdist.WeightedDist
	wMin       float32     // in seconds
	wMax       float32     // in seconds
	nServer    int
	nClient    int

	logger *traceLogger

	state     uint32

	paddingChan          chan bool   // true when start defense, false when stop defense
	loggerChan           chan []int64
	receiveBuffer        *bytes.Buffer
	receiveDecodedBuffer *bytes.Buffer
	readBuffer           []byte

	encoder *framing.Encoder
	decoder *framing.Decoder
}


func newfrontClientConn(conn net.Conn, args *frontClientArgs) (c *frontConn, err error) {
	// Generate the initial protocol polymorphism distribution(s).
	var seed *drbg.Seed
	if seed, err = drbg.NewSeed(); err != nil {
		return
	}
	lenDist := probdist.New(seed, 0, framing.MaximumSegmentLength, false)
	var loggerChan chan []int64
	if traceLogEnabled {
		loggerChan = make(chan []int64, 100)
	} else {
		loggerChan = nil
	}
	paddingChan := make(chan bool)

	logPath := atomic.Value{}
	logPath.Store("")
	logOn  := atomic.Value{}
	logOn.Store(false)
	server := grpc.NewServer()
	logger := &traceLogger{gRPCServer: server, logOn: &logOn, logPath: &logPath}

	pb.RegisterTraceLoggingServer(logger.gRPCServer, &traceLoggingServer{callBack:logger.UpdateLogInfo})
	// Allocate the client structure.
	c = &frontConn{conn, false, lenDist, args.wMin, args.wMax, args.nServer, args.nClient, logger, stateStop, paddingChan, loggerChan, bytes.NewBuffer(nil), bytes.NewBuffer(nil), make([]byte, consumeReadSize), nil, nil}

	log.Debugf("Client pt con status: isServer: %v, w-min: %.2f, w-max: %.2f, n-server: %d, n-client: %d", c.isServer, c.wMin, c.wMax, c.nServer, c.nClient)
	// Start the handshake timeout.
	deadline := time.Now().Add(clientHandshakeTimeout)
	if err = conn.SetDeadline(deadline); err != nil {
		return nil, err
	}

	if err = c.clientHandshake(args.nodeID, args.publicKey, args.sessionKey); err != nil {
		return nil, err
	}

	// Stop the handshake timeout.
	if err = conn.SetDeadline(time.Time{}); err != nil {
		return nil, err
	}

	return
}

func (conn *frontConn) clientHandshake(nodeID *ntor.NodeID, peerIdentityKey *ntor.PublicKey, sessionKey *ntor.Keypair) error {
	if conn.isServer {
		return fmt.Errorf("clientHandshake called on server connection")
	}

	// Generate and send the client handshake.
	hs := newClientHandshake(nodeID, peerIdentityKey, sessionKey)
	blob, err := hs.generateHandshake()
	if err != nil {
		return err
	}
	if _, err = conn.Conn.Write(blob); err != nil {
		return err
	}

	// Consume the server handshake.
	var hsBuf [maxHandshakeLength]byte
	for {
		n, err := conn.Conn.Read(hsBuf[:])
		if err != nil {
			// The Read() could have returned data and an error, but there is
			// no point in continuing on an EOF or whatever.
			return err
		}
		conn.receiveBuffer.Write(hsBuf[:n])

		n, seed, err := hs.parseServerHandshake(conn.receiveBuffer.Bytes())
		if err == ErrMarkNotFoundYet {
			continue
		} else if err != nil {
			return err
		}
		_ = conn.receiveBuffer.Next(n)

		// Use the derived key material to intialize the link crypto.
		okm := ntor.Kdf(seed, framing.KeyLength*2)
		conn.encoder = framing.NewEncoder(okm[:framing.KeyLength])
		conn.decoder = framing.NewDecoder(okm[framing.KeyLength:])

		return nil
	}
}

func (conn *frontConn) serverHandshake(sf *frontServerFactory, sessionKey *ntor.Keypair) error {
	if !conn.isServer {
		return fmt.Errorf("serverHandshake called on client connection")
	}

	// Generate the server handshake, and arm the base timeout.
	hs := newServerHandshake(sf.nodeID, sf.identityKey, sessionKey)
	if err := conn.Conn.SetDeadline(time.Now().Add(serverHandshakeTimeout)); err != nil {
		return err
	}

	// Consume the client handshake.
	var hsBuf [maxHandshakeLength]byte
	for {
		n, err := conn.Conn.Read(hsBuf[:])
		if err != nil {
			// The Read() could have returned data and an error, but there is
			// no point in continuing on an EOF or whatever.
			return err
		}
		conn.receiveBuffer.Write(hsBuf[:n])

		seed, err := hs.parseClientHandshake(sf.replayFilter, conn.receiveBuffer.Bytes())
		if err == ErrMarkNotFoundYet {
			continue
		} else if err != nil {
			return err
		}
		conn.receiveBuffer.Reset()

		if err := conn.Conn.SetDeadline(time.Time{}); err != nil {
			return nil
		}

		// Use the derived key material to intialize the link crypto.
		okm := ntor.Kdf(seed, framing.KeyLength*2)
		conn.encoder = framing.NewEncoder(okm[framing.KeyLength:])
		conn.decoder = framing.NewDecoder(okm[:framing.KeyLength])

		break
	}

	// Since the current and only implementation always sends a PRNG seed for
	// the length obfuscation, this makes the amount of data received from the
	// server inconsistent with the length sent from the client.
	//
	// Rebalance this by tweaking the client mimimum padding/server maximum
	// padding, and sending the PRNG seed unpadded (As in, treat the PRNG seed
	// as part of the server response).  See inlineSeedFrameLength in
	// handshake_ntor.go.

	// Generate/send the response.
	blob, err := hs.generateHandshake()
	if err != nil {
		return err
	}
	var frameBuf bytes.Buffer
	if _, err = frameBuf.Write(blob); err != nil {
		return err
	}

	// Send the PRNG seed as the first packet.
	if err := conn.makePacket(&frameBuf, packetTypePrngSeed, sf.lenSeed.Bytes()[:], uint16(maxPacketPayloadLength-len(sf.lenSeed.Bytes()[:]))); err != nil {
		return err
	}
	if _, err = conn.Conn.Write(frameBuf.Bytes()); err != nil {
		return err
	}

	return nil
}

func (conn *frontConn) Read(b []byte) (n int, err error) {
	// If there is no payload from the previous Read() calls, consume data off
	// the network.  Not all data received is guaranteed to be usable payload,
	// so do this in a loop till data is present or an error occurs.
	for conn.receiveDecodedBuffer.Len() == 0 {
		err = conn.readPackets()
		if err == framing.ErrAgain {
			// Don't proagate this back up the call stack if we happen to break
			// out of the loop.
			err = nil
			continue
		} else if err != nil {
			break
		}
	}

	// Even if err is set, attempt to do the read anyway so that all decoded
	// data gets relayed before the connection is torn down.
	if conn.receiveDecodedBuffer.Len() > 0 {
		var berr error
		n, berr = conn.receiveDecodedBuffer.Read(b)
		if err == nil {
			// Only propagate berr if there are not more important (fatal)
			// errors from the network/crypto/packet processing.
			err = berr
		}
	}
	return
}


func (conn *frontConn) initFrontArgs(N int, tsQueue *queue.FixedFIFO, frontInitTime *atomic.Value) (err error){
	wSampler := distuv.Uniform{Min: float64(conn.wMin), Max: float64(conn.wMax),
		Src: expRand.NewSource(uint64(time.Now().UTC().UnixNano()))}
	n_sampler := distuv.Uniform{Min: 1.0, Max: float64(N),
		Src: expRand.NewSource(uint64(time.Now().UTC().UnixNano()))}
	w_tmp := wSampler.Rand()
	n_tmp := int(n_sampler.Rand())
	log.Infof("[Init] Sampled w: %.2fs, n: %d", w_tmp, n_tmp)
	tSampler := distuv.Weibull{K: 2, Lambda: math.Sqrt2 * w_tmp,
		Src: expRand.NewSource(uint64(time.Now().UTC().UnixNano()))}
	ts := make([]float64, n_tmp)
	for i := 0; i  < n_tmp; i++ {
		ts[i] = tSampler.Rand()
	}
	sort.Float64s(ts)
	for tsQueue.GetLen() > 0 {
		//empty the queue before refill it
		_, _ = tsQueue.Dequeue()
	}

	frontInitTime.Store(time.Now())
	for i:= 0; i< n_tmp; i++ {
		err := tsQueue.Enqueue(time.Duration(int64(ts[i] * 1e9)))
		if err != nil {
			return err
		}
	}
	return
}

func (conn *frontConn) ReadFrom(r io.Reader) (written int64, err error) {
	log.Infof("[State] Enter copyloop state: %v (%v is stateStart, %v is statStop)", conn.state, stateStart, stateStop)
	closeChan := make(chan int)
	defer close(closeChan)

	errChan := make(chan error, 5)  // errors from all the go routines will be sent to this channel
	sendChan := make(chan PacketInfo, 65535) // all packed packets are sent through this channel

	var realNSeg uint32 = 0  // real packet counter over 1 second
	var receiveBuf utils.SafeBuffer //read payload from upstream and buffer here
	var frontInitTime atomic.Value
	var tsQueue *queue.FixedFIFO // maintain a queue of timestamps sampled
	var maxPaddingN int
	if conn.isServer {
		maxPaddingN = conn.nServer
	} else {
		maxPaddingN = conn.nClient
	}
	tsQueue = queue.NewFixedFIFO(maxPaddingN)

	//client side launch trace logger routine
	if traceLogEnabled && !conn.isServer {
		//start gRPC routine
		listen, err := net.Listen("tcp", gRPCAddr)
		if err != nil {
			log.Errorf("Fail to launch gRPC service err: %v", err)
			return 0, err
		}
		go func() {
			log.Infof("[Routine] gRPC server starts listeners.")
			gErr := conn.logger.gRPCServer.Serve(listen)
			if gErr != nil {
				log.Infof("[Routine] gRPC server exits by gErr: %v", gErr)
				errChan <- gErr
				return
			} else {
				log.Infof("[Routine] gRPC server is closed.")
			}
		}()

		time.Sleep(50 * time.Millisecond)
		go func() {
			log.Infof("[Routine] Client traceLogger turns on.")
			for {
				select {
				case _, ok := <- closeChan:
					if !ok {
						conn.logger.gRPCServer.Stop()
						log.Infof("[Routine] traceLogger exits by closeChan signal.")
						return
					}
				case pktinfo, ok := <- conn.loggerChan:
					if !ok {
						log.Debugf("[Routine] traceLogger exits: %v.")
						return
					}
					_ = conn.logger.LogTrace(pktinfo[0], pktinfo[1], pktinfo[2])
				}
			}
		}()
	}

	//create a go routine to send out packets to the wire
	go func() {
		for {
			select{
			case _, ok := <- closeChan:
				if !ok{
					log.Infof("[Routine] Send routine exits by closedChan.")
					return
				}
			case packetInfo := <- sendChan:
				pktType := packetInfo.pktType
				data    := packetInfo.data
				padLen  := packetInfo.padLen
				var frameBuf bytes.Buffer
				err = conn.makePacket(&frameBuf, pktType, data, padLen)
				if err != nil {
					errChan <- err
					log.Infof("[Routine] Send routine exits by make pkt err.")
					return
				}
				_, wtErr := conn.Conn.Write(frameBuf.Bytes())
				if wtErr != nil {
					errChan <- wtErr
					log.Infof("[Routine] Send routine exits by write err.")
					return
				}
				if !conn.isServer && traceLogEnabled && conn.logger.logOn.Load().(bool) {
					conn.loggerChan <- []int64{time.Now().UnixNano(), int64(len(data)), int64(padLen)}
				}
				if !conn.isServer && logEnabled {
					log.Infof("[TRACE_LOG] %d %d %d", time.Now().UnixNano(), int64(len(data)), int64(padLen))
				}
				log.Debugf("[Send] %-8s, %-3d+ %-3d bytes at %v", pktTypeMap[pktType], len(data), padLen, time.Now().Format("15:04:05.000"))
			}
		}
	}()

	//create a go routine to receive padding signal and schdule dummy pkts
	//true: need to init front params
	//false: need to cancel unsent dummy packets
	go func() {
		for{
			select{
			case _, ok := <- closeChan:
				if !ok{
					log.Infof("[Routine] padding factory exits by closedChan.")
					return
				}
			case startPadding := <- conn.paddingChan:
				if startPadding {
					err := conn.initFrontArgs(maxPaddingN, tsQueue, &frontInitTime)
					if err != nil {
						errChan <- err
						log.Infof("[Routine] padding factory exits by err in init.")
						return
					}
					log.Debugf("[Event] Get %v dummy packets to send", tsQueue.GetLen())
				} else {
					log.Debugf("[Event] Empty the ts queue (len %v)", tsQueue.GetLen())
					for tsQueue.GetLen() > 0 {
						_, _ = tsQueue.Dequeue()
					}
				}
			}
		}
	}()

	go func() {
		for{
			select {
			case _, ok := <-closeChan:
				if !ok {
					log.Infof("[Routine] padding routine exits by closedChan.")
					return
				}
			default:
				// here to send out dummy packets
				if atomic.LoadUint32(&conn.state) == stateStop {
					time.Sleep(20 * time.Millisecond)
					continue
				}
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				timestamp, qErr := tsQueue.DequeueOrWaitForNextElementContext(ctx)
				if qErr == context.DeadlineExceeded {
					log.Infof("[Routine] Dequeue timeout after 5 seconds.")
					continue
				}
				if qErr != nil {
					log.Infof("[Routine] padding routine exits by dequeue err.")
					errChan <- qErr
					return
				}
				cancel()
				utils.SleepRho(frontInitTime.Load().(time.Time) ,timestamp.(time.Duration))
				sendChan <- PacketInfo{pktType: packetTypeDummy, data: []byte{},  padLen: maxPacketPaddingLength}
			}
		}
	}()

	// this go routine regularly check the real throughput
	// if it is small, change to stop state
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for{
			select{
			case _, ok := <- closeChan:
				if !ok {
					log.Infof("[Routine] Ticker routine exits by closeChan.")
					return
				}
			case <- ticker.C:
				//log.Debugf("NRealSeg %v at %v", realNSeg, time.Now().Format("15:04:05.000000"))
				if !conn.isServer && atomic.LoadUint32(&conn.state) != stateStop && atomic.LoadUint32(&realNSeg) < 2 {
					log.Infof("[State] %s -> %s.", stateMap[atomic.LoadUint32(&conn.state)], stateMap[stateStop])
					atomic.StoreUint32(&conn.state, stateStop)
					sendChan <- PacketInfo{pktType: packetTypeSignalStop, data: []byte{}, padLen: maxPacketPaddingLength}
					conn.paddingChan <- false
				}
				atomic.StoreUint32(&realNSeg, 0) //reset counter
			}
		}
	}()

	for {
		select {
		case conErr := <- errChan:
			log.Infof("downstream copy loop terminated at %v. Reason: %v", time.Now().Format("15:04:05.000000"), conErr)
			return written, conErr
		default:
			buf := make([]byte, 65535)
			rdLen, err := r.Read(buf[:])
			if err!= nil {
				log.Errorf("Exit by read err:%v", err)
				return written, err
			}
			if rdLen > 0 {
				receiveBuf.Write(buf[: rdLen])
			} else {
				log.Errorf("BUG? read 0 bytes, err: %v", err)
				return written, io.EOF
			}
			//signal server to start if there is more than one cell coming
			// else switch to padding state
			// stop -> ready -> start
			if !conn.isServer {
				if (atomic.LoadUint32(&conn.state) == stateStop && rdLen > maxPacketPayloadLength) ||
					(atomic.LoadUint32(&conn.state) == stateReady) {
					log.Infof("[State] %s -> %s.", stateMap[atomic.LoadUint32(&conn.state)], stateMap[stateStart])
					atomic.StoreUint32(&conn.state, stateStart)
					sendChan <- PacketInfo{pktType: packetTypeSignalStart, data: []byte{}, padLen: maxPacketPaddingLength}
					conn.paddingChan <- true
				} else if atomic.LoadUint32(&conn.state) == stateStop {
					log.Infof("[State] %s -> %s.", stateMap[stateStop], stateMap[stateReady])
					atomic.StoreUint32(&conn.state, stateReady)
				}
			}
			for receiveBuf.GetLen() > 0 {
				var payload [maxPacketPayloadLength]byte
				rdLen, rdErr := receiveBuf.Read(payload[:])
				written += int64(rdLen)
				if rdErr != nil {
					log.Infof("Exit by read buffer err:%v", rdErr)
					return written, rdErr
				}
				sendChan <- PacketInfo{pktType: packetTypePayload, data: payload[:rdLen], padLen: uint16(maxPacketPaddingLength-rdLen)}
				atomic.AddUint32(&realNSeg, 1)
			}
		}
	}
}


func (conn *frontConn) SetDeadline(t time.Time) error {
	return syscall.ENOTSUP
}

func (conn *frontConn) SetWriteDeadline(t time.Time) error {
	return syscall.ENOTSUP
}

func (conn *frontConn) closeAfterDelay(sf *frontServerFactory, startTime time.Time) {
	// I-it's not like I w-wanna handshake with you or anything.  B-b-baka!
	defer conn.Conn.Close()

	delay := time.Duration(sf.closeDelay)*time.Second + serverHandshakeTimeout
	deadline := startTime.Add(delay)
	if time.Now().After(deadline) {
		return
	}

	if err := conn.Conn.SetReadDeadline(deadline); err != nil {
		return
	}

	// Consume and discard data on this connection until the specified interval
	// passes.
	_, _ = io.Copy(ioutil.Discard, conn.Conn)
}



var _ base.ClientFactory = (*frontClientFactory)(nil)
var _ base.ServerFactory = (*frontServerFactory)(nil)
var _ base.Transport = (*Transport)(nil)
var _ net.Conn = (*frontConn)(nil)
