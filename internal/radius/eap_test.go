package radius_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"

	"github.com/maaransoft/isp-bss-oss/internal/radius"
)

// EAP codec tests — FR-AAA-006 | MDS §4.18.
//
// The parser reads attacker-supplied bytes off the wire before anything has
// authenticated, so its failure modes matter more than its happy path: a
// length field trusted blindly is how a malformed packet becomes a panic in
// a RADIUS worker, taking the whole authentication service with it.

func TestParseEAP_RoundTrip(t *testing.T) {
	original := &radius.EAPPacket{
		Code:       radius.EAPCodeResponse,
		Identifier: 7,
		Type:       radius.EAPTypeIdentity,
		Data:       []byte("alice@isp"),
	}

	got, err := radius.ParseEAP(original.Encode())
	if err != nil {
		t.Fatalf("ParseEAP: %v", err)
	}
	if got.Code != original.Code || got.Identifier != original.Identifier || got.Type != original.Type {
		t.Errorf("header round trip: got %+v want %+v", got, original)
	}
	if !bytes.Equal(got.Data, original.Data) {
		t.Errorf("data round trip: got %q want %q", got.Data, original.Data)
	}
}

// TestParseEAP_SuccessAndFailureAreHeaderOnly: EAP-Success and EAP-Failure
// carry no Type byte. Reading one anyway would consume a byte that is not
// there.
func TestParseEAP_SuccessAndFailureAreHeaderOnly(t *testing.T) {
	for _, code := range []uint8{radius.EAPCodeSuccess, radius.EAPCodeFailure} {
		encoded := (&radius.EAPPacket{Code: code, Identifier: 3}).Encode()
		if len(encoded) != 4 {
			t.Errorf("code %d: encoded length = %d, want 4", code, len(encoded))
		}
		got, err := radius.ParseEAP(encoded)
		if err != nil {
			t.Fatalf("code %d: ParseEAP: %v", code, err)
		}
		if len(got.Data) != 0 {
			t.Errorf("code %d: want no payload, got %q", code, got.Data)
		}
	}
}

// TestParseEAP_RejectsLengthMismatch is the important one. A packet claiming
// to be longer than it is would otherwise slice past the end of the buffer.
func TestParseEAP_RejectsLengthMismatch(t *testing.T) {
	valid := (&radius.EAPPacket{
		Code: radius.EAPCodeResponse, Identifier: 1,
		Type: radius.EAPTypeMSCHAPv2, Data: []byte("payload"),
	}).Encode()

	t.Run("declared longer than actual", func(t *testing.T) {
		bad := append([]byte(nil), valid...)
		binary.BigEndian.PutUint16(bad[2:4], uint16(len(bad)+50)) //nolint:gosec // deliberately wrong length
		if _, err := radius.ParseEAP(bad); !errors.Is(err, radius.ErrEAPLengthMismatch) {
			t.Errorf("want ErrEAPLengthMismatch, got %v", err)
		}
	})

	t.Run("declared shorter than actual", func(t *testing.T) {
		bad := append([]byte(nil), valid...)
		binary.BigEndian.PutUint16(bad[2:4], 5)
		if _, err := radius.ParseEAP(bad); !errors.Is(err, radius.ErrEAPLengthMismatch) {
			t.Errorf("want ErrEAPLengthMismatch, got %v", err)
		}
	})
}

func TestParseEAP_RejectsTruncatedPackets(t *testing.T) {
	for _, b := range [][]byte{nil, {}, {1}, {1, 2}, {1, 2, 0}} {
		if _, err := radius.ParseEAP(b); err == nil {
			t.Errorf("want an error for a %d-byte packet", len(b))
		}
	}
}

// TestParseMSCHAPv2Response_RoundTrip builds a response the way a supplicant
// would and reads it back.
func TestParseMSCHAPv2Response_RoundTrip(t *testing.T) {
	peerChallenge := bytes.Repeat([]byte{0xAA}, 16)
	ntResponse := bytes.Repeat([]byte{0xBB}, 24)

	value := make([]byte, 0, 49)
	value = append(value, peerChallenge...)
	value = append(value, make([]byte, 8)...) // reserved
	value = append(value, ntResponse...)
	value = append(value, 0x00) // flags

	payload := make([]byte, 0, 5+len(value)+len("alice"))
	payload = append(payload, radius.MSCHAPv2OpResponse, 42, 0, 0, 49)
	payload = append(payload, value...)
	payload = append(payload, []byte("alice")...)
	binary.BigEndian.PutUint16(payload[2:4], uint16(len(payload))) //nolint:gosec // fixed-size test fixture

	got, err := radius.ParseMSCHAPv2Response(payload)
	if err != nil {
		t.Fatalf("ParseMSCHAPv2Response: %v", err)
	}
	if got.MSCHAPv2ID != 42 {
		t.Errorf("MSCHAPv2ID = %d, want 42", got.MSCHAPv2ID)
	}
	if !bytes.Equal(got.PeerChallenge, peerChallenge) {
		t.Errorf("peer challenge mismatch")
	}
	if !bytes.Equal(got.NTResponse, ntResponse) {
		t.Errorf("NT response mismatch")
	}
	if got.Name != "alice" {
		t.Errorf("name = %q, want alice", got.Name)
	}
}

// TestParseMSCHAPv2Response_RejectsMalformed: the fixed 49-byte Value is what
// keeps the hard-coded offsets in range, so a wrong Value-Size must be
// refused rather than trusted.
func TestParseMSCHAPv2Response_RejectsMalformed(t *testing.T) {
	cases := []struct {
		name    string
		payload []byte
	}{
		{"empty", nil},
		{"too short for a header", []byte{2, 1, 0}},
		{"wrong opcode", append([]byte{radius.MSCHAPv2OpChallenge, 1, 0, 0, 49}, make([]byte, 49)...)},
		{"value size not 49", append([]byte{radius.MSCHAPv2OpResponse, 1, 0, 0, 24}, make([]byte, 24)...)},
		{"declares 49 but carries fewer", []byte{radius.MSCHAPv2OpResponse, 1, 0, 0, 49, 0x01, 0x02}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := radius.ParseMSCHAPv2Response(tc.payload); err == nil {
				t.Error("want an error rather than reading past the buffer")
			}
		})
	}
}

// TestMSCHAPv2Challenge_EncodeShape pins the wire layout: supplicants parse
// these offsets literally, so a shifted field silently breaks every client.
func TestMSCHAPv2Challenge_EncodeShape(t *testing.T) {
	challenge := bytes.Repeat([]byte{0xCC}, 16)
	encoded := (&radius.MSCHAPv2Challenge{
		MSCHAPv2ID: 9, Challenge: challenge, Name: "isp-bss-oss",
	}).Encode()

	if encoded[0] != radius.MSCHAPv2OpChallenge {
		t.Errorf("opcode = %d, want %d", encoded[0], radius.MSCHAPv2OpChallenge)
	}
	if encoded[1] != 9 {
		t.Errorf("mschap id = %d, want 9", encoded[1])
	}
	if declared := binary.BigEndian.Uint16(encoded[2:4]); int(declared) != len(encoded) {
		t.Errorf("MS-Length = %d, want %d", declared, len(encoded))
	}
	if encoded[4] != 16 {
		t.Errorf("value-size = %d, want 16", encoded[4])
	}
	if !bytes.Equal(encoded[5:21], challenge) {
		t.Error("challenge bytes are not at the expected offset")
	}
	if string(encoded[21:]) != "isp-bss-oss" {
		t.Errorf("name = %q", encoded[21:])
	}
}

func TestEncodeMSCHAPv2Success_CarriesTheAuthenticatorResponse(t *testing.T) {
	const authResp = "S=407A5589115FD0D6209F510FE9C04566932CDA56"
	encoded := radius.EncodeMSCHAPv2Success(5, authResp)

	if encoded[0] != radius.MSCHAPv2OpSuccess {
		t.Errorf("opcode = %d, want Success", encoded[0])
	}
	if encoded[1] != 5 {
		t.Errorf("mschap id = %d, want 5", encoded[1])
	}
	if got := string(encoded[4:]); got != authResp {
		t.Errorf("payload = %q, want %q", got, authResp)
	}
}

// TestEncodeMSCHAPv2Failure_TellsWindowsNotToRetry: without R=0 a Windows
// supplicant re-prompts in a loop, which presents as a hung client rather
// than a rejected password.
func TestEncodeMSCHAPv2Failure_TellsWindowsNotToRetry(t *testing.T) {
	encoded := radius.EncodeMSCHAPv2Failure(3)
	if encoded[0] != radius.MSCHAPv2OpFailure {
		t.Errorf("opcode = %d, want Failure", encoded[0])
	}
	body := string(encoded[4:])
	if !bytes.Contains([]byte(body), []byte("R=0")) {
		t.Errorf("failure message must carry R=0, got %q", body)
	}
	if !bytes.Contains([]byte(body), []byte("E=691")) {
		t.Errorf("failure message must carry E=691 (auth failure), got %q", body)
	}
}

// TestNewChallengeAndState_AreUnpredictable: a guessable challenge lets an
// attacker precompute responses, and a guessable State lets them attach to
// somebody else's half-finished conversation.
func TestNewChallengeAndState_AreUnpredictable(t *testing.T) {
	const draws = 200

	seenChallenges := make(map[string]bool, draws)
	for i := 0; i < draws; i++ {
		c, err := radius.NewChallenge()
		if err != nil {
			t.Fatalf("NewChallenge: %v", err)
		}
		if len(c) != 16 {
			t.Fatalf("challenge length = %d, want 16", len(c))
		}
		key := string(c)
		if seenChallenges[key] {
			t.Fatal("NewChallenge repeated a value; challenges must be single-use")
		}
		seenChallenges[key] = true
	}

	seenStates := make(map[string]bool, draws)
	for i := 0; i < draws; i++ {
		s, err := radius.NewState()
		if err != nil {
			t.Fatalf("NewState: %v", err)
		}
		if seenStates[s] {
			t.Fatal("NewState repeated a value")
		}
		seenStates[s] = true
	}
}
