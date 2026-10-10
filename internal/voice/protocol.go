package voice

import (
	"encoding/binary"
	"encoding/json"
	"errors"
)

type Opcode int

const (
	OpIdentify                 Opcode = 0
	OpSelectProtocol           Opcode = 1
	OpReady                    Opcode = 2
	OpHeartbeat                Opcode = 3
	OpSessionDescription       Opcode = 4
	OpSpeaking                 Opcode = 5
	OpHeartbeatAck             Opcode = 6
	OpResume                   Opcode = 7
	OpHello                    Opcode = 8
	OpResumed                  Opcode = 9
	OpClientsConnect           Opcode = 11
	OpClientDisconnect         Opcode = 13
	OpDavePrepareTransition    Opcode = 21
	OpDaveExecuteTransition    Opcode = 22
	OpDaveTransitionReady      Opcode = 23
	OpDavePrepareEpoch         Opcode = 24
	OpDaveExternalSender       Opcode = 25
	OpDaveKeyPackage           Opcode = 26
	OpDaveProposals            Opcode = 27
	OpDaveCommitWelcome        Opcode = 28
	OpDaveAnnounceCommit       Opcode = 29
	OpDaveWelcome              Opcode = 30
	OpDaveInvalidCommitWelcome Opcode = 31
)

const GatewayVersion = 8

type EncryptionMode string

const (
	ModeAES256GCM         EncryptionMode = "aead_aes256_gcm_rtpsize"
	ModeXChaCha20Poly1305 EncryptionMode = "aead_xchacha20_poly1305_rtpsize"
)

func ChooseMode(offered []EncryptionMode) (EncryptionMode, error) {
	has := func(m EncryptionMode) bool {
		for _, o := range offered {
			if o == m {
				return true
			}
		}
		return false
	}
	if has(ModeAES256GCM) {
		return ModeAES256GCM, nil
	}
	if has(ModeXChaCha20Poly1305) {
		return ModeXChaCha20Poly1305, nil
	}
	return "", errors.New("voice server offered no supported encryption mode")
}

type inbound struct {
	Op  Opcode          `json:"op"`
	D   json.RawMessage `json:"d"`
	S   *int            `json:"s"`
	Seq *int            `json:"seq"`
}

func (m inbound) sequence() (int, bool) {
	if m.Seq != nil {
		return *m.Seq, true
	}
	if m.S != nil {
		return *m.S, true
	}
	return 0, false
}

type outbound struct {
	Op Opcode `json:"op"`
	D  any    `json:"d"`
}

type identify struct {
	ServerID               string `json:"server_id"`
	UserID                 string `json:"user_id"`
	SessionID              string `json:"session_id"`
	Token                  string `json:"token"`
	MaxDaveProtocolVersion int    `json:"max_dave_protocol_version"`
}

type resume struct {
	ServerID  string `json:"server_id"`
	SessionID string `json:"session_id"`
	Token     string `json:"token"`
	SeqAck    int    `json:"seq_ack"`
}

type heartbeat struct {
	T      int64 `json:"t"`
	SeqAck int   `json:"seq_ack"`
}

type hello struct {
	HeartbeatInterval float64 `json:"heartbeat_interval"`
}

type ready struct {
	SSRC  uint32           `json:"ssrc"`
	IP    string           `json:"ip"`
	Port  int              `json:"port"`
	Modes []EncryptionMode `json:"modes"`
}

type selectProtocol struct {
	Protocol string             `json:"protocol"`
	Data     selectProtocolData `json:"data"`
}

type selectProtocolData struct {
	Address string         `json:"address"`
	Port    int            `json:"port"`
	Mode    EncryptionMode `json:"mode"`
}

type sessionDescription struct {
	Mode                EncryptionMode `json:"mode"`
	SecretKey           []byte         `json:"secret_key"`
	DaveProtocolVersion int            `json:"dave_protocol_version"`
}

type speaking struct {
	Speaking int    `json:"speaking"`
	Delay    int    `json:"delay"`
	SSRC     uint32 `json:"ssrc"`
}

const SpeakingMicrophone = 1

type clientsConnect struct {
	UserIDs []string `json:"user_ids"`
}

type clientDisconnect struct {
	UserID string `json:"user_id"`
}

type transition struct {
	TransitionID    uint16 `json:"transition_id"`
	ProtocolVersion uint16 `json:"protocol_version"`
}

type prepareEpoch struct {
	Epoch           int    `json:"epoch"`
	ProtocolVersion uint16 `json:"protocol_version"`
}

type BinaryFrame struct {
	Seq          uint16
	Op           Opcode
	TransitionID uint16
	Payload      []byte
}

var ErrShortFrame = errors.New("binary voice frame too short")

func ParseBinary(raw []byte) (BinaryFrame, error) {
	if len(raw) < 3 {
		return BinaryFrame{}, ErrShortFrame
	}
	f := BinaryFrame{Seq: binary.BigEndian.Uint16(raw), Op: Opcode(raw[2])}
	body := raw[3:]
	switch f.Op {
	case OpDaveAnnounceCommit, OpDaveWelcome:
		if len(body) < 2 {
			return BinaryFrame{}, ErrShortFrame
		}
		f.TransitionID = binary.BigEndian.Uint16(body)
		body = body[2:]
	}
	f.Payload = append([]byte(nil), body...)
	return f, nil
}

func EncodeBinary(op Opcode, payload []byte) []byte {
	out := make([]byte, 1+len(payload))
	out[0] = byte(op)
	copy(out[1:], payload)
	return out
}
