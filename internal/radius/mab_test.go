package radius_test

import (
	"testing"

	"github.com/maaransoft/isp-bss-oss/internal/nas"
	"github.com/maaransoft/isp-bss-oss/internal/radius"
)

// MAC Auth Bypass unit tests — FR-HSP-002 | MDS §4.23.
//
// MAB trades a credential for convenience, so the tests here are about the
// boundaries that keep that trade bounded rather than about the happy path.

func TestFR_HSP_002_NormaliseMAC(t *testing.T) {
	// Every spelling a real NAS sends must land on one canonical form, or the
	// same physical device can be registered twice and authenticate under a
	// spelling nobody reviewed.
	same := []string{
		"AA:BB:CC:DD:EE:FF",
		"aa:bb:cc:dd:ee:ff",
		"aa-bb-cc-dd-ee-ff",
		"aabb.ccdd.eeff",
		"aabbccddeeff",
		"AABBCCDDEEFF",
	}
	for _, in := range same {
		got, ok := radius.NormaliseMAC(in)
		if !ok {
			t.Errorf("NormaliseMAC(%q) rejected a valid MAC", in)
			continue
		}
		if got != "AA:BB:CC:DD:EE:FF" {
			t.Errorf("NormaliseMAC(%q) = %q, want AA:BB:CC:DD:EE:FF", in, got)
		}
	}

	for _, bad := range []string{
		"", "not-a-mac", "AA:BB:CC:DD:EE", "AA:BB:CC:DD:EE:FF:00",
		"GG:BB:CC:DD:EE:FF", "subscriber@isp", "12345",
	} {
		if _, ok := radius.NormaliseMAC(bad); ok {
			t.Errorf("NormaliseMAC(%q) accepted a non-MAC", bad)
		}
	}
}

// TestFR_HSP_002_UnregisteredNASNeverAllowsMAB is the property the whole
// opt-in design rests on. nas.Resolver falls back to a synthetic Device for an
// IP with no nas_devices row, and if that fallback ever carried AllowMAB the
// opt-in would be silently global on exactly the deployments that never
// registered their NAS inventory.
func TestFR_HSP_002_UnregisteredNASNeverAllowsMAB(t *testing.T) {
	r := nas.NewResolver(nil, nil, []byte("secret"), 3799)

	device := r.Resolve("203.0.113.99") // never registered
	if device.AllowMAB {
		t.Fatal("an unregistered NAS must never permit MAB — the fallback Device would make " +
			"the per-NAS opt-in global for anyone who has not registered their NAS inventory")
	}

	// And the zero value of the struct itself, in case a future constructor
	// builds one without going through Resolve.
	var zero nas.Device
	if zero.AllowMAB {
		t.Error("the zero value of nas.Device must not permit MAB")
	}
}
