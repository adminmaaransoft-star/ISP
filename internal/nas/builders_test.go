package nas

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"layeh.com/radius"
)

// decodeVSA unwraps a Vendor-Specific attribute back to its vendor ID and
// raw sub-attribute TLV bytes, the inverse of buildVendorSubAttr /
// radius.NewVendorSpecific, so tests can assert on what was actually built
// rather than trusting the builder that produced it.
func decodeVSA(t *testing.T, a Attr) (vendorID uint32, subType byte, value []byte) {
	t.Helper()
	if a.Type != 26 {
		t.Fatalf("attribute type = %d, want 26 (Vendor-Specific)", a.Type)
	}
	vendorID, sub, err := radius.VendorSpecific(a.Value)
	if err != nil {
		t.Fatalf("decode vendor-specific: %v", err)
	}
	if len(sub) < 2 {
		t.Fatalf("sub-attribute too short: %d bytes", len(sub))
	}
	return vendorID, sub[0], sub[2:]
}

func TestMikrotikBuilder_MatchesOriginalInlineEncoding(t *testing.T) {
	// This is a byte-for-byte regression test: internal/radius/handlers.go
	// and internal/fup/coa_task.go used to hand-encode this exact VSA
	// inline before it moved here. Any drift changes what every MikroTik
	// NAS in production receives.
	attrs, err := mikrotikBuilder{}.BuildAccept(RateProfile{RateLimitString: "50M/50M"})
	if err != nil {
		t.Fatalf("BuildAccept: %v", err)
	}
	if len(attrs) != 1 {
		t.Fatalf("want 1 attribute, got %d", len(attrs))
	}

	vendorID, subType, value := decodeVSA(t, attrs[0])
	if vendorID != 14988 {
		t.Errorf("vendor ID = %d, want 14988", vendorID)
	}
	if subType != 8 {
		t.Errorf("sub-type = %d, want 8 (Mikrotik-Rate-Limit)", subType)
	}
	if string(value) != "50M/50M" {
		t.Errorf("value = %q, want %q", value, "50M/50M")
	}
}

func TestMikrotikBuilder_EmptyRateLimitErrors(t *testing.T) {
	if _, err := (mikrotikBuilder{}).BuildAccept(RateProfile{}); err == nil {
		t.Fatal("want an error for an empty rate limit string")
	}
}

func TestHuaweiBuilder_EncodesInputOutputAsBigEndianBps(t *testing.T) {
	attrs, err := huaweiBuilder{}.BuildAccept(RateProfile{RateLimitString: "10M/5M"})
	if err != nil {
		t.Fatalf("BuildAccept: %v", err)
	}
	if len(attrs) != 2 {
		t.Fatalf("want 2 attributes (input + output), got %d", len(attrs))
	}

	vendorID, subType, value := decodeVSA(t, attrs[0])
	if vendorID != huaweiVendorID || subType != huaweiInputAvgRateType {
		t.Errorf("attrs[0] = vendor %d sub-type %d, want %d/%d", vendorID, subType, huaweiVendorID, huaweiInputAvgRateType)
	}
	if got := beToUint32(value); got != 10_000_000 {
		t.Errorf("input rate = %d bps, want 10,000,000 (10M)", got)
	}

	_, subType2, value2 := decodeVSA(t, attrs[1])
	if subType2 != huaweiOutputAvgRateType {
		t.Errorf("attrs[1] sub-type = %d, want %d", subType2, huaweiOutputAvgRateType)
	}
	if got := beToUint32(value2); got != 5_000_000 {
		t.Errorf("output rate = %d bps, want 5,000,000 (5M)", got)
	}
}

func TestZTEBuilder_EncodesRateAsBigEndianBps(t *testing.T) {
	attrs, err := zteBuilder{}.BuildCoA(RateProfile{RateLimitString: "1G/1G"})
	if err != nil {
		t.Fatalf("BuildCoA: %v", err)
	}
	vendorID, _, value := decodeVSA(t, attrs[0])
	if vendorID != zteVendorID {
		t.Errorf("vendor ID = %d, want %d", vendorID, zteVendorID)
	}
	if got := beToUint32(value); got != 1_000_000_000 {
		t.Errorf("rate = %d bps, want 1,000,000,000 (1G)", got)
	}
}

func TestCiscoBuilder_SendsInAndOutAVPairs(t *testing.T) {
	attrs, err := ciscoBuilder{}.BuildAccept(RateProfile{ProfileName: "PLAN_50M"})
	if err != nil {
		t.Fatalf("BuildAccept: %v", err)
	}
	if len(attrs) != 2 {
		t.Fatalf("want 2 av-pairs (in + out), got %d", len(attrs))
	}

	vendorID, subType, in := decodeVSA(t, attrs[0])
	if vendorID != ciscoVendorID || subType != ciscoAVPairType {
		t.Errorf("attrs[0] = vendor %d sub-type %d, want %d/%d", vendorID, subType, ciscoVendorID, ciscoAVPairType)
	}
	if string(in) != "subscriber:sub-qos-policy-in=PLAN_50M" {
		t.Errorf("in av-pair = %q", in)
	}
	_, _, out := decodeVSA(t, attrs[1])
	if string(out) != "subscriber:sub-qos-policy-out=PLAN_50M" {
		t.Errorf("out av-pair = %q", out)
	}
}

func TestCiscoBuilder_EmptyProfileNameErrors(t *testing.T) {
	if _, err := (ciscoBuilder{}).BuildAccept(RateProfile{}); err == nil {
		t.Fatal("want an error when no plan_nas_profiles row exists (empty ProfileName)")
	}
}

func TestJuniperBuilder_UsesStandardFilterID(t *testing.T) {
	attrs, err := juniperBuilder{}.BuildAccept(RateProfile{ProfileName: "PLAN_50M"})
	if err != nil {
		t.Fatalf("BuildAccept: %v", err)
	}
	if len(attrs) != 1 {
		t.Fatalf("want 1 attribute, got %d", len(attrs))
	}
	if attrs[0].Type != 11 {
		t.Errorf("attribute type = %d, want 11 (Filter-Id), not a Vendor-Specific wrapper", attrs[0].Type)
	}
	if string(attrs[0].Value) != "PLAN_50M" {
		t.Errorf("value = %q, want %q", attrs[0].Value, "PLAN_50M")
	}
}

func TestWirelessBuilders_SendProfileNameAsVendorSubAttr(t *testing.T) {
	cases := []struct {
		name     string
		builder  AttributeBuilder
		vendorID uint32
		subType  byte
	}{
		{"cisco_wlc", ciscoWLCBuilder{}, ciscoWLCVendorID, ciscoWLCQoSType},
		{"aruba", arubaBuilder{}, arubaVendorID, arubaUserRoleType},
		{"ruckus", ruckusBuilder{}, ruckusVendorID, ruckusRoleType},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			attrs, err := tc.builder.BuildAccept(RateProfile{ProfileName: "GUEST_ROLE"})
			if err != nil {
				t.Fatalf("BuildAccept: %v", err)
			}
			vendorID, subType, value := decodeVSA(t, attrs[0])
			if vendorID != tc.vendorID {
				t.Errorf("vendor ID = %d, want %d", vendorID, tc.vendorID)
			}
			if subType != tc.subType {
				t.Errorf("sub-type = %d, want %d", subType, tc.subType)
			}
			if string(value) != "GUEST_ROLE" {
				t.Errorf("value = %q", value)
			}
			if _, err := tc.builder.BuildAccept(RateProfile{}); err == nil {
				t.Error("want an error for an empty profile name")
			}
		})
	}
}

func TestBuilderFor_UnknownVendorFallsBackToMikrotik(t *testing.T) {
	got := BuilderFor(Vendor("some-future-vendor-not-yet-known"))
	want := BuilderFor(VendorMikrotik)
	if got != want {
		t.Errorf("unknown vendor did not resolve to the MikroTik builder")
	}
}

func TestBuildAcceptAttrs_IncrementsErrorMetricOnFailure(t *testing.T) {
	counter := nasAttributeBuildErrorsTotal.WithLabelValues(string(VendorCisco), "accept")
	before := testutil.ToFloat64(counter)

	if _, err := BuildAcceptAttrs(VendorCisco, RateProfile{}); err == nil {
		t.Fatal("want an error: cisco requires a ProfileName")
	}

	after := testutil.ToFloat64(counter)
	if after != before+1 {
		t.Errorf("nas_attribute_build_errors_total{cisco,accept} = %v, want %v", after, before+1)
	}
}

func TestParseRateLimitString(t *testing.T) {
	cases := []struct {
		in         string
		wantFirst  uint32
		wantSecond uint32
		wantErr    bool
	}{
		{"50M/50M", 50_000_000, 50_000_000, false},
		{"10M/5M", 10_000_000, 5_000_000, false},
		{"1G/1G", 1_000_000_000, 1_000_000_000, false},
		{"512K/256K", 512_000, 256_000, false},
		{"100/200", 100, 200, false}, // no suffix = bits/sec
		{"50M", 0, 0, true},          // missing slash
		{"", 0, 0, true},
		{"abc/50M", 0, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			first, second, err := parseRateLimitString(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want an error for %q", tc.in)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseRateLimitString(%q): %v", tc.in, err)
			}
			if first != tc.wantFirst || second != tc.wantSecond {
				t.Errorf("parseRateLimitString(%q) = (%d, %d), want (%d, %d)", tc.in, first, second, tc.wantFirst, tc.wantSecond)
			}
		})
	}
}

func beToUint32(b []byte) uint32 {
	if len(b) != 4 {
		return 0
	}
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}
