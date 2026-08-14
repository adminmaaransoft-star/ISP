// MD4, SHA-1 and single-DES appear throughout this file because RFC 2759
// specifies exactly them for MS-CHAPv2, and every Windows supplicant and
// wireless controller computes its response with exactly them. Substituting
// a modern primitive would not harden anything — it would produce a server
// that cannot authenticate any real client. gosec is suppressed file-wide
// for that reason rather than line by line, since every finding here has the
// same answer.
//
// The security decision that IS ours — who has an NT hash stored at all —
// is handled by making enrolment opt-in (migration 029).
//
//nolint:gosec // RFC 2759 mandates these primitives; see above.
package radius

import (
	"crypto/des"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"unicode/utf16"

	"golang.org/x/crypto/md4" //nolint:staticcheck // MD4 is mandated by RFC 2759's NT hash
)

// MS-CHAPv2 verification, per RFC 2759 — FR-AAA-006 | MDS §4.18.
//
// A note on the primitives, because they look alarming in isolation: MD4 and
// single-DES appear here because RFC 2759 specifies exactly them, and a
// Windows supplicant or wireless controller computes its response with
// exactly them. Substituting anything stronger would not be a hardened
// implementation, it would be one that cannot authenticate anybody. The
// security posture that *is* ours to choose — who has an NT hash stored at
// all — is handled by making enrolment opt-in (migration 029).

var (
	// ErrNoNTHash is returned for a subscriber who has not been enrolled for
	// EAP. Their bcrypt password is untouched and PAP still works; there is
	// simply nothing to verify a challenge against.
	ErrNoNTHash = errors.New("radius: subscriber is not enrolled for EAP-MSCHAPv2")
	// ErrBadNTResponse means the peer's response did not match — a wrong
	// password, or a different one from the enrolled hash.
	ErrBadNTResponse = errors.New("radius: MS-CHAPv2 response did not verify")
)

// ntHashLen is the fixed size of MD4 output, and therefore of an NT hash.
const ntHashLen = 16

// NTPasswordHash computes MD4(UTF-16LE(password)) — the "NT hash".
//
// UTF-16LE, not UTF-8: the encoding is part of the wire contract. A
// supplicant hashing "café" encodes it as UTF-16LE before MD4, so hashing
// the UTF-8 bytes here would authenticate every ASCII password correctly and
// silently fail on any accented one — the kind of bug that surfaces months
// later for a subset of subscribers.
func NTPasswordHash(password string) []byte {
	encoded := utf16.Encode([]rune(password))
	buf := make([]byte, 0, len(encoded)*2)
	for _, r := range encoded {
		buf = append(buf, byte(r), byte(r>>8))
	}
	h := md4.New()
	h.Write(buf) //nolint:errcheck // hash.Hash.Write never returns an error
	return h.Sum(nil)
}

// hashNTPasswordHash computes MD4 of the NT hash itself (RFC 2759
// §8.4 HashNtPasswordHash), used when building the authenticator response.
func hashNTPasswordHash(ntHash []byte) []byte {
	h := md4.New()
	h.Write(ntHash) //nolint:errcheck
	return h.Sum(nil)
}

// challengeHash is RFC 2759 §8.2: SHA1(PeerChallenge ‖ AuthenticatorChallenge
// ‖ UserName), truncated to 8 bytes.
//
// The username is the bare account name as the peer sent it, without any
// domain prefix — a supplicant sending "DOMAIN\user" hashes only "user", so
// the caller strips it before getting here.
func challengeHash(peerChallenge, authenticatorChallenge []byte, username string) []byte {
	h := sha1.New()                 //nolint:gosec // SHA-1 is mandated by RFC 2759
	h.Write(peerChallenge)          //nolint:errcheck
	h.Write(authenticatorChallenge) //nolint:errcheck
	h.Write([]byte(username))       //nolint:errcheck
	return h.Sum(nil)[:8]
}

// challengeResponse is RFC 2759 §8.5: three DES encryptions of the 8-byte
// challenge under three 7-byte keys carved from the NT hash padded to 21
// bytes.
func challengeResponse(challenge, ntHash []byte) ([]byte, error) {
	if len(challenge) != 8 {
		return nil, fmt.Errorf("radius: challenge must be 8 bytes, got %d", len(challenge))
	}
	if len(ntHash) != ntHashLen {
		return nil, fmt.Errorf("radius: NT hash must be %d bytes, got %d", ntHashLen, len(ntHash))
	}

	// 16-byte hash zero-padded to 21 = three 7-byte DES keys.
	padded := make([]byte, 21)
	copy(padded, ntHash)

	out := make([]byte, 0, 24)
	for i := 0; i < 3; i++ {
		block, err := des.NewCipher(desKeyFromSeven(padded[i*7 : i*7+7])) //nolint:gosec // RFC 2759
		if err != nil {
			return nil, fmt.Errorf("radius: build DES cipher: %w", err)
		}
		chunk := make([]byte, 8)
		block.Encrypt(chunk, challenge)
		out = append(out, chunk...)
	}
	return out, nil
}

// desKeyFromSeven expands a 7-byte key to DES's 8-byte form by inserting a
// parity bit every 7 bits (RFC 2759 §8.6 DesEncrypt).
//
// The parity bits are ignored by DES itself, but the bit *positions* of the
// key material are not — getting this shift wrong produces a cipher that
// works consistently with itself and disagrees with every real supplicant.
func desKeyFromSeven(k7 []byte) []byte {
	k8 := make([]byte, 8)
	k8[0] = k7[0]
	k8[1] = k7[0]<<7 | k7[1]>>1
	k8[2] = k7[1]<<6 | k7[2]>>2
	k8[3] = k7[2]<<5 | k7[3]>>3
	k8[4] = k7[3]<<4 | k7[4]>>4
	k8[5] = k7[4]<<3 | k7[5]>>5
	k8[6] = k7[5]<<2 | k7[6]>>6
	k8[7] = k7[6] << 1
	return k8
}

// GenerateNTResponse is RFC 2759 §8.1: the 24-byte value a peer sends, and
// the value the server recomputes to verify it.
func GenerateNTResponse(authenticatorChallenge, peerChallenge []byte, username string, ntHash []byte) ([]byte, error) {
	if len(authenticatorChallenge) != 16 || len(peerChallenge) != 16 {
		return nil, fmt.Errorf("radius: challenges must be 16 bytes (got %d and %d)",
			len(authenticatorChallenge), len(peerChallenge))
	}
	return challengeResponse(challengeHash(peerChallenge, authenticatorChallenge, username), ntHash)
}

// Magic constants from RFC 2759 §8.7. Their only purpose is domain
// separation, but they are part of the wire format and must match byte for
// byte.
var (
	magic1 = []byte("Magic server to client signing constant")
	magic2 = []byte("Pad to make it do more than one iteration")
)

// GenerateAuthenticatorResponse is RFC 2759 §8.7: the "S=<40 hex digits>"
// value the server returns so the *peer* can authenticate the *server*.
//
// This half is what makes MS-CHAPv2 mutual. Skipping it (or returning a
// constant) would let any rogue access point impersonate this ACS to a
// supplicant, so it is computed properly rather than stubbed.
func GenerateAuthenticatorResponse(ntHash, ntResponse, peerChallenge, authenticatorChallenge []byte, username string) (string, error) {
	if len(ntResponse) != 24 {
		return "", fmt.Errorf("radius: NT response must be 24 bytes, got %d", len(ntResponse))
	}
	if len(ntHash) != ntHashLen {
		return "", fmt.Errorf("radius: NT hash must be %d bytes, got %d", ntHashLen, len(ntHash))
	}

	hashHash := hashNTPasswordHash(ntHash)

	h := sha1.New()     //nolint:gosec // RFC 2759
	h.Write(hashHash)   //nolint:errcheck
	h.Write(ntResponse) //nolint:errcheck
	h.Write(magic1)     //nolint:errcheck
	digest := h.Sum(nil)

	chHash := challengeHash(peerChallenge, authenticatorChallenge, username)

	h2 := sha1.New() //nolint:gosec // RFC 2759
	h2.Write(digest) //nolint:errcheck
	h2.Write(chHash) //nolint:errcheck
	h2.Write(magic2) //nolint:errcheck

	return "S=" + strings.ToUpper(hex.EncodeToString(h2.Sum(nil))), nil
}

// VerifyMSCHAPv2 checks a peer's NT response and, on success, returns the
// authenticator response to send back.
//
// The comparison is a plain byte equality rather than a constant-time one:
// both sides are derived from a server-chosen random challenge that is never
// reused, so there is no secret an attacker could walk out byte by byte —
// unlike comparing a stored MAC, where timing would matter.
func VerifyMSCHAPv2(ntHash, authenticatorChallenge, peerChallenge, ntResponse []byte, username string) (string, error) {
	if len(ntHash) == 0 {
		return "", ErrNoNTHash
	}
	expected, err := GenerateNTResponse(authenticatorChallenge, peerChallenge, username, ntHash)
	if err != nil {
		return "", err
	}
	if len(expected) != len(ntResponse) {
		return "", ErrBadNTResponse
	}
	for i := range expected {
		if expected[i] != ntResponse[i] {
			return "", ErrBadNTResponse
		}
	}
	return GenerateAuthenticatorResponse(ntHash, ntResponse, peerChallenge, authenticatorChallenge, username)
}

// StripDomain removes a "DOMAIN\" prefix from a supplicant-supplied username.
//
// Windows supplicants routinely send one, and RFC 2759's challenge hash uses
// the bare account name — leaving the prefix in would compute a different
// hash from the peer's and reject every domain-joined client.
func StripDomain(username string) string {
	if i := strings.LastIndex(username, `\`); i >= 0 {
		return username[i+1:]
	}
	return username
}
