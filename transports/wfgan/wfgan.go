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


package wfgan

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	pt "git.torproject.org/pluggable-transports/goptlib.git"
	queue "github.com/enriquebris/goconcurrentqueue"
	"github.com/websitefingerprinting/wfdef.git/common/log"
	"github.com/websitefingerprinting/wfdef.git/common/ntor"
	"github.com/websitefingerprinting/wfdef.git/common/probdist"
	"github.com/websitefingerprinting/wfdef.git/common/replayfilter"
	"github.com/websitefingerprinting/wfdef.git/common/utils"
	"github.com/websitefingerprinting/wfdef.git/transports/base"
	"github.com/websitefingerprinting/wfdef.git/transports/wfgan/grpc/pb"
	"google.golang.org/grpc"
	"io"
	"io/ioutil"
	"math"
	"math/rand"
	"net"
	"os"
	"path"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/websitefingerprinting/wfdef.git/common/drbg"
	"github.com/websitefingerprinting/wfdef.git/transports/wfgan/framing"
)

const (
	transportName = "wfgan"

	nodeIDArg     = "node-id"
	publicKeyArg  = "public-key"
	privateKeyArg = "private-key"
	seedArg       = "drbg-seed"
	certArg       = "cert"
	tolArg        = "tol"


	seedLength             = drbg.SeedLength
	headerLength           = framing.FrameOverhead + packetOverhead
	clientHandshakeTimeout = time.Duration(60) * time.Second
	serverHandshakeTimeout = time.Duration(30) * time.Second
	replayTTL              = time.Duration(3) * time.Hour

	maxCloseDelay      = 60
	tWindow            = 5000 * time.Millisecond
	maxQueueSize       = 1000 * 3

	gRPCAddr           = "localhost:9999"
	iptRelPath         = "../transports/wfgan/grpc/time_feature_0-100x0-1000_o2o.ipt"  //relative to wfdef/obfs4proxy

	logEnabled         = true
)

type wfganClientArgs struct {
	nodeID     *ntor.NodeID
	publicKey  *ntor.PublicKey
	sessionKey *ntor.Keypair
	tol        float32
}

// Transport is the wfgan implementation of the base.Transport interface.
type Transport struct{}

// Name returns the name of the wfgan transport protocol.
func (t *Transport) Name() string {
	return transportName
}

// ClientFactory returns a new wfganClientFactory instance.
func (t *Transport) ClientFactory(stateDir string) (base.ClientFactory, error) {
	cf := &wfganClientFactory{transport: t}
	return cf, nil
}

// ServerFactory returns a new wfganServerFactory instance.
func (t *Transport) ServerFactory(stateDir string, args *pt.Args) (base.ServerFactory, error) {
	st, err := serverStateFromArgs(stateDir, args)
	if err != nil {
		return nil, err
	}


	// Store the arguments that should appear in our descriptor for the clients.
	ptArgs := pt.Args{}
	ptArgs.Add(certArg, st.cert.String())
	ptArgs.Add(tolArg, strconv.FormatFloat(float64(st.tol), 'f', -1, 32))

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

	sf := &wfganServerFactory{t, &ptArgs, st.nodeID, st.identityKey, st.drbgSeed, st.tol, filter, rng.Intn(maxCloseDelay)}
	return sf, nil
}

type wfganClientFactory struct {
	transport base.Transport
}

func (cf *wfganClientFactory) Transport() base.Transport {
	return cf.transport
}

func (cf *wfganClientFactory) ParseArgs(args *pt.Args) (interface{}, error) {
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


	tolStr, tolOk := args.Get(tolArg)
	if !tolOk {
		return nil, fmt.Errorf("missing argument '%s'", tolArg)
	}
	
	tol, err := strconv.ParseFloat(tolStr, 32)
	if err != nil {
		return nil, fmt.Errorf("malformed w-min '%s'", tolStr)
	}
	

	// Generate the session key pair before connectiong to hide the Elligator2
	// rejection sampling from network observers.
	sessionKey, err := ntor.NewKeypair(true)
	if err != nil {
		return nil, err
	}

	return &wfganClientArgs{nodeID, publicKey, sessionKey, float32(tol)}, nil
}

func (cf *wfganClientFactory) Dial(network, addr string, dialFn base.DialFunc, args interface{}) (net.Conn, error) {
	// Validate args before bothering to open connection.
	ca, ok := args.(*wfganClientArgs)
	if !ok {
		return nil, fmt.Errorf("invalid argument type for args")
	}
	conn, err := dialFn(network, addr)
	if err != nil {
		return nil, err
	}
	dialConn := conn
	if conn, err = newWfganClientConn(conn, ca); err != nil {
		dialConn.Close()
		return nil, err
	}
	return conn, nil
}

type wfganServerFactory struct {
	transport base.Transport
	args      *pt.Args

	nodeID       *ntor.NodeID
	identityKey  *ntor.Keypair
	lenSeed      *drbg.Seed
	
	tol          float32

	replayFilter *replayfilter.ReplayFilter
	closeDelay int
}

func (sf *wfganServerFactory) Transport() base.Transport {
	return sf.transport
}

func (sf *wfganServerFactory) Args() *pt.Args {
	return sf.args
}

func (sf *wfganServerFactory) WrapConn(conn net.Conn) (net.Conn, error) {
	// Not much point in having a separate newwfganServerConn routine when
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
	canSendChan := make(chan uint32, 10)  // just to make sure that this channel wont be blocked

	lenDist := probdist.New(sf.lenSeed, 0, framing.MaximumSegmentLength, false)
	// The server's initial state is intentionally set to stateStart at the very beginning to obfuscate the RTT between client and server
	c := &wfganConn{conn, true, lenDist, sf.tol, stateStop, canSendChan, bytes.NewBuffer(nil), bytes.NewBuffer(nil), make([]byte, consumeReadSize), nil, nil, nil}
	log.Debugf("Server pt con status: isServer: %v, tol: %.1f", c.isServer, c.tol)
	startTime := time.Now()

	if err = c.serverHandshake(sf, sessionKey); err != nil {
		log.Errorf("Handshake err %v", err)
		c.closeAfterDelay(sf, startTime)
		return nil, err
	}

	return c, nil
}


type wfganConn struct {
	net.Conn

	isServer  bool

	lenDist   *probdist.WeightedDist
	tol       float32
	state     uint32

	canSendChan          chan uint32 //  used on server side
	receiveBuffer        *bytes.Buffer
	receiveDecodedBuffer *bytes.Buffer
	readBuffer           []byte
	iptList              []float64

	encoder *framing.Encoder
	decoder *framing.Decoder
}

type rrTuple struct {
	request  int32
	response int32
}

func newWfganClientConn(conn net.Conn, args *wfganClientArgs) (c *wfganConn, err error) {
	// Generate the initial protocol polymorphism distribution(s).
	var seed *drbg.Seed
	if seed, err = drbg.NewSeed(); err != nil {
		return
	}
	lenDist := probdist.New(seed, 0, framing.MaximumSegmentLength, false)

	//read in the ipt file

	parPath, _ := path.Split(os.Args[0])
	iptList := utils.ReadFloatFromFile(path.Join(parPath, iptRelPath))


	// Allocate the client structure.
	c = &wfganConn{conn, false, lenDist, args.tol, stateStop, nil, bytes.NewBuffer(nil), bytes.NewBuffer(nil), make([]byte, consumeReadSize), iptList, nil, nil}
	log.Debugf("Client pt con status: isServer: %v, tol: %.2f", c.isServer, c.tol)
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

func (conn *wfganConn) clientHandshake(nodeID *ntor.NodeID, peerIdentityKey *ntor.PublicKey, sessionKey *ntor.Keypair) error {
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

func (conn *wfganConn) serverHandshake(sf *wfganServerFactory, sessionKey *ntor.Keypair) error {
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

func (conn *wfganConn) Read(b []byte) (n int, err error) {
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


func (conn *wfganConn) ReadFrom(r io.Reader) (written int64, err error) {
	log.Infof("[State] Enter copyloop state: %v", stateMap[conn.state])
	closeChan := make(chan int)
	defer close(closeChan)

	errChan := make(chan error, 5)  // errors from all the go routines will be sent to this channel
	sendChan := make(chan PacketInfo, 10000) // all packed packets are sent through this channel
	refillChan := make(chan bool, 1000) // signal gRPC to refill the burst sequence queue

	var realNSeg uint32 = 0  // real packet counter over tWindow second
	var receiveBuf bytes.Buffer //read payload from upstream and buffer here
	var burstQueue = queue.NewFixedFIFO(maxQueueSize)// maintain a queue of burst seqs

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
					return
				}
				capacity, _ := utils.EstimateTCPCapacity(conn.Conn)
				log.Debugf("The current tcp capacity is %v at %v", capacity, time.Now().Format("15:04:05.000000"))
				_, wtErr := conn.Conn.Write(frameBuf.Bytes())
				if wtErr != nil {
					errChan <- wtErr
					log.Infof("[Routine] Send routine exits by write err.")
					return
				}
				if !conn.isServer && logEnabled && pktType != packetTypeFinish {
					// since it is very trivial to remove the finish packet for an attacker
					// (i.e., the last packet of each burst), there is no need to log this packet
					log.Infof("[TRACE_LOG] %d %d %d", time.Now().UnixNano(), int64(len(data)), int64(padLen))
				} else if conn.isServer {
					log.Debugf("[Send] %-8s, %-3d+ %-3d bytes at %v", pktTypeMap[pktType], len(data), padLen, time.Now().Format("15:04:05.000000"))
				}
			}
		}
	}()

	//create a go routine to maintain burst sequence queue
	//true: need to refill the channel
	//false: need to dequeue the channel
	go func() {
		for{
			select{
			case _, ok := <- closeChan:
				if !ok{
					log.Infof("[Routine] padding factory exits by closedChan.")
					return
				}
			case shouldRefill := <- refillChan:
				if shouldRefill {
					ctx, _ := context.WithTimeout(context.Background(), 5*time.Second)
					conn, err := grpc.DialContext(ctx, gRPCAddr, grpc.WithInsecure(), grpc.WithBlock())
					if err != nil {
						log.Errorf("[gRPC] Cannot connect to py server. Exit the program.")
						errChan <- err
						return
					}
					client := pb.NewGenerateTraceClient(conn)
					//log.Debugf("[gRPC] Succeed to connect to py server.")
					req := &pb.GANRequest{Ask: 1}
					resp, err := client.Query(context.Background(), req)
					if err!= nil{
						log.Errorf("[gRPC] Error in request %v",err)
					}
					_  = conn.Close()
					log.Debugf("[gRPC] Before: Refill queue (size %v) with %v elements at %v", burstQueue.GetLen(), len(resp.Packets)/2, time.Now().Format("15:04:05.000000"))
					for i := 0; i < len(resp.Packets) - 1; i += 2 {
						qerr := burstQueue.Enqueue(rrTuple{request: resp.Packets[i], response: resp.Packets[i+1]})
						if qerr != nil {
							log.Errorf("[gRPC] Error happened when enqueue: %v", qerr)
							break
						}
					}
					log.Debugf("[gRPC] After: Refilled queue (size %v) with %v elements at %v", burstQueue.GetLen(), len(resp.Packets)/2, time.Now().Format("15:04:05.000000"))
				} else {
					if !conn.isServer && atomic.LoadUint32(&conn.state) != stateStart {
						log.Debugf("[Event] Empty the queue (len %v)", burstQueue.GetLen())
						for {
							_, qerr := burstQueue.Dequeue()
							if qerr != nil {
								break
							}
						}
					}
				}
			default:
				//client, defense on
				// if the capacity of burstQueue is small, refill the queue
				capacity := float64(burstQueue.GetLen()) / float64(maxQueueSize)
				if !conn.isServer && atomic.LoadUint32(&conn.state) == stateStart && capacity < 0.1 {
					log.Debugf("[Event] Low queue capacity %.2f, triggering refill event at %v", capacity, time.Now().Format("15:04:05.000000"))
					refillChan <- true
				}
				time.Sleep(50 * time.Millisecond)
			}
		}
	}()


	// this go routine regularly check the real throughput
	// if it is small, change to stop state
	go func() {
		ticker := time.NewTicker(tWindow)
		defer ticker.Stop()
		for{
			select{
			case _, ok := <- closeChan:
				if !ok {
					log.Infof("[Routine] Ticker routine exits by closeChan.")
					return
				}
			case <- ticker.C:
				if !conn.isServer {
					log.Debugf("[Event] NRealSeg %v at %v", realNSeg, time.Now().Format("15:04:05.000000"))
				}
				if !conn.isServer && atomic.LoadUint32(&conn.state) != stateStop && atomic.LoadUint32(&realNSeg) < 2 {
					log.Infof("[State] %s -> %s at %v.", stateMap[atomic.LoadUint32(&conn.state)], stateMap[stateStop], time.Now().Format("15:04:05.000000"))
					atomic.StoreUint32(&conn.state, stateStop)
					sendChan <- PacketInfo{pktType: packetTypeSignalStop, data: []byte{}, padLen: maxPacketPaddingLength}
					refillChan <- false //empty the queue since the defense will be turned off
				}
				atomic.StoreUint32(&realNSeg, 0) //reset counter
			}
		}
	}()

	// go routine to receive data from upperstream
	go func() {
		for {
			select {
			case _, ok := <-closeChan:
				if !ok {
					log.Infof("[Routine] Send routine exits by closedChan.")
					return
				}
			default:
				buf := make([]byte, 65535)
				rdLen, err := r.Read(buf[:])
				if err!= nil {
					log.Errorf("Exit by read err:%v", err)
					errChan <- err
				}
				if rdLen > 0 {
					receiveBuf.Write(buf[: rdLen])
				} else {
					log.Errorf("BUG? read 0 bytes, err: %v", err)
					errChan <- io.EOF
				}
			}
		}
	}()

	for {
		select {
		case conErr := <- errChan:
			log.Infof("downstream copy loop terminated at %v. Reason: %v", time.Now().Format("15:04:05.000000"), conErr)
			return written, conErr
		default:
			bufSize := receiveBuf.Len()

			//signal server to start if there is more than one cell coming
			// else switch to padding state
			// stop -> ready -> start
			if !conn.isServer && bufSize > 0{
				if (atomic.LoadUint32(&conn.state) == stateStop && bufSize > maxPacketPayloadLength) ||
					(atomic.LoadUint32(&conn.state) == stateReady) {
					// stateStop with >2 cells -> stateStart
					// or stateReady with >0 cell -> stateStart
					log.Infof("[State] %s -> %s.", stateMap[atomic.LoadUint32(&conn.state)], stateMap[stateStart])
					atomic.StoreUint32(&conn.state, stateStart)
					sendChan <- PacketInfo{pktType: packetTypeSignalStart, data: []byte{}, padLen: maxPacketPaddingLength}
					refillChan <- true
				} else if atomic.LoadUint32(&conn.state) == stateStop {
					log.Infof("[State] %s -> %s.", stateMap[stateStop], stateMap[stateReady])
					atomic.StoreUint32(&conn.state, stateReady)
				}
			}

			if atomic.LoadUint32(&conn.state) == stateStart {
				//defense on, client: sample a ipt and send out a burst
				//server: wait for the signal from canSendChan
				if conn.isServer {
					refBurstSize := <- conn.canSendChan
					realNSegTmp, writtenTmp := sendRefBurst(refBurstSize, conn.tol, &receiveBuf, sendChan)
					atomic.AddUint32(&realNSeg, realNSegTmp)
					written += writtenTmp
				} else {
					ipt, err := conn.sampleIPT()
					if err != nil {
						log.Errorf("Error when sampling ipt: %v", err)
						errChan <- err
					}
					log.Debugf("[Event] Should sleep %v at %v", ipt, time.Now().Format("15:04:05.000000"))
					utils.SleepRho(time.Now(), ipt)
					log.Debugf("[Event] Finish sleep at %v", time.Now().Format("15:04:05.000000"))

					burstTuple, qerr := burstQueue.Dequeue()
					if qerr != nil {
						log.Warnf("[Event] Potential data race in the main loop: %v", qerr)
						time.Sleep(50 * time.Millisecond)
						break
					}
					log.Debugf("[Event] Sample a burst tuple: %v", burstTuple)
					requestSize := burstTuple.(rrTuple).request
					responseSize := burstTuple.(rrTuple).response
					realNSegTmp, writtenTmp := sendRefBurst(uint32(requestSize), conn.tol, &receiveBuf, sendChan)
					atomic.AddUint32(&realNSeg, realNSegTmp)
					written += writtenTmp
					//send a finish signal
					var payload [4]byte
					binary.BigEndian.PutUint32(payload[:], uint32(responseSize))
					sendChan <- PacketInfo{pktType: packetTypeFinish, data: payload[:], padLen: uint16(maxPacketPaddingLength-4)}
					log.Debugf("[ON] Response size %v", responseSize)
				}
			} else {
				//defense off (in stop or ready)
				realNSegTmp, writtenTmp, werr := sendRealBurst(&receiveBuf, sendChan)
				atomic.AddUint32(&realNSeg, realNSegTmp)
				written += writtenTmp
				if werr != nil {
					return written, werr
				}
				time.Sleep(50 * time.Millisecond) //avoid infinite loop
			}
		}
	}
}

func (conn *wfganConn) sampleIPT() (ipt time.Duration, err error) {
	if len(conn.iptList) == 0 {
		return time.Duration(0), errors.New("the ipt list is empty or nil")
	}
	return time.Duration(utils.SampleIPT(conn.iptList)) * time.Millisecond, nil
}

func sendRefBurst(refBurstSize uint32, tol float32, receiveBuf *bytes.Buffer, sendChan chan PacketInfo) (realNSeg uint32, written int64) {
	lowerBound := utils.IntMax(int(math.Round(float64(refBurstSize) * float64(1 - tol))), 536)
	upperBound := int(math.Round(float64(refBurstSize) * float64(1 + tol)))

	var toSend int
	bufSize := receiveBuf.Len()
	if bufSize < lowerBound {
		toSend = lowerBound
	} else if bufSize < upperBound {
		toSend = bufSize
	} else {
		toSend = upperBound
	}
	log.Debugf("[ON] Ref: %v bytes, lower: %v bytes, upper: %v bytes, bufSize: %v, toSend: %v bytes", refBurstSize, lowerBound, upperBound, bufSize, toSend)
	for toSend >= maxPacketPayloadLength {
		var payload [maxPacketPayloadLength]byte
		rdLen, _ := receiveBuf.Read(payload[:])
		written += int64(rdLen)
		var pktType uint8
		if rdLen > 0{
			pktType = packetTypePayload
			realNSeg += 1
		} else {
			// no data, send out a dummy packet
			pktType = packetTypeDummy
		}
		sendChan <- PacketInfo{pktType: pktType, data: payload[:rdLen], padLen: uint16(maxPacketPaddingLength-rdLen)}
		toSend -= maxPacketPayloadLength
	}
	log.Debugf("[ON] Real NSeg Number: %v", realNSeg)
	return realNSeg, written
}

func sendRealBurst(receiveBuf *bytes.Buffer, sendChan chan PacketInfo) (realNSeg uint32, written int64, err error) {
	if receiveBuf.Len() > 0 {
		log.Debugf("[OFF] Send %v bytes at %v", receiveBuf.Len(), time.Now().Format("15:04:05.000000"))
	}
	for receiveBuf.Len() > 0 {
		var payload [maxPacketPayloadLength]byte
		rdLen, rdErr := receiveBuf.Read(payload[:])
		written += int64(rdLen)
		if rdErr != nil {
			log.Infof("Exit by read buffer err:%v", rdErr)
			return realNSeg, written, rdErr
		}
		sendChan <- PacketInfo{pktType: packetTypePayload, data: payload[:rdLen], padLen: uint16(maxPacketPaddingLength-rdLen)}
		realNSeg += 1
	}
	return
}


func (conn *wfganConn) SetDeadline(t time.Time) error {
	return syscall.ENOTSUP
}

func (conn *wfganConn) SetWriteDeadline(t time.Time) error {
	return syscall.ENOTSUP
}

func (conn *wfganConn) closeAfterDelay(sf *wfganServerFactory, startTime time.Time) {
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



var _ base.ClientFactory = (*wfganClientFactory)(nil)
var _ base.ServerFactory = (*wfganServerFactory)(nil)
var _ base.Transport = (*Transport)(nil)
var _ net.Conn = (*wfganConn)(nil)