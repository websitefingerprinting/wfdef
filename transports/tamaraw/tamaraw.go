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

// Package tamaraw provides an implementation of the Tor Project's tamaraw
// obfuscation protocol.
package tamaraw // import "github.com/websitefingerprinting/wfdef.git/transports/tamaraw"

import (
	"bytes"
	"fmt"
	"github.com/websitefingerprinting/wfdef.git/common/log"
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
	"github.com/websitefingerprinting/wfdef.git/transports/tamaraw/framing"
)

const (
	transportName = "tamaraw"

	nodeIDArg     = "node-id"
	publicKeyArg  = "public-key"
	privateKeyArg = "private-key"
	seedArg       = "drbg-seed"
	certArg       = "cert"
	rhoServerArg  = "rho-server"
	rhoClientArg  = "rho-client"
	nSegArg       = "nseg"


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

type tamarawClientArgs struct {
	nodeID     *ntor.NodeID
	publicKey  *ntor.PublicKey
	sessionKey *ntor.Keypair
	nSeg       int
	rhoServer  int   // in milliseconds
	rhoClient  int  // in milliseconds
}

// Transport is the tamaraw implementation of the base.Transport interface.
type Transport struct{}

// Name returns the name of the tamaraw transport protocol.
func (t *Transport) Name() string {
	return transportName
}

// ClientFactory returns a new tamarawClientFactory instance.
func (t *Transport) ClientFactory(stateDir string) (base.ClientFactory, error) {
	cf := &tamarawClientFactory{transport: t}
	return cf, nil
}

// ServerFactory returns a new tamarawServerFactory instance.
func (t *Transport) ServerFactory(stateDir string, args *pt.Args) (base.ServerFactory, error) {
	st, err := serverStateFromArgs(stateDir, args)
	if err != nil {
		return nil, err
	}


	// Store the arguments that should appear in our descriptor for the clients.
	ptArgs := pt.Args{}
	ptArgs.Add(certArg, st.cert.String())
	ptArgs.Add(nSegArg, strconv.Itoa(st.nSeg))
	ptArgs.Add(rhoServerArg, strconv.Itoa(st.rhoServer))
	ptArgs.Add(rhoClientArg, strconv.Itoa(st.rhoClient))


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

	sf := &tamarawServerFactory{t, &ptArgs, st.nodeID, st.identityKey, st.drbgSeed, st.nSeg, st.rhoServer, st.rhoClient, filter, rng.Intn(maxCloseDelay)}
	return sf, nil
}

type tamarawClientFactory struct {
	transport base.Transport
}

func (cf *tamarawClientFactory) Transport() base.Transport {
	return cf.transport
}

func (cf *tamarawClientFactory) ParseArgs(args *pt.Args) (interface{}, error) {
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


	nSegStr, nSegOk := args.Get(nSegArg)
	if !nSegOk {
		return nil, fmt.Errorf("missing argument '%s'", nSegArg)
	}
	nSeg, err := strconv.Atoi(nSegStr)
	if err != nil {
		return nil, fmt.Errorf("malformed nseg '%s'", nSegStr)
	}

	rhoServerStr, rhoServerOk := args.Get(rhoServerArg)
	if !rhoServerOk {
		return nil, fmt.Errorf("missing argument '%s'", rhoServerArg)

	}
	rhoServer, err := strconv.Atoi(rhoServerStr)
	if err != nil {
		return nil, fmt.Errorf("malformed rho-client '%s'", rhoServerStr)
	}
	
	rhoClientStr, rhoClientOk := args.Get(rhoClientArg)
	if !rhoClientOk {
		return nil, fmt.Errorf("missing argument '%s'", rhoClientArg)

	}
	rhoClient, err := strconv.Atoi(rhoClientStr)
	if err != nil {
		return nil, fmt.Errorf("malformed rho-client '%s'", rhoClientStr)
	}


	// Generate the session key pair before connectiong to hide the Elligator2
	// rejection sampling from network observers.
	sessionKey, err := ntor.NewKeypair(true)
	if err != nil {
		return nil, err
	}

	return &tamarawClientArgs{nodeID, publicKey, sessionKey, nSeg, rhoServer, rhoClient}, nil
}

func (cf *tamarawClientFactory) Dial(network, addr string, dialFn base.DialFunc, args interface{}) (net.Conn, error) {
	// Validate args before bothering to open connection.
	ca, ok := args.(*tamarawClientArgs)
	if !ok {
		return nil, fmt.Errorf("invalid argument type for args")
	}
	conn, err := dialFn(network, addr)
	if err != nil {
		return nil, err
	}
	dialConn := conn
	if conn, err = newTamarawClientConn(conn, ca); err != nil {
		dialConn.Close()
		return nil, err
	}
	return conn, nil
}

type tamarawServerFactory struct {
	transport base.Transport
	args      *pt.Args

	nodeID       *ntor.NodeID
	identityKey  *ntor.Keypair
	lenSeed      *drbg.Seed
	
	nSeg        int
	rhoServer    int
	rhoClient    int
	
	replayFilter *replayfilter.ReplayFilter
	closeDelay int
}

func (sf *tamarawServerFactory) Transport() base.Transport {
	return sf.transport
}

func (sf *tamarawServerFactory) Args() *pt.Args {
	return sf.args
}

func (sf *tamarawServerFactory) WrapConn(conn net.Conn) (net.Conn, error) {
	// Not much point in having a separate newTamarawServerConn routine when
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

	lenDist := probdist.New(sf.lenSeed, 0, framing.MaximumSegmentLength, false)
	logger := &traceLogger{gRPCServer: grpc.NewServer(), logOn: nil, logPath: nil}
	// The server's initial state is intentionally set to stateStart at the very beginning to obfuscate the RTT between client and server
	c := &tamarawConn{conn, true, lenDist,  sf.nSeg, sf.rhoClient, sf.rhoServer, logger, stateStop, nil, bytes.NewBuffer(nil), bytes.NewBuffer(nil), make([]byte, consumeReadSize), nil, nil}
	log.Debugf("Server pt con status: %v %v %v %v", c.isServer, c.nSeg, c.rhoClient, c.rhoServer)
	startTime := time.Now()

	if err = c.serverHandshake(sf, sessionKey); err != nil {
		log.Errorf("Handshake err %v", err)
		c.closeAfterDelay(sf, startTime)
		return nil, err
	}

	return c, nil
}

type tamarawConn struct {
	net.Conn

	isServer  bool

	lenDist   *probdist.WeightedDist
	nSeg       int
	rhoClient  int
	rhoServer  int

	logger *traceLogger

	state     uint32

	loggerChan           chan []int64
	receiveBuffer        *bytes.Buffer
	receiveDecodedBuffer *bytes.Buffer
	readBuffer           []byte

	encoder *framing.Encoder
	decoder *framing.Decoder
}


func newTamarawClientConn(conn net.Conn, args *tamarawClientArgs) (c *tamarawConn, err error) {
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

	logPath := atomic.Value{}
	logPath.Store("")
	logOn  := atomic.Value{}
	logOn.Store(false)
	server := grpc.NewServer()
	logger := &traceLogger{gRPCServer: server, logOn: &logOn, logPath: &logPath}

	pb.RegisterTraceLoggingServer(logger.gRPCServer, &traceLoggingServer{callBack:logger.UpdateLogInfo})

	// Allocate the client structure.
	c = &tamarawConn{conn, false, lenDist, args.nSeg, args.rhoClient, args.rhoServer, logger, stateStop, loggerChan, bytes.NewBuffer(nil), bytes.NewBuffer(nil), make([]byte, consumeReadSize), nil, nil}

	log.Debugf("client pt con status: %v %v %v %v", c.isServer, c.nSeg, c.rhoClient, c.rhoServer)
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

func (conn *tamarawConn) clientHandshake(nodeID *ntor.NodeID, peerIdentityKey *ntor.PublicKey, sessionKey *ntor.Keypair) error {
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

func (conn *tamarawConn) serverHandshake(sf *tamarawServerFactory, sessionKey *ntor.Keypair) error {
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
	if err := conn.makePacket(&frameBuf, packetTypePrngSeed, sf.lenSeed.Bytes()[:], 0); err != nil {
		return err
	}
	if _, err = conn.Conn.Write(frameBuf.Bytes()); err != nil {
		return err
	}

	return nil
}

func (conn *tamarawConn) Read(b []byte) (n int, err error) {
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

func (conn *tamarawConn) ReadFrom(r io.Reader) (written int64, err error) {
	log.Debugf("[State] Enter copyloop state: %v", stateMap[conn.state])
	closeChan := make(chan int)
	defer close(closeChan)

	errChan := make(chan error, 5)
	var rho time.Duration
	if conn.isServer {
		rho = time.Duration(conn.rhoServer * 1e6)
	} else {
		rho = time.Duration(conn.rhoClient * 1e6)
	}

	var curNSeg uint32 = 0
	var realNSeg uint32 = 0 //the number of real packets over the latest windowSize time
	sendChan := make(chan PacketInfo, 65535) // all packed packets are sent through this channel
	var receiveBuf bytes.Buffer

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
		ticker := time.NewTicker(rho)
		defer ticker.Stop()
		var pktType uint8
		var data []byte
		var padLen uint16
		for {
			select{
			case _, ok := <- closeChan:
				if !ok{
					log.Infof("[Routine] Send routine exits by closedChan.")
					return
				}
			case <- ticker.C:
				atomic.AddUint32(&curNSeg, 1)
				var frameBuf bytes.Buffer
				var curState uint32
				if len(sendChan) > 0 {
					// pkts in the queue: possibly signal ones or real ones
					packetInfo := <- sendChan
					pktType = packetInfo.pktType
					data    = packetInfo.data
					padLen  = packetInfo.padLen
				} else {
					pktType = packetTypeDummy
					data = []byte{}
					padLen = maxPacketPaddingLength
				}

				curState = atomic.LoadUint32(&conn.state)
				if (curState == stateStop || curState == stateReady) && pktType == packetTypeDummy {
					// stop state should not send dummy packets
					continue
				}

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
					conn.loggerChan <- []int64{time.Now().UnixNano(), int64(len(data)), int64(padLen)}
				}
				if !conn.isServer && logEnabled{
					log.Infof("[TRACE_LOG] %d %d %d", time.Now().UnixNano(), int64(len(data)), int64(padLen))
				}
				//log.Debugf("[Send] %-8s, %-3d+ %-3d bytes at %v", pktTypeMap[pktType], len(data), padLen, time.Now().Format("15:04:05.000"))

				// update state for client
				// stateStop -(real pkt)-> stateReady
				// stateReady -(real pkt)-> stateStart and send a StartSignal to server
				// statePadding -(curNSeg % NSeg == 0)-> stateStop and send a StopSignal to server
				if !conn.isServer {
					curState = atomic.LoadUint32(&conn.state)
					if curState == stateStop && pktType == packetTypePayload {
						atomic.StoreUint32(&conn.state, stateReady)
						log.Debugf("[State] %-12s->%-12s", stateMap[atomic.LoadUint32(&curState)], stateMap[stateReady])
					} else if curState == stateReady && pktType == packetTypePayload {
						atomic.StoreUint32(&conn.state, stateStart)
						log.Debugf("[State] %-12s->%-12s", stateMap[atomic.LoadUint32(&curState)], stateMap[stateStart])
						sendChan <- PacketInfo{pktType: packetTypeSignalStart, data: []byte{}, padLen: maxPacketPaddingLength}
					} else  if curState == statePadding {
						if int(curNSeg) % conn.nSeg == 0 {
							log.Debugf("[Event] current nseg is %v", curNSeg)
							atomic.StoreUint32(&conn.state, stateStop)
							log.Debugf("[State] %-12s->%-12s", stateMap[atomic.LoadUint32(&curState)], stateMap[stateStop])
							sendChan <- PacketInfo{pktType: packetTypeSignalStop, data: []byte{}, padLen: maxPacketPaddingLength}
							atomic.StoreUint32(&curNSeg, 0)
						}
					}
				}
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
				curState := atomic.LoadUint32(&conn.state)
				//log.Debugf("[Event] NRealSeg %v at %v", realNSeg, time.Now().Format("15:04:05.000000"))
				if !conn.isServer && curState != stateStop && atomic.LoadUint32(&realNSeg) < 2 {
					// if throughput is small, change client's state:
					// stateReady -> stateStop
					// stateStart -> statePadding
					if curState == stateReady {
						atomic.StoreUint32(&conn.state, stateStop)
						log.Debugf("[State] %-12s->%-12s", stateMap[curState], stateMap[stateStop])
					} else if curState == stateStart {
						atomic.StoreUint32(&conn.state, statePadding)
						log.Debugf("[State] %-12s->%-12s", stateMap[curState], stateMap[statePadding])
					}
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
				log.Infof("Exit by read err:%v", err)
				return written, err
			}
			if rdLen > 0 {
				receiveBuf.Write(buf[: rdLen])
			} else {
				log.Errorf("BUG? read 0 bytes, err: %v", err)
				return written, io.EOF
			}

			for receiveBuf.Len() > 0 {
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

func (conn *tamarawConn) SetDeadline(t time.Time) error {
	return syscall.ENOTSUP
}

func (conn *tamarawConn) SetWriteDeadline(t time.Time) error {
	return syscall.ENOTSUP
}

func (conn *tamarawConn) closeAfterDelay(sf *tamarawServerFactory, startTime time.Time) {
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



var _ base.ClientFactory = (*tamarawClientFactory)(nil)
var _ base.ServerFactory = (*tamarawServerFactory)(nil)
var _ base.Transport = (*Transport)(nil)
var _ net.Conn = (*tamarawConn)(nil)
