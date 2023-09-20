package front

import (
	"bytes"
	"crypto/rand"
	"github.com/websitefingerprinting/wfdef.git/transports/front/framing"
	"testing"
)

func generateRandomKey() []byte {
	key := make([]byte, framing.KeyLength)

	_, err := rand.Read(key)
	if err != nil {
		panic(err)
	}

	return key
}

func BenchmarkPacket_MakePacket(b *testing.B) {
	var chopBuf [framing.MaximumFramePayloadLength]byte
	var frame [framing.MaximumSegmentLength]byte
	pktType := 0
	payload := make([]byte, 512)
	var conn frontConn
	conn.encoder = framing.NewEncoder(generateRandomKey())
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		transfered := 0
		buffer := bytes.NewBuffer(payload)
		for 0 < buffer.Len() {
			n, err := buffer.Read(chopBuf[:])
			if err != nil {
				b.Fatal("buffer.Read() failed:", err)
			}

			_ = conn.makePacket(bytes.NewBuffer(frame[:]), uint8(pktType), chopBuf[:n], uint16(maxPacketPayloadLength-len(payload)))
			transfered += n - framing.FrameOverhead
		}
	}
}

func BenchmarkEncoder_Encode(b *testing.B) {
	var chopBuf [framing.MaximumFramePayloadLength]byte
	var frame [framing.MaximumSegmentLength]byte
	payload := make([]byte, packetOverhead+maxPacketPayloadLength)
	encoder := framing.NewEncoder(generateRandomKey())
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		transfered := 0
		buffer := bytes.NewBuffer(payload)
		for 0 < buffer.Len() {
			n, err := buffer.Read(chopBuf[:])
			if err != nil {
				b.Fatal("buffer.Read() failed:", err)
			}

			n, _ = encoder.Encode(frame[:], chopBuf[:n])
			transfered += n - framing.FrameOverhead
		}
		if transfered != len(payload) {
			b.Fatalf("Transfered length mismatch: %d != %d", transfered,
				len(payload))
		}
	}
}
