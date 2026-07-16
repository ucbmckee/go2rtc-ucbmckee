package baichuan

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
)

func TestUIDPacketRoundTrip(t *testing.T) {
	payload := []byte("independently-authored-payload")
	tests := []struct {
		name string
		make func() ([]byte, error)
		want uidPacket
	}{
		{name: "discovery", make: func() ([]byte, error) {
			return marshalUIDDiscovery(73, payload)
		}, want: uidPacket{magic: uidMagicDiscovery, transaction: 73}},
		{name: "ack", make: func() ([]byte, error) {
			return marshalUIDAck(-37, 91, payload, 128)
		}, want: uidPacket{magic: uidMagicAck, connectionID: -37, packetID: 91}},
		{name: "data", make: func() ([]byte, error) {
			return marshalUIDData(-37, 91, payload, 128)
		}, want: uidPacket{magic: uidMagicData, connectionID: -37, packetID: 91}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			b, err := test.make()
			if err != nil {
				t.Fatal(err)
			}
			packet, err := parseUIDPacket(b, 128)
			if err != nil {
				t.Fatal(err)
			}
			if packet.magic != test.want.magic || packet.connectionID != test.want.connectionID ||
				packet.packetID != test.want.packetID || packet.transaction != test.want.transaction ||
				!bytes.Equal(packet.payload, payload) {
				t.Fatalf("unexpected packet: %+v", packet)
			}
		})
	}
}

func TestUIDPacketRejectsMalformedInput(t *testing.T) {
	valid, err := marshalUIDDiscovery(1, []byte("payload"))
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		data []byte
		max  int
		want string
	}{
		{name: "short", data: valid[:3], max: 128, want: "short"},
		{name: "header", data: valid[:12], max: 128, want: "short"},
		{name: "unknown", data: append([]byte{1, 2, 3, 4}, valid[4:]...), max: 128, want: "unknown"},
		{name: "truncated", data: valid[:len(valid)-1], max: 128, want: "length"},
		{name: "trailing", data: append(append([]byte(nil), valid...), 0), max: 128, want: "length"},
		{name: "limited", data: valid, max: 3, want: "length"},
		{name: "negative limit", data: valid, max: -1, want: "limit"},
		{name: "datagram limit", data: make([]byte, uidMaxDatagram+1), max: uidMaxDatagram, want: "limit"},
		{name: "checksum", data: append([]byte(nil), valid...), max: 128, want: "checksum"},
	}
	tests[len(tests)-1].data[len(valid)-1] ^= 1
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := parseUIDPacket(test.data, test.max); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestUIDPacketParseDoesNotCopyPayload(t *testing.T) {
	b, err := marshalUIDData(1, 2, []byte("payload"), 32)
	if err != nil {
		t.Fatal(err)
	}
	packet, err := parseUIDPacket(b, 32)
	if err != nil {
		t.Fatal(err)
	}
	b[uidDataHeader] = 'P'
	if string(packet.payload) != "Payload" {
		t.Fatalf("payload was copied: %q", packet.payload)
	}
	if allocs := testing.AllocsPerRun(100, func() {
		_, _ = parseUIDPacket(b, 32)
	}); allocs != 0 {
		t.Fatalf("parse allocated %.1f times", allocs)
	}
}

func TestUIDPacketMarshalLimits(t *testing.T) {
	payload := make([]byte, 9)
	if _, err := marshalUIDData(1, 1, payload, 8); err == nil {
		t.Fatal("data payload exceeded limit")
	}
	if _, err := marshalUIDAck(1, 1, payload, 8); err == nil {
		t.Fatal("ack payload exceeded limit")
	}
	if _, err := marshalUIDDiscovery(1, make([]byte, uidMaxDatagram)); err == nil {
		t.Fatal("discovery payload exceeded datagram limit")
	}
}

func TestUIDXORRoundTrip(t *testing.T) {
	plain := []byte("uid discovery xml")
	encrypted := make([]byte, len(plain))
	decrypted := make([]byte, len(plain))
	xorUID(encrypted, plain, 0x12345678)
	xorUID(decrypted, encrypted, 0x12345678)
	if !bytes.Equal(decrypted, plain) || bytes.Equal(encrypted, plain) {
		t.Fatalf("unexpected XOR round trip: encrypted=%x decrypted=%q", encrypted, decrypted)
	}
}

func TestUIDChecksumKnownVector(t *testing.T) {
	if sum := uidChecksum([]byte("123456789")); sum != 0x2dfd2d88 {
		t.Fatalf("unexpected checksum: %08x", sum)
	}
}

func FuzzUIDPacket(f *testing.F) {
	for _, magic := range []uint32{uidMagicDiscovery, uidMagicAck, uidMagicData} {
		b := make([]byte, uidAckHeader)
		binary.LittleEndian.PutUint32(b, magic)
		f.Add(b)
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		_, _ = parseUIDPacket(b, 2048)
	})
}
