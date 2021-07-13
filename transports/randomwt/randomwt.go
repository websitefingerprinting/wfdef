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

// Package randomwt provides an implementation of the Tor Project's randomwt
// obfuscation protocol.
package randomwt // import "github.com/websitefingerprinting/wfdef.git/transports/randomwt"

import (
	"bytes"
	"fmt"
	"github.com/websitefingerprinting/wfdef.git/common/log"
	"github.com/websitefingerprinting/wfdef.git/common/utils"
	"github.com/websitefingerprinting/wfdef.git/transports/pb"
	"google.golang.org/grpc"
	"io"
	"io/ioutil"
	"math/rand"
	"net"
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
	"github.com/websitefingerprinting/wfdef.git/transports/randomwt/framing"
)

const (
	transportName    = "randomwt"

	nodeIDArg        = "node-id"
	publicKeyArg     = "public-key"
	privateKeyArg    = "private-key"
	seedArg          = "drbg-seed"
	certArg          = "cert"
	nClientRealArg   = "n-client-real"
	nServerRealArg   = "n-server-real"
	nClientFakeArg   = "n-client-fake"
	nServerFakeArg   = "n-server-fake"
	pFakeArg         = "p-fake"



	seedLength             = drbg.SeedLength
	clientHandshakeTimeout = time.Duration(60) * time.Second
	serverHandshakeTimeout = time.Duration(30) * time.Second
	replayTTL              = time.Duration(3) * time.Hour

	maxCloseDelay      = 60
	maxWaitingTime     = 500 * time.Millisecond
	aliveCheckingTime  = 5 * time.Second

	gRPCAddr           = "localhost:10086"
	traceLogEnabled    = false
	logEnabled         = true
)

type randomwtClientArgs struct {
	nodeID      *ntor.NodeID
	publicKey   *ntor.PublicKey
	sessionKey  *ntor.Keypair
	nClientReal int
	nServerReal int
	nClientFake int
	nServerFake int
	pFake       float64
}

// Transport is the randomwt implementation of the base.Transport interface.
type Transport struct{}

// Name returns the name of the randomwt transport protocol.
func (t *Transport) Name() string {
	return transportName
}

// ClientFactory returns a new randomwtClientFactory instance.
func (t *Transport) ClientFactory(stateDir string) (base.ClientFactory, error) {
	cf := &randomwtClientFactory{transport: t}
	return cf, nil
}

// ServerFactory returns a new randomwtServerFactory instance.
func (t *Transport) ServerFactory(stateDir string, args *pt.Args) (base.ServerFactory, error) {
	st, err := serverStateFromArgs(stateDir, args)
	if err != nil {
		return nil, err
	}


	// Store the arguments that should appear in our descriptor for the clients.
	ptArgs := pt.Args{}
	ptArgs.Add(certArg, st.cert.String())
	ptArgs.Add(nClientRealArg, strconv.Itoa(st.nClientReal))
	ptArgs.Add(nServerRealArg, strconv.Itoa(st.nServerReal))
	ptArgs.Add(nClientFakeArg, strconv.Itoa(st.nClientFake))
	ptArgs.Add(nServerFakeArg, strconv.Itoa(st.nServerFake))
	ptArgs.Add(pFakeArg, strconv.FormatFloat(st.pFake, 'f', -1, 64))


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

	sf := &randomwtServerFactory{t, &ptArgs, st.nodeID, st.identityKey, st.drbgSeed, st.nClientReal, st.nServerReal, st.nClientFake, st.nServerFake, st.pFake, filter, rng.Intn(maxCloseDelay)}
	return sf, nil
}

type randomwtClientFactory struct {
	transport base.Transport
}

func (cf *randomwtClientFactory) Transport() base.Transport {
	return cf.transport
}


func (cf *randomwtClientFactory) ParseArgs(args *pt.Args) (interface{}, error) {
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


	nClientReal, err := utils.ParseArgByKey(args, nClientRealArg, "int")
	if err != nil {
		return nil ,err
	}
	nServerReal, err := utils.ParseArgByKey(args, nServerRealArg, "int")
	if err != nil {
		return nil, err
	}
	nClientFake, err := utils.ParseArgByKey(args, nClientFakeArg, "int")
	if err != nil {
		return nil, err
	}
	nServerFake, err := utils.ParseArgByKey(args, nServerFakeArg, "int")
	if err != nil {
		return nil, err
	}
	pFake, err := utils.ParseArgByKey(args, pFakeArg, "float64")
	if err != nil {
		return nil, err
	}


	// Generate the session key pair before connectiong to hide the Elligator2
	// rejection sampling from network observers.
	sessionKey, err := ntor.NewKeypair(true)
	if err != nil {
		return nil, err
	}

	return &randomwtClientArgs{nodeID, publicKey, sessionKey,
		nClientReal.(int), nServerReal.(int), nClientFake.(int), nServerFake.(int), pFake.(float64)}, nil
}

func (cf *randomwtClientFactory) Dial(network, addr string, dialFn base.DialFunc, args interface{}) (net.Conn, error) {
	// Validate args before bothering to open connection.
	ca, ok := args.(*randomwtClientArgs)
	if !ok {
		return nil, fmt.Errorf("invalid argument type for args")
	}
	conn, err := dialFn(network, addr)
	if err != nil {
		return nil, err
	}
	dialConn := conn
	if conn, err = newRandomwtClientConn(conn, ca); err != nil {
		dialConn.Close()
		return nil, err
	}
	return conn, nil
}

type randomwtServerFactory struct {
	transport base.Transport
	args      *pt.Args

	nodeID       *ntor.NodeID
	identityKey  *ntor.Keypair
	lenSeed      *drbg.Seed

	nClientReal  int
	nServerReal  int
	nClientFake  int
	nServerFake  int
	pFake        float64
	
	replayFilter *replayfilter.ReplayFilter
	closeDelay int
}

func (sf *randomwtServerFactory) Transport() base.Transport {
	return sf.transport
}

func (sf *randomwtServerFactory) Args() *pt.Args {
	return sf.args
}

func (sf *randomwtServerFactory) WrapConn(conn net.Conn) (net.Conn, error) {
	// Not much point in having a separate newRandomwtServerConn routine when
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

	canSendChan := make(chan uint32, 1)

	lenDist := probdist.New(sf.lenSeed, 0, framing.MaximumSegmentLength, false)
	logger := &traceLogger{gRPCServer: grpc.NewServer(), logOn: nil, logPath: nil}
	// The server's initial state is intentionally set to stateStart at the very beginning to obfuscate the RTT between client and server
	c := &randomwtConn{conn, true, lenDist,  sf.nClientReal, sf.nServerReal, sf.nClientFake, sf.nServerFake,sf.pFake, logger, stateStop, canSendChan, nil, bytes.NewBuffer(nil), bytes.NewBuffer(nil), make([]byte, consumeReadSize), nil, nil}
	log.Debugf("Server pt con status: isServer: %v, n-client-real: %d, n-server-real: %d, n-client-fake: %d, n-server-fake: %d, p-fake: %.1f", c.isServer, c.nClientReal, c.nServerReal, c.nClientFake, c.nServerFake, c.pFake)
	startTime := time.Now()

	if err = c.serverHandshake(sf, sessionKey); err != nil {
		log.Errorf("Handshake err %v", err)
		c.closeAfterDelay(sf, startTime)
		return nil, err
	}

	return c, nil
}

type randomwtConn struct {
	net.Conn

	isServer     bool

	lenDist      *probdist.WeightedDist
	nClientReal  int
	nServerReal  int
	nClientFake  int
	nServerFake  int
	pFake        float64

	logger *traceLogger

	state     uint32

	canSendChan          chan uint32
	loggerChan           chan []int64
	receiveBuffer        *bytes.Buffer
	receiveDecodedBuffer *bytes.Buffer
	readBuffer           []byte

	encoder *framing.Encoder
	decoder *framing.Decoder
}

func newRandomwtClientConn(conn net.Conn, args *randomwtClientArgs) (c *randomwtConn, err error) {
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
	canSendChan := make(chan uint32, 1)

	logPath := atomic.Value{}
	logPath.Store("")
	logOn  := atomic.Value{}
	logOn.Store(false)
	server := grpc.NewServer()
	logger := &traceLogger{gRPCServer: server, logOn: &logOn, logPath: &logPath}

	pb.RegisterTraceLoggingServer(logger.gRPCServer, &traceLoggingServer{callBack:logger.UpdateLogInfo})
	// Allocate the client structure.
	c = &randomwtConn{conn, false, lenDist, args.nClientReal, args.nServerReal, args.nClientFake, args.nServerFake, args.pFake, logger, stateStop, canSendChan, loggerChan, bytes.NewBuffer(nil), bytes.NewBuffer(nil), make([]byte, consumeReadSize), nil, nil}
	log.Debugf("Server pt con status: isServer: %v, n-client-real: %d, n-server-real: %d, n-client-fake: %d, n-server-fake: %d, p-fake: %.1f", c.isServer, c.nClientReal, c.nServerReal, c.nClientFake, c.nServerFake, c.pFake)
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

func (conn *randomwtConn) returnRoleString() string {
	if conn.isServer{
		return "Server"
	} else {
		return "Client"
	}
}

func (conn *randomwtConn) clientHandshake(nodeID *ntor.NodeID, peerIdentityKey *ntor.PublicKey, sessionKey *ntor.Keypair) error {
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

func (conn *randomwtConn) serverHandshake(sf *randomwtServerFactory, sessionKey *ntor.Keypair) error {
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

func (conn *randomwtConn) Read(b []byte) (n int, err error) {
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

func (conn *randomwtConn) tearDown() {
	var frameBuf bytes.Buffer
	_ = conn.makePacket(&frameBuf, packetTypeTearDown, []byte{}, maxPacketPaddingLength)
	_, _ = conn.Conn.Write(frameBuf.Bytes())
}

func (conn *randomwtConn) ReadFrom(r io.Reader) (written int64, err error) {
	log.Debugf("[State] Enter copyloop state: %v", stateMap[conn.state])
	closeChan := make(chan int)
	var realNSeg uint32
	defer close(closeChan)
	defer conn.tearDown()


	errChan := make(chan error, 5)  // errors from all the go routines will be sent to this channel
	writeChan := make(chan PacketInfo, 65535) // all packed packets are sent through this channel

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
			case packetInfo := <- writeChan:
				pktType := packetInfo.pktType
				data    := packetInfo.data
				padLen  := packetInfo.padLen
				if pktType == packetTypePayload {
					atomic.AddUint32(&realNSeg, 1)
				}
				var frameBuf bytes.Buffer
				err = conn.makePacket(&frameBuf, pktType, data, padLen)
				if err != nil {
					errChan <- err
					return
				}
				_, wtErr := conn.Conn.Write(frameBuf.Bytes())
				if wtErr != nil {
					errChan <- wtErr
					log.Infof("[Routine] Send routine exits by write err.")
					return
				}
				if !conn.isServer && traceLogEnabled && conn.logger.logOn.Load().(bool) {
					if pktType != packetTypeFakeFinish && pktType != packetTypeRealFinish {
						// we assume that the attacker knows the protocol
						// therefore, he can simply always remove the last packet of a burst which is a FakeFinish or RealFinish dummy packet
						conn.loggerChan <- []int64{time.Now().UnixNano(), int64(len(data)), int64(padLen)}
					}
				}
				if !conn.isServer && pktType != packetTypeFakeFinish && pktType != packetTypeRealFinish && logEnabled{
					log.Infof("[TRACE_LOG] %d %d %d", time.Now().UnixNano(), int64(len(data)), int64(padLen))
				}
				log.Debugf("[Send] %-10s, %-3d+%-3d bytes at %v", pktTypeMap[pktType], len(data), padLen, time.Now().Format("15:04:05.000"))
			}
		}
	}()

	// this go routine regularly checks the real throughput
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
					// stateStartReady -> stateStop
					// stateStart -> stateStop
					writeChan <- PacketInfo{pktType: packetTypeSignalStop, data: []byte{}, padLen: maxPacketPaddingLength}
					log.Debugf("[State] %s -> %s.", stateMap[atomic.LoadUint32(&conn.state)], stateMap[stateStop])
					atomic.StoreUint32(&conn.state, stateStop)
				}
				atomic.StoreUint32(&realNSeg, 0) //reset counter
			}
		}
	}()

	if !conn.isServer {
		// client initiate the communication
		conn.canSendChan <- signalReal
	}

	var isFakeTurn uint32 = 0
	isAliveTimer := time.NewTimer(aliveCheckingTime)
	defer isAliveTimer.Stop()
	for {
		select {
		case conErr := <- errChan:
			log.Infof("downstream copy loop terminated at %v. Reason: %v", time.Now().Format("15:04:05.000000"), conErr)
			return written, conErr
		case signal :=<- conn.canSendChan:
			log.Debugf("------Enter the send loop------")
			if !isAliveTimer.Stop() {
				<- isAliveTimer.C
			}
			isAliveTimer.Reset(aliveCheckingTime)
			if signal == signalTearDown {
				log.Infof("teardown signal from otherside.")
				return written, io.EOF
			}
			var wLen int64
			var wErr error
			if !conn.isServer{
				wLen, wErr = conn.clientTalkie(isFakeTurn, writeChan, r.(net.Conn))
				isFakeTurn = (isFakeTurn + 1) % 2
			} else {
				wLen, wErr = conn.serverTalkie(signal, writeChan, r.(net.Conn))
			}
			written += wLen
			if wErr != nil {
				return written, wErr
			}
		case <- isAliveTimer.C:
			// to avoid one party hanging over forever when the other side exits unexpectedly
			// we try break the dead lock by actively sending out a realFinish packet affter waiting for at least `aliveCheckingTime`
			writeChan <- PacketInfo{pktType: packetTypeRealFinish, data: []byte{}, padLen: maxPacketPaddingLength}
			log.Warnf("[Event] hang on for at least %v, to avoid dead lock, send finish packet at %v", aliveCheckingTime, time.Now().Format("15:04:05.000000"))
			isAliveTimer.Reset(aliveCheckingTime)
		}
	}
}

func (conn *randomwtConn) clientTalkie(isFakeTurn uint32, writeChan chan PacketInfo, r net.Conn) (written int64, err error){
	log.Debugf("isFakeTurn: %v", isFakeTurn)
	var burstSize int
	if atomic.LoadUint32(&conn.state) != stateStop && isFakeTurn % 2 == 1 {
		// send a fake burst
		shouldSendFake := utils.Bernoulli(conn.pFake)
		if shouldSendFake == 1 {
			burstSize = conn.sampleDummyBurst(conn.nClientFake, writeChan)
			writeChan <- PacketInfo{pktType: packetTypeFakeFinish, data: []byte{}, padLen: maxPacketPaddingLength}
			log.Debugf("[fake] send a fake burst of size %-2d at %v", burstSize + 1, time.Now().Format("15:04:05.000"))
		} else {
			// skip this round
			log.Debugf("[Event] skip this fake sending round")
			conn.canSendChan <- signalReal
		}
		return 0, nil
	} else {
		// send a real burst
		written, err = conn.padRealBurst(writeChan, r)
		return written, err
	}
}

func (conn *randomwtConn) serverTalkie(signal uint32, writeChan chan PacketInfo, r net.Conn) (written int64, err error){
	log.Debugf("signal: %v", signalMap[signal])
	var burstSize int
	if signal == signalDummy {
		// send a fake burst
		burstSize = conn.sampleDummyBurst(conn.nServerFake, writeChan)
		writeChan <- PacketInfo{pktType: packetTypeFakeFinish, data: []byte{}, padLen: maxPacketPaddingLength}
		log.Debugf("[fake] send a fake burst of size %-2d at %v", burstSize + 1, time.Now().Format("15:04:05.000"))
		return 0, nil
	} else if signal == signalReal {
		written, err = conn.padRealBurst(writeChan, r)
	} else {
		panic(fmt.Sprintf("[Error] Received invalid signal '%v'", signal))
	}
	return written, err
}


func (conn *randomwtConn) sampleDummyBurst (n int, writeChan chan PacketInfo) (burstSize int){
	if atomic.LoadUint32(&conn.state) == stateStop {
		return 0
	}
	burstSize = utils.Uniform(0, n) // sample a number between 0 ~ n
	log.Debugf("Max N: %v, sampled N: %v", n, burstSize)
	for i := 0; i < burstSize; i++ {
		writeChan <- PacketInfo{pktType: packetTypeDummy, data: []byte{}, padLen: maxPacketPaddingLength}
	}
	return burstSize
}

func (conn *randomwtConn) padRealBurst (writeChan chan PacketInfo, r io.Reader) (written int64, err error) {
	var nFake int
	var nReal int
	var curState uint32
	if conn.isServer {
		nFake = conn.nServerFake
		nReal = conn.nServerReal
	} else {
		nFake = conn.nClientFake
		nReal = conn.nClientReal
	}

	// send a real burst
	err = r.(net.Conn).SetReadDeadline(time.Now().Add(maxWaitingTime)) // timeout
	if err != nil {
		log.Errorf("setReadDeadline failed:", err)
		return 0, err
	}
	buf := make([]byte, 65535)
	rdLen, rdErr := r.Read(buf[:])
	if rdErr == nil {
		// no err
		if !conn.isServer {
			//stateStop -(one cell)-> stateStartReady -(one cell)-> stateStart
			curState = atomic.LoadUint32(&conn.state)
			var nextState uint32
			if curState == stateStop {
				if rdLen <= maxPacketPayloadLength {
					nextState = stateStartReady
				} else {
					nextState = stateStart
				}
			} else if curState == stateStartReady {
				nextState = stateStart
			}
			if nextState == stateStart && curState != stateStart {
				//signal server to start.
				writeChan <- PacketInfo{pktType: packetTypeSignalStart, data: []byte{}, padLen: maxPacketPaddingLength}
			}
			atomic.StoreUint32(&conn.state, nextState)
			log.Debugf("[State] %s -> %s", stateMap[curState], stateMap[nextState])		
		}
		
		var receiveBuf bytes.Buffer
		burstSize := 0
		// has real data
		receiveBuf.Write(buf[: rdLen])

		for receiveBuf.Len() > 0 {
			var payload [maxPacketPayloadLength]byte
			rLen, rErr := receiveBuf.Read(payload[:])
			written += int64(rLen)
			if rErr != nil {
				return written, rErr
			}
			writeChan <- PacketInfo{pktType: packetTypePayload, data: payload[:rLen], padLen: uint16(maxPacketPaddingLength-rLen)}
			burstSize += 1
		}
		burstSize += conn.sampleDummyBurst(nReal, writeChan)
		writeChan <- PacketInfo{pktType: packetTypeRealFinish, data: []byte{}, padLen: maxPacketPaddingLength}
		log.Debugf("[real] send a real burst of size %-2d at %v", burstSize + 1, time.Now().Format("15:04:05.000"))
	} else if netErr, ok := rdErr.(net.Error); ok && netErr.Timeout() {
		// timeout err
		var burstSize int
		burstSize = conn.sampleDummyBurst(nFake, writeChan)
		// still need to use realfinish signal otherwise the server wound respond with a fake burst
		// without trying to fetch data from its upstream
		writeChan <- PacketInfo{pktType: packetTypeRealFinish, data: []byte{}, padLen: maxPacketPaddingLength}
		log.Debugf("[timeout] send a fake burst of size %-2d at %v", burstSize + 1, time.Now().Format("15:04:05.000"))
	} else{
		// some err
		return 0, rdErr
	}
	return written, err
}

func (conn *randomwtConn) SetDeadline(t time.Time) error {
	return syscall.ENOTSUP
}

func (conn *randomwtConn) SetWriteDeadline(t time.Time) error {
	return syscall.ENOTSUP
}

func (conn *randomwtConn) closeAfterDelay(sf *randomwtServerFactory, startTime time.Time) {
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



var _ base.ClientFactory = (*randomwtClientFactory)(nil)
var _ base.ServerFactory = (*randomwtServerFactory)(nil)
var _ base.Transport = (*Transport)(nil)
var _ net.Conn = (*randomwtConn)(nil)
