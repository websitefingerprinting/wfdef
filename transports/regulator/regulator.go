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

// package regulator provides an implementation of the Tor Project's regulator
// obfuscation protocol.
package regulator // import "github.com/websitefingerprinting/wfdef.git/transports/regulator"

import (
	"bytes"
	"git.torproject.org/pluggable-transports/goptlib.git"
	"github.com/websitefingerprinting/wfdef.git/common/log"
	"github.com/websitefingerprinting/wfdef.git/common/utils"
	"github.com/websitefingerprinting/wfdef.git/transports/base"
	"github.com/websitefingerprinting/wfdef.git/transports/defconn"
	"io"
	"net"
	"sync/atomic"
	"time"
)

const (
	transportName = "regulator"

	rArg = "r"
	dArg = "d"
	tArg = "t"
	nArg = "n"
	uArg = "u"
	cArg = "c"
)

type regulatorClientArgs struct {
	*defconn.DefConnClientArgs
	r int32
	d float32
	t float32
	n int32
	u float32
	c float32
}

// Transport is the regulator implementation of the base.Transport interface.
type Transport struct {
	defconn.Transport
}

// Name returns the name of the regulator transport protocol.
func (t *Transport) Name() string {
	return transportName
}

// ClientFactory returns a new regulatorClientFactory instance.
func (t *Transport) ClientFactory(stateDir string) (base.ClientFactory, error) {
	parentFactory, err := t.Transport.ClientFactory(stateDir)
	return &regulatorClientFactory{
		parentFactory.(*defconn.DefConnClientFactory),
	}, err
}

// ServerFactory returns a new regulatorServerFactory instance.
func (t *Transport) ServerFactory(stateDir string, args *pt.Args) (base.ServerFactory, error) {
	sf, err := t.Transport.ServerFactory(stateDir, args)
	if err != nil {
		return nil, err
	}

	st, err := serverStateFromArgs(stateDir, args)
	if err != nil {
		return nil, err
	}

	regulatorSf := regulatorServerFactory{
		sf.(*defconn.DefConnServerFactory), st.r, st.d, st.t, st.n, st.u, st.c,
	}

	return &regulatorSf, nil
}

type regulatorClientFactory struct {
	*defconn.DefConnClientFactory
}

func (cf *regulatorClientFactory) Transport() base.Transport {
	return cf.DefConnClientFactory.Transport()
}

func (cf *regulatorClientFactory) ParseArgs(args *pt.Args) (interface{}, error) {
	arguments, err := cf.DefConnClientFactory.ParseArgs(args)

	r, err := utils.GetIntArgFromStr(rArg, args)
	if err != nil {
		return nil, err
	}

	d, err := utils.GetFloatArgFromStr(dArg, args)
	if err != nil {
		return nil, err
	}

	t, err := utils.GetFloatArgFromStr(tArg, args)
	if err != nil {
		return nil, err
	}

	n, err := utils.GetIntArgFromStr(nArg, args)
	if err != nil {
		return nil, err
	}

	u, err := utils.GetFloatArgFromStr(uArg, args)
	if err != nil {
		return nil, err
	}

	c, err := utils.GetFloatArgFromStr(cArg, args)
	if err != nil {
		return nil, err
	}

	return &regulatorClientArgs{
		arguments.(*defconn.DefConnClientArgs),
		int32(r.(int)), float32(d.(float64)), float32(t.(float64)), int32(n.(int)),
		float32(u.(float64)), float32(c.(float64)),
	}, nil
}

func (cf *regulatorClientFactory) Dial(network, addr string, dialFn base.DialFunc, args interface{}) (net.Conn, error) {
	defConn, err := cf.DefConnClientFactory.Dial(network, addr, dialFn, args)
	if err != nil {
		return nil, err
	}
	paddingChan := make(chan bool)
	sendChan := make(chan packetInfo, 10000)

	argsT := args.(*regulatorClientArgs)
	c := &regulatorConn{
		defConn.(*defconn.DefConn),
		argsT.r, argsT.d, argsT.t, argsT.n, argsT.u, argsT.c, sendChan, paddingChan,
	}
	return c, nil
}

type regulatorServerFactory struct {
	*defconn.DefConnServerFactory
	r int32
	d float32
	t float32
	n int32
	u float32
	c float32
}

func (sf *regulatorServerFactory) WrapConn(conn net.Conn) (net.Conn, error) {
	defConn, err := sf.DefConnServerFactory.WrapConn(conn)
	if err != nil {
		return nil, err
	}

	paddingChan := make(chan bool)
	sendChan := make(chan packetInfo, 10000)
	c := &regulatorConn{
		defConn.(*defconn.DefConn),
		sf.r, sf.d, sf.t, sf.n, sf.u, sf.c, sendChan, paddingChan,
	}
	return c, nil
}

type regulatorConn struct {
	*defconn.DefConn
	r int32
	d float32
	t float32
	n int32
	u float32
	c float32

	sendChan    chan packetInfo
	paddingChan chan bool // true when start defense, false when stop defense
}

func (conn *regulatorConn) Send() {
	//A dedicated function responsible for sending out packets coming from conn.sendChan
	//Err is propagated via conn.ErrChan
	for {
		select {
		case _, ok := <-conn.CloseChan:
			if !ok {
				log.Infof("[Routine] Send routine exits by closedChan.")
				return
			}
		case packetInfo := <-conn.sendChan:
			pktType := packetInfo.pktType
			rate := packetInfo.rate
			data := packetInfo.data
			padLen := packetInfo.padLen
			var frameBuf bytes.Buffer
			err := conn.MakePacket(&frameBuf, pktType, rate, data, padLen)
			if err != nil {
				conn.ErrChan <- err
				log.Infof("[Routine] Send routine exits by make pkt err.")
				return
			}
			_, wtErr := conn.Conn.Write(frameBuf.Bytes())
			if wtErr != nil {
				conn.ErrChan <- wtErr
				log.Infof("[Routine] Send routine exits by write err.")
				return
			}

			if !conn.IsServer && defconn.LogEnabled {
				log.Infof("[TRACE_LOG] %d %d %d", time.Now().UnixNano(), int64(len(data)), int64(padLen))
			} else {
				log.Debugf("[Send] %-8s, %-3d+ %-3d bytes at %v", defconn.PktTypeMap[pktType], len(data), padLen, time.Now().Format("15:04:05.000"))
			}
		}
	}
}

func (conn *regulatorConn) serverReadFrom(r io.Reader) (written int64, err error) {
	log.Debugf("[State] Regulator Server Enter copyloop state: %v at %v", defconn.StateMap[conn.ConnState.LoadCurState()], time.Now().Format("15:04:05.000"))
	defer close(conn.CloseChan)

	var receiveBuf utils.SafeBuffer
	var sp serverParams

	sp.Init(conn.r, conn.n)
	//create a go routine to send out packets to the wire
	go conn.Send()

	// go routine to receive data from upperstream
	go func() {
		for {
			select {
			case _, ok := <-conn.CloseChan:
				if !ok {
					log.Infof("[Routine] Send routine exits by closedChan.")
					return
				}
			default:
				buf := make([]byte, 65535)
				rdLen, err := r.Read(buf[:])
				log.Debugf("[Event] Read %v bytes from upstream", rdLen)

				if err != nil {
					log.Errorf("Exit by read err:%v", err)
					conn.ErrChan <- err
					return
				}
				if rdLen > 0 {
					_, werr := receiveBuf.Write(buf[:rdLen])
					if werr != nil {
						conn.ErrChan <- werr
						return
					}
				} else {
					log.Errorf("BUG? read 0 bytes, err: %v", err)
					conn.ErrChan <- io.EOF
					return
				}
			}
		}
	}()

	// go routine to initialize the parameters when receive the start signal
	go func() {
		for {
			select {
			case _, ok := <-conn.CloseChan:
				if !ok {
					log.Infof("[Routine] Send routine exits by closedChan.")
					return
				}
			case shouldStart := <-conn.paddingChan:
				if shouldStart {
					// initialize the parameters:
					// 1) padding budget 2) record current timestamp 3) reset sending rate
					sp.Init(conn.r, conn.n)
					log.Debugf("[DEBUG] Current padding budget: %v", sp.GetPaddingBudget())
					log.Debugf("[State] Client signal: %s -> %s.", defconn.StateMap[conn.ConnState.LoadCurState()], defconn.StateMap[defconn.StateStart])
					conn.ConnState.SetState(defconn.StateStart)
				} else {
					if conn.ConnState.LoadCurState() != defconn.StateStop {
						log.Debugf("[State] Client signal: %s -> %s.", defconn.StateMap[conn.ConnState.LoadCurState()], defconn.StateMap[defconn.StateStop])
						conn.ConnState.SetState(defconn.StateStop)
					}
				}
			}
		}
	}()

	// mainloop, when defense is on, server send packets with decayed rate r' (r resumed when hits the threshold)
	lastSend := time.Now() //record the last packet sending time
	for {
		select {
		case conErr := <-conn.ErrChan:
			log.Infof("downstream copy loop terminated at %v. Reason: %v", utils.GetFormattedCurrentTime(), conErr)
			return written, conErr
		default:
			if conn.ConnState.LoadCurState() == defconn.StateStart {
				// defense on
				// compute new sending rate 1) if hit threshold, reset rate 2) else compute the decayed rate
				curTime := time.Now()
				curRate, isChanged := sp.CalTargetRate(curTime, conn.r, conn.d)
				if isChanged {
					log.Infof("[Event] The rate is adjusted to %v at %v", curRate, curTime.Format("15:04:05.000000"))
				}

				threshold := int(conn.t * float32(sp.GetTargetRate()))
				bufLen := receiveBuf.GetLen() / defconn.MaxPacketPayloadLength
				//log.Debugf("[DEBUG] BufLen %v, threshold %v", bufLen, threshold)
				if bufLen > threshold {
					sp.SetTargetRate(conn.r)
					log.Infof("[Event] BufLen %v > threshold %v, the rate is reset to %v at %v",
						bufLen, threshold, conn.r, utils.GetFormattedCurrentTime())
				}
			}

			if receiveBuf.GetLen() > 0 {
				// send real packets
				var payload [defconn.MaxPacketPayloadLength]byte
				rdLen, rdErr := receiveBuf.Read(payload[:])
				written += int64(rdLen)
				if rdErr != nil {
					log.Infof("Exit by read buffer err:%v", rdErr)
					return written, rdErr
				}
				conn.sendChan <- packetInfo{pktType: defconn.PacketTypePayload, rate: uint32(sp.GetTargetRate()),
					data: payload[:rdLen], padLen: uint16(defconn.MaxPacketPaddingLength - rdLen)}
			} else if conn.ConnState.LoadCurState() == defconn.StateStart && sp.ConsumePaddingBudget() {
				//if there is no data in the buffer:
				//if defense on && has padding budget -> send dummy packet
				//else: skip (defense off or no padding budget)
				log.Debugf("[DEBUG] The current padding budget is %v", sp.GetPaddingBudget())
				conn.sendChan <- packetInfo{pktType: defconn.PacketTypeDummy, rate: uint32(sp.GetTargetRate()),
					data: []byte{}, padLen: defconn.MaxPacketPaddingLength}
			}
			rho := time.Duration(int64(1.0 / float64(sp.GetTargetRate()) * 1e9))
			utils.SleepRho(lastSend, rho)
			lastSend = time.Now()
		}
	}

}

func (conn *regulatorConn) clientReadFrom(r io.Reader) (written int64, err error) {
	log.Debugf("[State] Regulator Client Enter copyloop state: %v at %v", defconn.StateMap[conn.ConnState.LoadCurState()], time.Now().Format("15:04:05.000"))
	defer close(conn.CloseChan)

	var receiveBuf utils.SafeBuffer

	var lastSend time.Time

	//create a go routine to send out packets to the wire
	go conn.Send()

	// this go routine regularly check the real throughput
	// if it is small, change to stop state
	go func() {
		ticker := time.NewTicker(defconn.TWindow)
		defer ticker.Stop()
		for {
			select {
			case _, ok := <-conn.CloseChan:
				if !ok {
					log.Infof("[Routine] Ticker routine exits by closeChan.")
					return
				}
			case <-ticker.C:
				log.Debugf("[State] Real Sent: %v, Real Receive: %v, curState: %s at %v.",
					conn.NRealSegSentLoad(), conn.NRealSegRcvLoad(), defconn.StateMap[conn.ConnState.LoadCurState()], utils.GetFormattedCurrentTime())
				if conn.ConnState.LoadCurState() != defconn.StateStop && (conn.NRealSegSentLoad() < 2 || conn.NRealSegRcvLoad() < 2) {
					log.Infof("[State] %s -> %s.", defconn.StateMap[conn.ConnState.LoadCurState()], defconn.StateMap[defconn.StateStop])
					conn.ConnState.SetState(defconn.StateStop)
					conn.sendChan <- packetInfo{pktType: defconn.PacketTypeSignalStop, rate: 1,
						data: []byte{}, padLen: defconn.MaxPacketPaddingLength}
				}
				conn.NRealSegReset()
			}
		}
	}()

	// go routine to receive data from upperstream
	go func() {
		for {
			select {
			case _, ok := <-conn.CloseChan:
				if !ok {
					log.Infof("[Routine] Send routine exits by closedChan.")
					return
				}
			default:
				buf := make([]byte, 65535)
				rdLen, err := r.Read(buf[:])
				if err != nil {
					log.Errorf("Exit by read err:%v", err)
					conn.ErrChan <- err
					return
				}
				if rdLen > 0 {
					_, werr := receiveBuf.Write(buf[:rdLen])
					if werr != nil {
						conn.ErrChan <- werr
						return
					}
					// signal server to start if there is more than one cell coming
					// else switch to padding state
					// stop -> ready -> start
					if (conn.ConnState.LoadCurState() == defconn.StateStop && rdLen > defconn.MaxPacketPayloadLength) ||
						(conn.ConnState.LoadCurState() == defconn.StateReady) {
						// stateStop with >2 cells -> stateStart
						// or stateReady with >0 cell -> stateStart
						log.Infof("[State] Got %v bytes upstream, %s -> %s.", rdLen, defconn.StateMap[conn.ConnState.LoadCurState()], defconn.StateMap[defconn.StateStart])
						conn.ConnState.SetState(defconn.StateStart)
						conn.sendChan <- packetInfo{pktType: defconn.PacketTypeSignalStart, rate: 1,
							data: []byte{}, padLen: defconn.MaxPacketPaddingLength}
					} else if conn.ConnState.LoadCurState() == defconn.StateStop {
						log.Infof("[State] Got %v bytes upstream, %s -> %s.", rdLen, defconn.StateMap[defconn.StateStop], defconn.StateMap[defconn.StateReady])
						conn.ConnState.SetState(defconn.StateReady)
					}
				} else {
					log.Errorf("BUG? read 0 bytes, err: %v", err)
					conn.ErrChan <- io.EOF
					return
				}
			}
		}
	}()

	// mainloop, when defense is on, client send packets with rate r/u
	lastSend = time.Now()
	for {
		select {
		case conErr := <-conn.ErrChan:
			log.Infof("downstream copy loop terminated at %v. Reason: %v", utils.GetFormattedCurrentTime(), conErr)
			return written, conErr
		default:
			//defense on
			curClientRate := float32(atomic.LoadInt32(&conn.r)) / conn.u
			rho := time.Duration(int64(1.0 / curClientRate * 1e9))
			//log.Debugf("[DEBUG] Current client rate: %.0f (%v) at %v", curClientRate, rho, utils.GetFormattedCurrentTime())

			if receiveBuf.GetLen() > 0 {
				var payload [defconn.MaxPacketPayloadLength]byte

				rdLen, rdErr := receiveBuf.Read(payload[:])
				written += int64(rdLen)
				if rdErr != nil {
					log.Infof("Exit by read buffer err:%v", rdErr)
					return written, rdErr
				}
				conn.sendChan <- packetInfo{pktType: defconn.PacketTypePayload, rate: 1,
					data: payload[:rdLen], padLen: uint16(defconn.MaxPacketPaddingLength - rdLen)}
				conn.NRealSegSentIncrement()
			} else if conn.ConnState.LoadCurState() == defconn.StateStart {
				conn.sendChan <- packetInfo{pktType: defconn.PacketTypeDummy, rate: 1,
					data: []byte{}, padLen: defconn.MaxPacketPaddingLength}
			}

			utils.SleepRho(lastSend, rho)
			lastSend = time.Now()
		}
	}
}

func (conn *regulatorConn) ReadFrom(r io.Reader) (written int64, err error) {
	if conn.IsServer {
		return conn.serverReadFrom(r)
	} else {
		return conn.clientReadFrom(r)
	}
}

func (conn *regulatorConn) Read(b []byte) (n int, err error) {
	return conn.DefConn.MyRead(b, conn.readPackets)
}

var _ base.ClientFactory = (*regulatorClientFactory)(nil)
var _ base.ServerFactory = (*regulatorServerFactory)(nil)
var _ base.Transport = (*Transport)(nil)
var _ net.Conn = (*regulatorConn)(nil)
