// MD4 is imported here to construct RFC 2759's expected inputs
// independently of the implementation under test — comparing the code
// against its own helper would prove nothing about which encoding it chose.
// Same suppression rationale as mschapv2.go.
//
//nolint:gosec // RFC 2759 test vectors require the RFC's primitives.
package radius_test

import (
	"bytes"
	"encoding/hex"
	"errors"
	"testing"

	"golang.org/x/crypto/md4" //nolint:staticcheck // constructing the RFC's expected input independently

	"github.com/maaransoft/isp-bss-oss/internal/radius"
)

// MS-CHAPv2 crypto tests — FR-AAA-006 | MDS §4.18.
//
// These use the published test vectors from RFC 2759 §9.2 rather than values
// this implementation produced. That distinction is the whole point: a
// self-generated fixture proves the code agrees with itself, which it would
// even if the DES key expansion or the UTF-16 encoding were wrong. Only the
// RFC's own numbers prove it agrees with the Windows supplicants and
// wireless controllers that will actually authenticate against it.

// RFC 2759 §9.2 test vectors.
const (
	tvUserName = "User"
	tvPassword = "clientPass"

	tvAuthenticatorChallenge = "5B5D7C7D7B3F2F3E3C2C602132262628"
	tvPeerChallenge          = "21402324255E262A28295F2B3A337C7E"
	tvChallengeHash          = "D02E4386BCE91226"
	tvNTHash                 = "44EBBA8D5312B8D611474411F56989AE"
	tvNTHashHash             = "41C00C584BD2D91C4017A2A12FA59F3F"
	tvNTResponse             = "82309ECD8D708B5EA08FAA3981CD83544233114A3D85D6DF"
	tvAuthenticatorResponse  = "S=407A5589115FD0D6209F510FE9C04566932CDA56"
)

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("decode %q: %v", s, err)
	}
	return b
}

// TestRFC2759_NTPasswordHash pins the very first step. If UTF-16LE encoding
// were wrong this would already diverge, and every value downstream would be
// wrong in a way that still looked internally consistent.
func TestRFC2759_NTPasswordHash(t *testing.T) {
	got := radius.NTPasswordHash(tvPassword)
	want := mustHex(t, tvNTHash)

	if !bytes.Equal(got, want) {
		t.Errorf("NTPasswordHash(%q):\n got %X\nwant %X", tvPassword, got, want)
	}
	if len(got) != 16 {
		t.Errorf("an NT hash must be 16 bytes, got %d", len(got))
	}
}

// TestRFC2759_GenerateNTResponse is the value a peer actually puts on the
// wire, and therefore the one the server must reproduce exactly.
func TestRFC2759_GenerateNTResponse(t *testing.T) {
	got, err := radius.GenerateNTResponse(
		mustHex(t, tvAuthenticatorChallenge),
		mustHex(t, tvPeerChallenge),
		tvUserName,
		mustHex(t, tvNTHash),
	)
	if err != nil {
		t.Fatalf("GenerateNTResponse: %v", err)
	}
	want := mustHex(t, tvNTResponse)
	if !bytes.Equal(got, want) {
		t.Errorf("NTResponse:\n got %X\nwant %X", got, want)
	}
}

// TestRFC2759_GenerateAuthenticatorResponse covers the mutual-auth half.
// Getting this wrong would not block logins — it would let a rogue AP
// impersonate this server to a supplicant, which is a far quieter failure.
func TestRFC2759_GenerateAuthenticatorResponse(t *testing.T) {
	got, err := radius.GenerateAuthenticatorResponse(
		mustHex(t, tvNTHash),
		mustHex(t, tvNTResponse),
		mustHex(t, tvPeerChallenge),
		mustHex(t, tvAuthenticatorChallenge),
		tvUserName,
	)
	if err != nil {
		t.Fatalf("GenerateAuthenticatorResponse: %v", err)
	}
	if got != tvAuthenticatorResponse {
		t.Errorf("AuthenticatorResponse:\n got %s\nwant %s", got, tvAuthenticatorResponse)
	}
}

// TestRFC2759_EndToEndVerification exercises the path the daemon takes.
func TestRFC2759_EndToEndVerification(t *testing.T) {
	ntHash := radius.NTPasswordHash(tvPassword)

	authResp, err := radius.VerifyMSCHAPv2(
		ntHash,
		mustHex(t, tvAuthenticatorChallenge),
		mustHex(t, tvPeerChallenge),
		mustHex(t, tvNTResponse),
		tvUserName,
	)
	if err != nil {
		t.Fatalf("VerifyMSCHAPv2 with the RFC's own response must succeed: %v", err)
	}
	if authResp != tvAuthenticatorResponse {
		t.Errorf("authenticator response:\n got %s\nwant %s", authResp, tvAuthenticatorResponse)
	}
}

// TestVerifyMSCHAPv2_WrongPasswordIsRejected is the negative control the
// positive vectors are meaningless without.
func TestVerifyMSCHAPv2_WrongPasswordIsRejected(t *testing.T) {
	wrongHash := radius.NTPasswordHash("notThePassword")

	_, err := radius.VerifyMSCHAPv2(
		wrongHash,
		mustHex(t, tvAuthenticatorChallenge),
		mustHex(t, tvPeerChallenge),
		mustHex(t, tvNTResponse),
		tvUserName,
	)
	if !errors.Is(err, radius.ErrBadNTResponse) {
		t.Errorf("want ErrBadNTResponse for a wrong password, got %v", err)
	}
}

// TestVerifyMSCHAPv2_UnenrolledSubscriberIsDistinguishable: "wrong password"
// and "never enrolled for EAP" are different operational problems and the
// caller must be able to tell them apart.
func TestVerifyMSCHAPv2_UnenrolledSubscriberIsDistinguishable(t *testing.T) {
	_, err := radius.VerifyMSCHAPv2(
		nil, // no nt_hash stored
		mustHex(t, tvAuthenticatorChallenge),
		mustHex(t, tvPeerChallenge),
		mustHex(t, tvNTResponse),
		tvUserName,
	)
	if !errors.Is(err, radius.ErrNoNTHash) {
		t.Errorf("want ErrNoNTHash, got %v", err)
	}
}

// TestVerifyMSCHAPv2_ChallengeIsBoundToTheResponse: a response captured from
// one exchange must not verify against a different challenge, or a replayed
// response would authenticate.
func TestVerifyMSCHAPv2_ChallengeIsBoundToTheResponse(t *testing.T) {
	ntHash := radius.NTPasswordHash(tvPassword)
	differentChallenge := mustHex(t, "00112233445566778899AABBCCDDEEFF")

	_, err := radius.VerifyMSCHAPv2(
		ntHash,
		differentChallenge, // not the challenge the response was computed against
		mustHex(t, tvPeerChallenge),
		mustHex(t, tvNTResponse),
		tvUserName,
	)
	if !errors.Is(err, radius.ErrBadNTResponse) {
		t.Errorf("a replayed response against a fresh challenge must fail, got %v", err)
	}
}

// TestVerifyMSCHAPv2_UsernameIsBoundToTheResponse: the username is inside the
// challenge hash, so one subscriber's response must not authenticate another.
func TestVerifyMSCHAPv2_UsernameIsBoundToTheResponse(t *testing.T) {
	ntHash := radius.NTPasswordHash(tvPassword)

	_, err := radius.VerifyMSCHAPv2(
		ntHash,
		mustHex(t, tvAuthenticatorChallenge),
		mustHex(t, tvPeerChallenge),
		mustHex(t, tvNTResponse),
		"SomebodyElse",
	)
	if !errors.Is(err, radius.ErrBadNTResponse) {
		t.Errorf("a response must not verify under a different username, got %v", err)
	}
}

// TestNTPasswordHash_NonASCIIPasswordsUseUTF16LE guards the encoding trap:
// hashing UTF-8 bytes would pass every ASCII test (where the two encodings
// differ only by interleaved zero bytes that MD4 still distinguishes) and
// fail only for subscribers with an accented character.
//
// The expected input bytes are constructed here by hand rather than by
// calling the same helper the implementation uses — comparing the code
// against itself would prove nothing about which encoding it chose.
func TestNTPasswordHash_NonASCIIPasswordsUseUTF16LE(t *testing.T) {
	const password = "café" // 'é' is U+00E9

	// UTF-16LE: each BMP code point as a little-endian 16-bit unit.
	wantInput := []byte{'c', 0, 'a', 0, 'f', 0, 0xE9, 0x00}
	h := md4.New()
	h.Write(wantInput) //nolint:errcheck
	want := h.Sum(nil)

	if got := radius.NTPasswordHash(password); !bytes.Equal(got, want) {
		t.Errorf("NTPasswordHash(%q) did not hash the UTF-16LE encoding:\n got %X\nwant %X", password, got, want)
	}

	// And the UTF-8 encoding of the same string must produce something
	// different, or the assertion above would hold for the wrong reason.
	h2 := md4.New()
	h2.Write([]byte(password)) //nolint:errcheck
	if bytes.Equal(want, h2.Sum(nil)) {
		t.Fatal("UTF-8 and UTF-16LE of this password collide; the test cannot distinguish the encodings")
	}
}

func TestNTPasswordHash_IsDeterministic(t *testing.T) {
	if !bytes.Equal(radius.NTPasswordHash("same"), radius.NTPasswordHash("same")) {
		t.Error("the same password must always produce the same NT hash")
	}
	if bytes.Equal(radius.NTPasswordHash("a"), radius.NTPasswordHash("b")) {
		t.Error("different passwords must produce different hashes")
	}
}

func TestStripDomain(t *testing.T) {
	cases := map[string]string{
		`DOMAIN\user`:      "user",
		`CORP\sub\deep`:    "deep",
		"plainuser":        "plainuser",
		`\leading`:         "leading",
		"user@realm.local": "user@realm.local", // UPN form is left alone
	}
	for in, want := range cases {
		if got := radius.StripDomain(in); got != want {
			t.Errorf("StripDomain(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestGenerateNTResponse_RejectsMalformedInput(t *testing.T) {
	valid := mustHex(t, tvNTHash)
	cases := []struct {
		name                    string
		authChallenge, peerChal []byte
		ntHash                  []byte
	}{
		{"short authenticator challenge", make([]byte, 8), make([]byte, 16), valid},
		{"short peer challenge", make([]byte, 16), make([]byte, 8), valid},
		{"short NT hash", make([]byte, 16), make([]byte, 16), make([]byte, 8)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := radius.GenerateNTResponse(tc.authChallenge, tc.peerChal, "u", tc.ntHash); err == nil {
				t.Error("want an error rather than a silently wrong response")
			}
		})
	}
}
