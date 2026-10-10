package voice

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"testing"
)

func TestChooseModePrefersAESGCM(t *testing.T) {
	m, err := ChooseMode([]EncryptionMode{"xsalsa20_poly1305", ModeXChaCha20Poly1305, ModeAES256GCM})
	if err != nil || m != ModeAES256GCM {
		t.Fatalf("mode %s err %v", m, err)
	}
	m, _ = ChooseMode([]EncryptionMode{ModeXChaCha20Poly1305})
	if m != ModeXChaCha20Poly1305 {
		t.Fatalf("fallback %s", m)
	}
	if _, err := ChooseMode([]EncryptionMode{"xsalsa20_poly1305_lite"}); err == nil {
		t.Fatal("deprecated mode accepted")
	}
}

func TestSealerRoundTrip(t *testing.T) {
	for _, mode := range []EncryptionMode{ModeAES256GCM, ModeXChaCha20Poly1305} {
		key := bytes.Repeat([]byte{7}, 32)
		s, err := NewSealer(mode, key)
		if err != nil {
			t.Fatal(err)
		}
		r, _ := NewSealer(mode, key)
		rtp := NewRTP(42)
		for i := 0; i < 3; i++ {
			payload := []byte{1, 2, 3, byte(i)}
			packet := s.Seal(rtp.Next(960), payload)
			got, err := r.Open(packet)
			if err != nil || !bytes.Equal(got, payload) {
				t.Fatalf("%s: got %v err %v", mode, got, err)
			}
		}
	}
}

func TestRTPHeaderAdvances(t *testing.T) {
	r := NewRTP(0xAABBCCDD)
	h1 := append([]byte(nil), r.Next(960)...)
	h2 := r.Next(960)
	if h1[0] != 0x80 || h1[1] != 0x78 || binary.BigEndian.Uint32(h1[8:]) != 0xAABBCCDD {
		t.Fatalf("header %x", h1)
	}
	if binary.BigEndian.Uint16(h2[2:]) != 1 || binary.BigEndian.Uint32(h2[4:]) != 960 {
		t.Fatalf("second header %x", h2)
	}
}

func TestDiscoveryRoundTrip(t *testing.T) {
	p := DiscoveryPacket(99)
	if len(p) != 74 || binary.BigEndian.Uint16(p[2:]) != 70 || binary.BigEndian.Uint32(p[4:]) != 99 {
		t.Fatalf("request %x", p)
	}
	res := make([]byte, 74)
	binary.BigEndian.PutUint16(res, 2)
	copy(res[8:], "203.0.113.9")
	binary.BigEndian.PutUint16(res[72:], 50001)
	ip, port, err := ParseDiscovery(res)
	if err != nil || ip != "203.0.113.9" || port != 50001 {
		t.Fatalf("%s:%d %v", ip, port, err)
	}
}

func TestParseBinaryCommitCarriesTransition(t *testing.T) {
	raw := []byte{0x00, 0x05, byte(OpDaveAnnounceCommit), 0x01, 0x02, 0xAA, 0xBB}
	f, err := ParseBinary(raw)
	if err != nil || f.Seq != 5 || f.TransitionID != 0x0102 || !bytes.Equal(f.Payload, []byte{0xAA, 0xBB}) {
		t.Fatalf("%+v %v", f, err)
	}
	f, _ = ParseBinary([]byte{0, 1, byte(OpDaveExternalSender), 9})
	if f.TransitionID != 0 || !bytes.Equal(f.Payload, []byte{9}) {
		t.Fatalf("external sender %+v", f)
	}
	if _, err := ParseBinary([]byte{0, 1, byte(OpDaveWelcome), 1}); err == nil {
		t.Fatal("short welcome accepted")
	}
}

func TestEncodeBinaryHasNoSequence(t *testing.T) {
	if got := EncodeBinary(OpDaveKeyPackage, []byte{1, 2}); !bytes.Equal(got, []byte{26, 1, 2}) {
		t.Fatalf("%v", got)
	}
}

func TestInboundSequenceAcceptsBothSpellings(t *testing.T) {
	var a, b inbound
	json.Unmarshal([]byte(`{"op":5,"d":{},"seq":12}`), &a)
	json.Unmarshal([]byte(`{"op":5,"d":{},"s":7}`), &b)
	if s, _ := a.sequence(); s != 12 {
		t.Fatalf("seq %d", s)
	}
	if s, _ := b.sequence(); s != 7 {
		t.Fatalf("s %d", s)
	}
}

func TestRegionFromEndpoint(t *testing.T) {
	s := Server{Endpoint: "c-dfw09-1a2b.discord.media:443"}
	if r := s.Region(); r != "dfw" {
		t.Fatalf("region %q", r)
	}
	if r := (Server{Endpoint: "brazil11120.discord.media:443"}).Region(); r != "brazil" {
		t.Fatalf("region %q", r)
	}
}
