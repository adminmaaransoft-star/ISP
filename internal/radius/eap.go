package radius

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// EAP packet encoding — RFC 3748 (EAP) and RFC 2759 (MS-CHAPv2 sub-packets).
// FR-AAA-006 | MDS §4.18.

// EAP codes (RFC 3748 §4).
const (
	EAPCodeRequest  = 1
	EAPCodeResponse = 2
	EAPCodeSuccess  = 3
	EAPCodeFailure  = 4
)

// EAP method types.
const (
	EAPTypeIdentity = 1
	EAPTypeNak      = 3
	EAPTypeMSCHAPv2 = 26
	EAPTypeNotUsed  = 0
)

// MS-CHAPv2 opcodes carried inside an EAP-MSCHAPv2 packet (RFC 2759 §2).
const (
	MSCHAPv2OpChallenge = 1
	MSCHAPv2OpResponse  = 2
	MSCHAPv2OpSuccess   = 3
	MSCHAPv2OpFailure   = 4
)

// eapHeaderLen is Code(1) + Identifier(1) + Length(2).
const eapHeaderLen = 4

var (
	// ErrShortEAP means the bytes could not be a well-formed EAP packet.
	// Always a reject rather than a retry: a truncated packet will not
	// become valid by asking again.
	ErrShortEAP = errors.New("radius: EAP packet is too short")
	// ErrEAPLengthMismatch means the declared length disagrees with the
	// bytes actually present — the classic parser confusion a hostile
	// supplicant would try first.
	ErrEAPLengthMismatch = errors.New("radius: EAP length field disagrees with the packet size")
)

// EAPPacket is a decoded EAP message.
type EAPPacket struct {
	Code       uint8
	Identifier uint8
	Type       uint8
	// Data is the method-specific payload after the Type byte. Empty for
	// Success and Failure, which carry no type at all.
	Data []byte
}

// ParseEAP decodes an EAP packet.
//
// The declared Length is validated against the real buffer rather than
// trusted: every field after it is sliced using attacker-supplied offsets,
// and a mismatch here is what turns a malformed packet into an out-of-range
// panic inside a RADIUS worker.
func ParseEAP(b []byte) (*EAPPacket, error) {
	if len(b) < eapHeaderLen {
		return nil, ErrShortEAP
	}
	declared := binary.BigEndian.Uint16(b[2:4])
	if int(declared) != len(b) {
		return nil, fmt.Errorf("%w: declared %d, have %d", ErrEAPLengthMismatch, declared, len(b))
	}

	p := &EAPPacket{Code: b[0], Identifier: b[1]}

	// Success and Failure are header-only; they have no Type byte.
	if p.Code == EAPCodeSuccess || p.Code == EAPCodeFailure {
		return p, nil
	}
	if len(b) < eapHeaderLen+1 {
		return nil, ErrShortEAP
	}
	p.Type = b[4]
	p.Data = b[5:]
	return p, nil
}

// Encode serialises an EAP packet, filling in the Length field.
func (p *EAPPacket) Encode() []byte {
	if p.Code == EAPCodeSuccess || p.Code == EAPCodeFailure {
		out := make([]byte, eapHeaderLen)
		out[0] = p.Code
		out[1] = p.Identifier
		binary.BigEndian.PutUint16(out[2:4], eapHeaderLen)
		return out
	}

	total := eapHeaderLen + 1 + len(p.Data)
	out := make([]byte, 0, total)
	out = append(out, p.Code, p.Identifier, 0, 0, p.Type)
	out = append(out, p.Data...)
	binary.BigEndian.PutUint16(out[2:4], uint16(total)) //nolint:gosec // bounded by RADIUS attribute limits
	return out
}

// MSCHAPv2Challenge is the server's challenge, carried as EAP-Request/26.
type MSCHAPv2Challenge struct {
	MSCHAPv2ID uint8
	Challenge  []byte // 16 bytes
	Name       string
}

// Encode builds the EAP-MSCHAPv2 Challenge payload (everything after the EAP
// Type byte): OpCode, MS-CHAPv2 ID, MS-Length, Value-Size, Value, Name.
func (c *MSCHAPv2Challenge) Encode() []byte {
	// 4 (opcode/id/ms-length) + 1 (value-size) + 16 (challenge) + name
	msLength := 4 + 1 + len(c.Challenge) + len(c.Name)

	out := make([]byte, 0, msLength)
	out = append(out, MSCHAPv2OpChallenge, c.MSCHAPv2ID, 0, 0)
	binary.BigEndian.PutUint16(out[2:4], uint16(msLength)) //nolint:gosec
	out = append(out, uint8(len(c.Challenge)))             //nolint:gosec // always 16
	out = append(out, c.Challenge...)
	out = append(out, []byte(c.Name)...)
	return out
}

// MSCHAPv2Response is the peer's reply, carried as EAP-Response/26.
type MSCHAPv2Response struct {
	MSCHAPv2ID    uint8
	PeerChallenge []byte // 16 bytes
	NTResponse    []byte // 24 bytes
	Flags         uint8
	Name          string
}

// ParseMSCHAPv2Response decodes the payload of an EAP-Response/26 packet.
//
// The 49-byte Value is fixed by RFC 2759: 16 peer challenge, 8 reserved, 24
// NT response, 1 flags. Anything else is malformed, and validating it here
// is what keeps the fixed offsets below in range.
func ParseMSCHAPv2Response(data []byte) (*MSCHAPv2Response, error) {
	// OpCode(1) + ID(1) + MS-Length(2) + Value-Size(1) = 5 minimum
	if len(data) < 5 {
		return nil, fmt.Errorf("radius: MS-CHAPv2 response too short (%d bytes)", len(data))
	}
	if data[0] != MSCHAPv2OpResponse {
		return nil, fmt.Errorf("radius: expected MS-CHAPv2 Response opcode, got %d", data[0])
	}

	valueSize := int(data[4])
	if valueSize != 49 {
		return nil, fmt.Errorf("radius: MS-CHAPv2 Value-Size must be 49, got %d", valueSize)
	}
	if len(data) < 5+valueSize {
		return nil, fmt.Errorf("radius: MS-CHAPv2 response declares %d value bytes but has %d",
			valueSize, len(data)-5)
	}

	value := data[5 : 5+valueSize]
	return &MSCHAPv2Response{
		MSCHAPv2ID:    data[1],
		PeerChallenge: value[0:16],
		// value[16:24] is reserved and must be ignored, not validated —
		// supplicants differ on what they put there.
		NTResponse: value[24:48],
		Flags:      value[48],
		Name:       string(data[5+valueSize:]),
	}, nil
}

// EncodeMSCHAPv2Success builds the EAP-Request/26 Success payload carrying
// the authenticator response, which is how the peer authenticates the server.
func EncodeMSCHAPv2Success(mschapID uint8, authenticatorResponse string) []byte {
	msLength := 4 + len(authenticatorResponse)

	out := make([]byte, 0, msLength)
	out = append(out, MSCHAPv2OpSuccess, mschapID, 0, 0)
	binary.BigEndian.PutUint16(out[2:4], uint16(msLength)) //nolint:gosec
	out = append(out, []byte(authenticatorResponse)...)
	return out
}

// EncodeMSCHAPv2Failure builds the EAP-Request/26 Failure payload.
//
// E=691 is "authentication failure" and R=0 tells the supplicant not to
// retry with the same credentials — without R=0 Windows re-prompts in a loop,
// which looks like a hung client rather than a rejected password.
func EncodeMSCHAPv2Failure(mschapID uint8) []byte {
	const message = "E=691 R=0 C=00000000000000000000000000000000 V=3 M=Authentication failed"
	msLength := 4 + len(message)

	out := make([]byte, 0, msLength)
	out = append(out, MSCHAPv2OpFailure, mschapID, 0, 0)
	binary.BigEndian.PutUint16(out[2:4], uint16(msLength)) //nolint:gosec
	out = append(out, []byte(message)...)
	return out
}
