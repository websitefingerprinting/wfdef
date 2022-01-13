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

package regulator

import (
	"encoding/binary"
	"fmt"
	"github.com/websitefingerprinting/wfdef.git/common/log"
	"github.com/websitefingerprinting/wfdef.git/transports/defconn"
	"github.com/websitefingerprinting/wfdef.git/transports/defconn/framing"
	"io"
	"sync/atomic"
	"time"

	"github.com/websitefingerprinting/wfdef.git/common/drbg"
)

const (
	// uint16_t rate
	regulatorOverhead = 8
)

type packetInfo struct {
	pktType uint8
	data    []byte
	padLen  uint16
	t0      int64
}

var zeroPadBytes [defconn.MaxPacketPaddingLength]byte

func (conn *regulatorConn) MakePacket(w io.Writer, pktType uint8, t0 int64, data []byte, padLen uint16) error {
	var pkt [framing.MaximumFramePayloadLength]byte

	if len(data)+int(padLen) > defconn.MaxPacketPayloadLength+regulatorOverhead {
		panic(fmt.Sprintf("BUG: MakePacket() len(Data) + PadLen > MaxPacketPayloadLength + RegulatorOverhead: %d + %d > %d + %d",
			len(data), padLen, defconn.MaxPacketPayloadLength, regulatorOverhead))
	}

	// Packets are:
	//   uint8_t  type         PacketTypePayload (0x00)
	//   uint16_t length       Length of the payload (Big Endian).
	//   uint8_t[]             Payload Data payload.
	//   uint64 t0             Surge start time Unix (Big Endian).
	//   uint8_t[]             padding Padding.
	pkt[0] = pktType
	binary.BigEndian.PutUint16(pkt[1:], uint16(len(data)))
	if len(data) > 0 {
		copy(pkt[3:], data[:])
	}

	var rateBytes [regulatorOverhead]byte
	binary.BigEndian.PutUint64(rateBytes[:], uint64(t0))
	copy(pkt[3+len(data):], rateBytes[:])
	copy(pkt[3+len(data)+regulatorOverhead:], zeroPadBytes[:padLen])

	pktLen := defconn.PacketOverhead + len(data) + regulatorOverhead + int(padLen)

	// Encode the packet in an AEAD frame.
	var frame [framing.MaximumSegmentLength]byte
	frameLen, err := conn.Encoder.Encode(frame[:], pkt[:pktLen])
	if err != nil {
		// All Encoder errors are fatal.
		return err
	}
	wrLen, err := w.Write(frame[:frameLen])
	if err != nil {
		return err
	} else if wrLen < frameLen {
		return io.ErrShortWrite
	}

	return nil
}

func (conn *regulatorConn) readPackets() (err error) {
	// Attempt to read off the network.
	rdLen, rdErr := conn.Conn.Read(conn.ReadBuffer)
	conn.ReceiveBuffer.Write(conn.ReadBuffer[:rdLen])

	var decoded [framing.MaximumFramePayloadLength]byte
	for conn.ReceiveBuffer.Len() > 0 {
		// Decrypt an AEAD frame.
		decLen := 0
		decLen, err = conn.Decoder.Decode(decoded[:], conn.ReceiveBuffer)
		if err == framing.ErrAgain {
			break
		} else if err != nil {
			break
		} else if decLen < defconn.PacketOverhead {
			err = defconn.InvalidPacketLengthError(decLen)
			break
		}

		// Decode the packet.
		pkt := decoded[0:decLen]
		pktType := pkt[0]
		payloadLen := binary.BigEndian.Uint16(pkt[1:])
		if int(payloadLen) > len(pkt)-defconn.PacketOverhead {
			err = defconn.InvalidPayloadLengthError(int(payloadLen))
			break
		}
		payload := pkt[3 : 3+payloadLen]

		t0 := int64(binary.BigEndian.Uint64(pkt[3+payloadLen:]))
		//log.Debugf("[DEBUG] Rcv Packet: decLen: %v, pktType %v rate %v payloadLen %v", decLen, pktType, rate, payloadLen)

		if !conn.IsServer && pktType != defconn.PacketTypePrngSeed && LogEnabled {
			log.Infof("[TRACE_LOG] %d %d %d", time.Now().UnixNano(), -int64(payloadLen), -int64(defconn.MaxPacketPayloadLength-payloadLen))
		} else {
			log.Debugf("[Rcv]  %-8s, %-3d+ %-3d bytes at %v", defconn.PktTypeMap[pktType], -int64(payloadLen),
				-int64(defconn.MaxPacketPayloadLength-payloadLen), time.Now().Format("15:04:05.000"))
		}

		switch pktType {
		case defconn.PacketTypePayload:
			if payloadLen > 0 {
				conn.ReceiveDecodedBuffer.Write(payload)
				if !conn.IsServer {
					conn.NRealSegRcvIncrement()
					if atomic.LoadInt64(&conn.t0) != t0 {
						atomic.StoreInt64(&conn.t0, t0)
						log.Debugf("[Event] Client Receives new t0: %v (%v)", time.Unix(0, t0), t0)
					}
				}
			}
		case defconn.PacketTypePrngSeed:
			// Only regenerate the distribution if we are the client.
			if len(payload) == defconn.SeedPacketPayloadLength && !conn.IsServer {
				var seed *drbg.Seed
				seed, err = drbg.SeedFromBytes(payload)
				if err != nil {
					break
				}
				conn.LenDist.Reset(seed)
			}
		case defconn.PacketTypeSignalStart:
			// a signal from client to make server change to stateStart
			if !conn.IsServer {
				panic(fmt.Sprintf("Client receive SignalStart pkt from server? "))
			}
			conn.paddingChan <- true //Initialize the defense params
		case defconn.PacketTypeSignalStop:
			// a signal from client to make server change to stateStop
			if !conn.IsServer {
				panic(fmt.Sprintf("Client receive SignalStop pkt from server? "))
			}
			conn.paddingChan <- false // Stop the defense

		case defconn.PacketTypeDummy:
			if !conn.IsServer && atomic.LoadInt64(&conn.t0) != t0 {
				atomic.StoreInt64(&conn.t0, t0)
				log.Debugf("[Event] Client Receives new t0: %v", time.Unix(0, t0))
			}
		default:
			// Ignore unknown packet types.
		}
	}

	// Read errors (all fatal) take priority over various frame processing
	// errors.
	if rdErr != nil {
		return rdErr
	}

	return
}
