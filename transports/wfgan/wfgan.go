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
	pArg          = "p"


	seedLength             = drbg.SeedLength
	headerLength           = framing.FrameOverhead + packetOverhead
	clientHandshakeTimeout = time.Duration(60) * time.Second
	serverHandshakeTimeout = time.Duration(30) * time.Second
	replayTTL              = time.Duration(3) * time.Hour

	maxCloseDelay      = 60
	tWindow            = 4000 * time.Millisecond
	maxQueueSize       = 1000 * 3

	gRPCAddr           = "localhost:9999"
	o2oRelPath         = "../transports/wfgan/grpc/py/training_0826_195223/time_feature_0-100x0-1000_o2o.ipt"  //relative to wfdef/obfs4proxy
	o2iRelPath         = "../transports/wfgan/grpc/py/training_0826_195223/time_feature_0-100x0-1000_o2i.ipt"
	o2iEnabled         = false
	logEnabled         = true

	tmpRho             = 100 // ms, 98%
)

type wfganClientArgs struct {
	nodeID     *ntor.NodeID
	publicKey  *ntor.PublicKey
	sessionKey *ntor.Keypair
	tol        float32
	p          float32
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
	ptArgs.Add(pArg, strconv.FormatFloat(float64(st.p), 'f', -1, 32))

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

	//read in the ipt file
	var iptList []float64
	if o2iEnabled {
		parPath, _ := path.Split(os.Args[0])
		iptList = utils.ReadFloatFromFile(path.Join(parPath, o2iRelPath))
	} else {
		log.Infof("O2I is not loaded.")
		iptList = []float64{}
	}

	sf := &wfganServerFactory{t, &ptArgs, st.nodeID, st.identityKey, st.drbgSeed, st.tol, st.p,&iptList, filter, rng.Intn(maxCloseDelay)}
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
		return nil, fmt.Errorf("malformed tol '%s'", tolStr)
	}

	pStr, pOk := args.Get(pArg)
	if !pOk {
		return nil, fmt.Errorf("missing argument '%s'", pArg)
	}

	p, err := strconv.ParseFloat(pStr, 32)
	if err != nil {
		return nil, fmt.Errorf("malformed p '%s'", pStr)
	}

	// Generate the session key pair before connectiong to hide the Elligator2
	// rejection sampling from network observers.
	sessionKey, err := ntor.NewKeypair(true)
	if err != nil {
		return nil, err
	}

	return &wfganClientArgs{nodeID, publicKey, sessionKey, float32(tol), float32(p)}, nil
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
	p            float32
	iptList      *[]float64

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
	canSendChan := make(chan uint32, 100)  // just to make sure that this channel wont be blocked

	lenDist := probdist.New(sf.lenSeed, 0, framing.MaximumSegmentLength, false)

	//read in the ipt file
	iptList := sf.iptList

	// The server's initial state is intentionally set to stateStart at the very beginning to obfuscate the RTT between client and server
	c := &wfganConn{conn, true, lenDist, sf.tol, float64(sf.p), stateStop, 0, 0,canSendChan, bytes.NewBuffer(nil), bytes.NewBuffer(nil), make([]byte, consumeReadSize), iptList, nil, nil}
	log.Debugf("Server pt con status: isServer: %v, tol: %.1f, p: %.1f", c.isServer, c.tol, c.p)
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
	p         float64
	state     uint32
	nRealSegSent  uint32 // real packet counter over tWindow second
	nRealSegRcv   uint32

	//use to receive the signal from client how much bytes to send
	//zero means defense off, positive numbers means defense on and the bytes required
	//used on serverside
	canSendChan          chan uint32

	receiveBuffer        *bytes.Buffer
	receiveDecodedBuffer *bytes.Buffer
	readBuffer           []byte
	iptList              *[]float64

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
	iptList := utils.ReadFloatFromFile(path.Join(parPath, o2oRelPath))

	// Allocate the client structure.
	c = &wfganConn{conn, false, lenDist, args.tol, float64(args.p), stateStop, 0, 0,nil, bytes.NewBuffer(nil), bytes.NewBuffer(nil), make([]byte, consumeReadSize), &iptList, nil, nil}
	log.Debugf("Client pt con status: isServer: %v, tol: %.1f, p: %.1f", c.isServer, c.tol, c.p)
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
	if conn.isServer {
		return conn.ReadFromServer(r)
	} else {
		return conn.ReadFromClient(r)
	}
}

func (conn *wfganConn) ReadFromServer(r io.Reader) (written int64, err error) {
	log.Infof("[State] Enter copyloop state: %v", stateMap[conn.state])
	closeChan := make(chan int)
	defer close(closeChan)

	errChan := make(chan error, 5)  // errors from all the go routines will be sent to this channel
	sendChan := make(chan PacketInfo, 10000) // all packed packets are sent through this channel

	var receiveBuf utils.SafeBuffer //read payload from upstream and buffer here

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
				//log.Debugf("[Send] %-8s, %-3d+ %-3d bytes at %v", pktTypeMap[pktType], len(data), padLen, time.Now().Format("15:04:05.000000"))
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
					log.Errorf("[Routine] Exit by read err:%v", err)
					errChan <- err
					return
				}
				if rdLen > 0 {
					_, werr := receiveBuf.Write(buf[: rdLen])
					if werr != nil {
						errChan <- werr
						log.Errorf("[Routine] Exit by write err:%v", err)
						return
					}
				} else {
					log.Errorf("[Routine] BUG? read 0 bytes, err: %v", err)
					errChan <- io.EOF
					return
				}
			}
		}
	}()

	//go routine to detect on/off
	go func() {
		for{
			select{
			case _, ok := <-closeChan:
				if !ok {
					log.Infof("[Routine] Detect on/off routine exits by closedChan.")
					return
				}
			default:
				if atomic.LoadUint32(&conn.state) == stateStop {
					conn.canSendChan <- 0
				}
				time.Sleep(50 * time.Millisecond)
			}
		}
	}()

	for {
		select {
		case conErr := <- errChan:
			log.Infof("downstream copy loop terminated at %v. Reason: %v", time.Now().Format("15:04:05.000000"), conErr)
			return written, conErr
		case signalByteNum := <- conn.canSendChan:
			if signalByteNum == 0 {
				//defense off
				writtenTmp, werr := conn.sendRealBurst(&receiveBuf, sendChan)
				written += writtenTmp
				if werr != nil {
					return written, werr
				}
			} else {
				//defense on
				log.Debugf("[DEBUG] p value: %v", conn.p)
				skipRespond := utils.Bernoulli(conn.p)
				if receiveBuf.GetLen() == 0 && skipRespond == 1 {
					log.Infof("[Event] No data in buffer and get 1, skip this response.")
					continue
				}
				ipt := conn.sampleIPT()
				log.Debugf("[Event] Should sleep %v at %v", ipt, time.Now().Format("15:04:05.000000"))
				utils.SleepRho(time.Now(), ipt)
				//log.Debugf("[Event] Finish sleep at %v", time.Now().Format("15:04:05.000000"))

				writtenTmp := conn.sendRefBurst(signalByteNum, &receiveBuf, sendChan)
				written += writtenTmp
			}
		}
	}
}

func (conn *wfganConn) ReadFromClient(r io.Reader) (written int64, err error) {
	log.Infof("[State] Enter copyloop state: %v", stateMap[conn.state])
	closeChan := make(chan int)
	defer close(closeChan)

	errChan := make(chan error, 5)  // errors from all the go routines will be sent to this channel
	sendChan := make(chan PacketInfo, 10000) // all packed packets are sent through this channel
	refillChan := make(chan bool, 1000) // signal gRPC to refill the burst sequence queue
	
	var receiveBuf utils.SafeBuffer //read payload from upstream and buffer here
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
					log.Infof("[Routine] Send routine exits by err.")
					return
				}
				_, wtErr := conn.Conn.Write(frameBuf.Bytes())
				if wtErr != nil {
					errChan <- wtErr
					log.Infof("[Routine] Send routine exits by write err.")
					return
				}
				if logEnabled && pktType != packetTypeFinish {
					// since it is very trivial to remove the finish packet for an attacker
					// (i.e., the last packet of each burst), there is no need to log this packet
					log.Infof("[TRACE_LOG] %d %d %d", time.Now().UnixNano(), int64(len(data)), int64(padLen))
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
			case <- refillChan:
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				grpcConn, gErr := grpc.DialContext(ctx, gRPCAddr, grpc.WithInsecure(), grpc.WithBlock())
				cancel()
				if gErr != nil {
					log.Errorf("[gRPC] Cannot connect to py server. Exit the program.")
					errChan <- gErr
					return
				}
				client := pb.NewGenerateTraceClient(grpcConn)
				//log.Debugf("[gRPC] Succeed to connect to py server.")
				req := &pb.GANRequest{Ask: 1}
				resp, rErr := client.Query(context.Background(), req)
				if rErr!= nil{
					log.Errorf("[gRPC] Error in request %v",err)
				}
				_  = grpcConn.Close()
				log.Debugf("[gRPC] Before: Refill queue (size %v) with %v elements at %v", burstQueue.GetLen(), len(resp.Packets)/2, time.Now().Format("15:04:05.000000"))
				for i := 0; i < len(resp.Packets) - 1; i += 2 {
					qerr := burstQueue.Enqueue(rrTuple{request: resp.Packets[i], response: resp.Packets[i+1]})
					if qerr != nil {
						log.Errorf("[gRPC] Error happened when enqueue: %v", qerr)
						break
					}
				}
				log.Debugf("[gRPC] After: Refilled queue (size %v) with %v elements at %v", burstQueue.GetLen(), len(resp.Packets)/2, time.Now().Format("15:04:05.000000"))
			default:
				//client, defense on
				// if the capacity of burstQueue is small, refill the queue
				capacity := float64(burstQueue.GetLen()) / float64(maxQueueSize)
				if !conn.isServer && capacity < 0.1 {
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
				log.Infof("[State] Real Sent: %v, Real Receive: %v, curState: %s at %v.", conn.nRealSegSent, conn.nRealSegRcv, stateMap[atomic.LoadUint32(&conn.state)], time.Now().Format("15:04:05.000000"))
				if atomic.LoadUint32(&conn.state) != stateStop {
					if atomic.LoadUint32(&conn.nRealSegSent) < 2 ||  atomic.LoadUint32(&conn.nRealSegRcv) < 2{
						log.Infof("[State] Real Sent: %v, Real Receive: %v, %s -> %s at %v.", conn.nRealSegSent, conn.nRealSegRcv, stateMap[atomic.LoadUint32(&conn.state)], stateMap[stateStop], time.Now().Format("15:04:05.000000"))
						atomic.StoreUint32(&conn.state, stateStop)
						sendChan <- PacketInfo{pktType: packetTypeSignalStop, data: []byte{}, padLen: maxPacketPaddingLength}
					}
				}
				atomic.StoreUint32(&conn.nRealSegSent, 0) //reset counter
				atomic.StoreUint32(&conn.nRealSegRcv, 0) //reset counter
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
				rdLen, rErr := r.Read(buf[:])
				if rErr!= nil {
					log.Errorf("Exit by read err:%v", rErr)
					errChan <- rErr
					return
				}
				if rdLen > 0 {
					_, werr := receiveBuf.Write(buf[: rdLen])
					if werr != nil {
						errChan <- werr
						return
					}
					//signal server to start if there is more than one cell coming
					// else switch to padding state
					// stop -> ready -> start
					if (atomic.LoadUint32(&conn.state) == stateStop && rdLen > maxPacketPayloadLength) ||
						(atomic.LoadUint32(&conn.state) == stateReady) {
						// stateStop with >2 cells -> stateStart
						// or stateReady with >0 cell -> stateStart
						log.Infof("[State] Got %v bytes upstream, %s -> %s.", rdLen, stateMap[atomic.LoadUint32(&conn.state)], stateMap[stateStart])
						atomic.StoreUint32(&conn.state, stateStart)
						sendChan <- PacketInfo{pktType: packetTypeSignalStart, data: []byte{}, padLen: maxPacketPaddingLength}
					} else if atomic.LoadUint32(&conn.state) == stateStop {
						log.Infof("[State] Got %v bytes upstream, %s -> %s.", rdLen, stateMap[stateStop], stateMap[stateReady])
						atomic.StoreUint32(&conn.state, stateReady)
					}
				} else {
					log.Errorf("BUG? read 0 bytes, err: %v", err)
					errChan <- io.EOF
					return
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
			if atomic.LoadUint32(&conn.state) == stateStart {
				//defense on, client: sample an ipt and send out a burst
				ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
				burstTuple, qerr := burstQueue.DequeueOrWaitForNextElementContext(ctx)
				cancel()
				if qerr != nil {
					log.Infof("The queue is empty for 0.5 second, something wrong happened? Try again.")
					break
				}
				log.Debugf("[Event] Sample a burst tuple: %v", burstTuple)
				requestSize := burstTuple.(rrTuple).request
				responseSize := burstTuple.(rrTuple).response
				writtenTmp := conn.sendRefBurst(uint32(requestSize), &receiveBuf, sendChan)
				written += writtenTmp
				//send a finish signal
				var payload [4]byte
				binary.BigEndian.PutUint32(payload[:], uint32(responseSize))
				sendChan <- PacketInfo{pktType: packetTypeFinish, data: payload[:], padLen: uint16(maxPacketPaddingLength-4)}
				log.Debugf("[ON] Response size %v", responseSize)

				ipt := conn.sampleIPT()
				log.Debugf("[Event] Should sleep %v at %v", ipt, time.Now().Format("15:04:05.000000"))
				utils.SleepRho(time.Now(), ipt)
				//log.Debugf("[Event] Finish sleep at %v", time.Now().Format("15:04:05.000000"))

			} else {
				//defense off (in stop or ready)
				writtenTmp, werr := conn.sendRealBurst(&receiveBuf, sendChan)
				written += writtenTmp
				if werr != nil {
					return written, werr
				}
				time.Sleep(50 * time.Millisecond) //avoid infinite loop
			}
		}
	}
}


func (conn *wfganConn) sampleIPT() time.Duration {
	//// Fixed time gap code
	//if conn.isServer {
	//	return 0 * time.Millisecond
	//} else {
	//	return tmpRho * time.Millisecond
	//}
	var ipt float64
	if len(*conn.iptList) == 0 {
		//log.Debugf("iptList is not given, return 0.")
		ipt = 0
	} else {
		ipt = utils.SampleIPT(*conn.iptList)
	}
	if ipt > tmpRho {
		ipt = tmpRho
	}
	return time.Duration(ipt) * time.Millisecond
}

func (conn *wfganConn) sendRefBurst(refBurstSize uint32, receiveBuf *utils.SafeBuffer, sendChan chan PacketInfo) (written int64) {
	lowerBound := utils.IntMax(int(math.Round(float64(refBurstSize) * float64(1 - conn.tol))), 536)
	upperBound := int(math.Round(float64(refBurstSize) * float64(1 + conn.tol)))

	var toSend int
	bufSize := receiveBuf.GetLen()
	if bufSize < lowerBound {
		toSend = lowerBound
	} else if bufSize < upperBound {
		toSend = bufSize
	} else {
		toSend = upperBound
	}

	log.Debugf("[ON] Ref: %v bytes, lower: %v bytes, upper: %v bytes, bufSize: %v, toSend: %v bytes at %v", refBurstSize, lowerBound, upperBound, bufSize, toSend, time.Now().Format("15:04:05.000000"))
	for toSend >= maxPacketPayloadLength {
		var payload [maxPacketPayloadLength]byte
		rdLen, _ := receiveBuf.Read(payload[:])
		written += int64(rdLen)
		var pktType uint8
		if rdLen > 0{
			pktType = packetTypePayload
			if !conn.isServer {
				atomic.AddUint32(&conn.nRealSegSent, 1)
			}
		} else {
			// no data, send out a dummy packet
			pktType = packetTypeDummy
		}
		sendChan <- PacketInfo{pktType: pktType, data: payload[:rdLen], padLen: uint16(maxPacketPaddingLength-rdLen)}
		toSend -= maxPacketPayloadLength
	}
	return written
}

func (conn *wfganConn) sendRealBurst(receiveBuf *utils.SafeBuffer, sendChan chan PacketInfo) (written int64, err error) {
	if size:= receiveBuf.GetLen(); size > 0 {
		log.Debugf("[OFF] Send %v bytes at %v", size, time.Now().Format("15:04:05.000000"))
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
		if !conn.isServer {
			atomic.AddUint32(&conn.nRealSegSent, 1)
		}
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