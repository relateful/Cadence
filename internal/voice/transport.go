package voice

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"time"

	"golang.org/x/crypto/chacha20poly1305"
)

const (
	RTPHeaderSize     = 12
	rtpVersionFlags   = 0x80
	rtpPayloadType    = 0x78
	discoveryLength   = 74
	discoveryRequest  = 0x1
	discoveryResponse = 0x2
	discoveryTimeout  = 5 * time.Second
	discoveryResend   = time.Second
	nonceCounterSize  = 4
)

type Sealer struct {
	aead    cipher.AEAD
	nonce   []byte
	counter uint32
	buf     []byte
}

func NewSealer(mode EncryptionMode, key []byte) (*Sealer, error) {
	var aead cipher.AEAD
	switch mode {
	case ModeAES256GCM:
		block, err := aes.NewCipher(key)
		if err != nil {
			return nil, err
		}
		if aead, err = cipher.NewGCM(block); err != nil {
			return nil, err
		}
	case ModeXChaCha20Poly1305:
		c, err := chacha20poly1305.NewX(key)
		if err != nil {
			return nil, err
		}
		aead = c
	default:
		return nil, fmt.Errorf("unsupported mode %q", mode)
	}
	return &Sealer{aead: aead, nonce: make([]byte, aead.NonceSize()), buf: make([]byte, 0, 2048)}, nil
}

func (s *Sealer) Seal(header []byte, payload []byte) []byte {
	binary.BigEndian.PutUint32(s.nonce, s.counter)
	s.counter++
	s.buf = append(s.buf[:0], header...)
	s.buf = s.aead.Seal(s.buf, s.nonce, payload, header)
	return append(s.buf, s.nonce[:nonceCounterSize]...)
}

func (s *Sealer) Open(packet []byte) ([]byte, error) {
	if len(packet) < RTPHeaderSize+nonceCounterSize+s.aead.Overhead() {
		return nil, errors.New("packet too short")
	}
	nonce := make([]byte, s.aead.NonceSize())
	copy(nonce, packet[len(packet)-nonceCounterSize:])
	return s.aead.Open(nil, nonce, packet[RTPHeaderSize:len(packet)-nonceCounterSize], packet[:RTPHeaderSize])
}

type RTP struct {
	header    [RTPHeaderSize]byte
	sequence  uint16
	timestamp uint32
}

func NewRTP(ssrc uint32) *RTP {
	r := &RTP{}
	r.header[0], r.header[1] = rtpVersionFlags, rtpPayloadType
	binary.BigEndian.PutUint32(r.header[8:], ssrc)
	return r
}

func (r *RTP) Next(samplesPerFrame uint32) []byte {
	binary.BigEndian.PutUint16(r.header[2:], r.sequence)
	binary.BigEndian.PutUint32(r.header[4:], r.timestamp)
	r.sequence++
	r.timestamp += samplesPerFrame
	return r.header[:]
}

func DiscoveryPacket(ssrc uint32) []byte {
	p := make([]byte, discoveryLength)
	binary.BigEndian.PutUint16(p, discoveryRequest)
	binary.BigEndian.PutUint16(p[2:], discoveryLength-4)
	binary.BigEndian.PutUint32(p[4:], ssrc)
	return p
}

func ParseDiscovery(p []byte) (string, int, error) {
	if len(p) < discoveryLength || binary.BigEndian.Uint16(p) != discoveryResponse {
		return "", 0, errors.New("invalid ip discovery response")
	}
	addr := p[8:72]
	end := 0
	for end < len(addr) && addr[end] != 0 {
		end++
	}
	return string(addr[:end]), int(binary.BigEndian.Uint16(p[72:])), nil
}

type Discovery struct {
	IP       string
	Port     int
	From     *net.UDPAddr
	Attempts int
}

func discover(conn *net.UDPConn, remote *net.UDPAddr, ssrc uint32) (Discovery, error) {
	request := DiscoveryPacket(ssrc)
	deadline := time.Now().Add(discoveryTimeout)
	defer conn.SetReadDeadline(time.Time{})
	buf := make([]byte, 1500)
	attempts := 0
	for time.Now().Before(deadline) {
		if _, err := conn.WriteToUDP(request, remote); err != nil {
			return Discovery{}, fmt.Errorf("ip discovery send: %w", err)
		}
		attempts++
		resend := time.Now().Add(discoveryResend)
		if resend.After(deadline) {
			resend = deadline
		}
		conn.SetReadDeadline(resend)
		for {
			n, from, err := conn.ReadFromUDP(buf)
			if err != nil {
				var ne net.Error
				if errors.As(err, &ne) && ne.Timeout() {
					break
				}
				return Discovery{}, fmt.Errorf("ip discovery: %w", err)
			}
			if n < discoveryLength || binary.BigEndian.Uint32(buf[4:8]) != ssrc {
				continue
			}
			ip, port, err := ParseDiscovery(buf[:n])
			if err != nil {
				continue
			}
			return Discovery{IP: ip, Port: port, From: from, Attempts: attempts}, nil
		}
	}
	return Discovery{}, fmt.Errorf("ip discovery: no reply from %s after %d attempts in %s", remote, attempts, discoveryTimeout)
}

func drainUDP(conn *net.UDPConn) {
	buf := make([]byte, 2048)
	for {
		if _, _, err := conn.ReadFromUDP(buf); err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return
		}
	}
}
