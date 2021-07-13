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

package front

import (
	"encoding/binary"
	"fmt"
	"github.com/websitefingerprinting/wfdef.git/common/log"
	"io"
	"sync/atomic"
	"time"

	"github.com/websitefingerprinting/wfdef.git/common/drbg"
	"github.com/websitefingerprinting/wfdef.git/transports/front/framing"
)

const (
	packetOverhead          = 2 + 1
	//maxPacketPayloadLength  = framing.MaximumFramePayloadLength - packetOverhead
	maxPacketPayloadLength  = 536 //size of a Tor cell wrapped by TLS
	maxPacketPaddingLength  = maxPacketPayloadLength
	seedPacketPayloadLength = seedLength

	consumeReadSize = framing.MaximumSegmentLength * 16
)

const (
	packetTypePayload = iota
	packetTypeDummy
	packetTypePrngSeed
	packetTypeSignalStart
	packetTypeSignalStop
)

var pktTypeMap = map[uint8]string {
	packetTypePayload:      "Payload",
	packetTypeDummy:        "Dummy",
	packetTypePrngSeed:     "PrngSeed",
	packetTypeSignalStart:  "SigStart",
	packetTypeSignalStop:   "SigStop",
}

// InvalidPacketLengthError is the error returned when decodePacket detects a
// invalid packet length/
type InvalidPacketLengthError int

func (e InvalidPacketLengthError) Error() string {
	return fmt.Sprintf("packet: Invalid packet length: %d", int(e))
}

// InvalidPayloadLengthError is the error returned when decodePacket rejects the
// payload length.
type InvalidPayloadLengthError int

type PacketInfo struct {
	pktType  uint8
	data     []byte
	padLen   uint16
}

func (e InvalidPayloadLengthError) Error() string {
	return fmt.Sprintf("packet: Invalid payload length: %d", int(e))
}

var zeroPadBytes [maxPacketPaddingLength]byte

func (conn *frontConn) makePacket(w io.Writer, pktType uint8, data []byte, padLen uint16) error {
	var pkt [framing.MaximumFramePayloadLength]byte

	if len(data)+int(padLen) > maxPacketPayloadLength {
		panic(fmt.Sprintf("BUG: makePacket() len(data) + padLen > maxPacketPayloadLength: %d + %d > %d",
			len(data), padLen, maxPacketPayloadLength))
	}

	// Packets are:
	//   uint8_t type      packetTypePayload (0x00)
	//   uint16_t length   Length of the payload (Big Endian).
	//   uint8_t[] payload Data payload.
	//   uint8_t[] padding Padding.
	pkt[0] = pktType
	binary.BigEndian.PutUint16(pkt[1:], uint16(len(data)))
	if len(data) > 0 {
		copy(pkt[3:], data[:])
	}
	copy(pkt[3+len(data):], zeroPadBytes[:padLen])

	pktLen := packetOverhead + len(data) + int(padLen)

	// Encode the packet in an AEAD frame.
	var frame [framing.MaximumSegmentLength]byte
	frameLen, err := conn.encoder.Encode(frame[:], pkt[:pktLen])
	if err != nil {
		// All encoder errors are fatal.
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

func (conn *frontConn) readPackets() (err error) {
	// Attempt to read off the network.
	rdLen, rdErr := conn.Conn.Read(conn.readBuffer)
	conn.receiveBuffer.Write(conn.readBuffer[:rdLen])

	var decoded [framing.MaximumFramePayloadLength]byte
	for conn.receiveBuffer.Len() > 0 {
		// Decrypt an AEAD frame.
		decLen := 0
		decLen, err = conn.decoder.Decode(decoded[:], conn.receiveBuffer)
		if err == framing.ErrAgain {
			break
		} else if err != nil {
			break
		} else if decLen < packetOverhead {
			err = InvalidPacketLengthError(decLen)
			break
		}

		// Decode the packet.
		pkt := decoded[0:decLen]
		pktType := pkt[0]
		payloadLen := binary.BigEndian.Uint16(pkt[1:])
		if int(payloadLen) > len(pkt)-packetOverhead {
			err = InvalidPayloadLengthError(int(payloadLen))
			break
		}
		payload := pkt[3 : 3+payloadLen]


		if !conn.isServer && traceLogEnabled && conn.logger.logOn.Load().(bool) && pktType != packetTypePrngSeed{
			conn.loggerChan <- []int64{time.Now().UnixNano(), -int64(payloadLen), -(int64(decLen - packetOverhead) - int64(payloadLen))}
		}
		if !conn.isServer && pktType != packetTypePrngSeed && logEnabled{
			log.Infof("[TRACE_LOG] %d %d %d", time.Now().UnixNano(), -int64(payloadLen), -(int64(decLen - packetOverhead) - int64(payloadLen)))
		}

		switch pktType {
		case packetTypePayload:
			if payloadLen > 0 {
				conn.receiveDecodedBuffer.Write(payload)
			}
		case packetTypePrngSeed:
			// Only regenerate the distribution if we are the client.
			if len(payload) == seedPacketPayloadLength && !conn.isServer {
				var seed *drbg.Seed
				seed, err = drbg.SeedFromBytes(payload)
				if err != nil {
					break
				}
				conn.lenDist.Reset(seed)
			}
		case packetTypeSignalStart:
			// a signal from client to make server change to stateStart
			if !conn.isServer {
				panic(fmt.Sprintf("Client receive SignalStart pkt from server? "))
			}
			if atomic.LoadUint32(&conn.state) != stateStart {
				log.Debugf("[State] Client signal: %s -> %s.", stateMap[atomic.LoadUint32(&conn.state)], stateMap[stateStart])
				conn.paddingChan <- true
				atomic.StoreUint32(&conn.state, stateStart)
			}
		case packetTypeSignalStop:
			// a signal from client to make server change to stateStop
			if !conn.isServer {
				panic(fmt.Sprintf("Client receive SignalStop pkt from server? "))
			}
			if atomic.LoadUint32(&conn.state) != stateStop{
				log.Debugf("[State] Client signal: %s -> %s.", stateMap[atomic.LoadUint32(&conn.state)], stateMap[stateStop])
				conn.paddingChan <- false
				atomic.StoreUint32(&conn.state, stateStop)
			}
		case packetTypeDummy:
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
