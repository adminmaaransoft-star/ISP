// Package nas resolves per-NAS RADIUS behavior: which vendor's bandwidth
// attribute to send, which shared secret to verify a packet against, and
// which CoA/PoD port to target.
//
// Before this package existed, internal/radius and internal/fup each
// hand-encoded one hardcoded MikroTik vendor-specific attribute (vendor
// 14988) into every Access-Accept and CoA packet, and every NAS shared one
// global RADIUS secret. Any Cisco, Juniper, Huawei, ZTE, or wireless-
// controller NAS authenticated subscribers correctly but received an
// attribute it does not understand — RADIUS silently ignores unrecognized
// vendor attributes, so the subscriber connected at whatever the NAS's own
// default was, never their plan speed, with no error anywhere.
//
// FR: FR-NAS-001..004 | MDS §4.11
package nas

import "layeh.com/radius"

// Vendor identifies which attribute dialect a NAS speaks. Distinct wireless
// controller brands are separate values, not one "wireless_generic" bucket:
// Cisco WLC inherited Airespace's vendor ID (14179) rather than Cisco's own
// (9), and Aruba/Ruckus each have their own vendor-specific dictionaries —
// they cannot share a builder.
type Vendor string

const (
	VendorMikrotik Vendor = "mikrotik"
	VendorHuawei   Vendor = "huawei"
	VendorZTE      Vendor = "zte"
	VendorCisco    Vendor = "cisco"
	VendorJuniper  Vendor = "juniper"
	VendorCiscoWLC Vendor = "cisco_wlc"
	VendorAruba    Vendor = "aruba"
	VendorRuckus   Vendor = "ruckus"
)

// RateProfile carries both representations a builder might need. Dynamic-
// rate vendors (MikroTik, Huawei, ZTE) read RateLimitString directly.
// Policy-reference vendors (Cisco, Juniper, the wireless controllers) read
// ProfileName instead — RADIUS never sends them a raw number, only the name
// of a QoS policy the NAS already has provisioned locally (MDS §4.11).
type RateProfile struct {
	RateLimitString string // e.g. "50M/50M" — plans.rate_limit_string
	ProfileName     string // pre-provisioned NAS-side policy name, from plan_nas_profiles
}

// Attr is a single (type, value) RADIUS attribute pair, ready for
// (*radius.Packet).Add.
type Attr struct {
	Type  radius.Type
	Value radius.Attribute
}

// AttributeBuilder constructs the vendor-specific attributes for one NAS
// dialect. PoD (Disconnect-Request) is deliberately not part of this
// interface: RFC 3576 Disconnect-Request needs only Acct-Session-Id, no
// vendor attribute, for any vendor.
type AttributeBuilder interface {
	BuildAccept(p RateProfile) ([]Attr, error)
	BuildCoA(p RateProfile) ([]Attr, error)
}

// Device is a resolved, ready-to-use NAS: secret already decrypted, vendor
// already known.
type Device struct {
	IP      string
	Vendor  Vendor
	Secret  []byte
	CoAPort int
	PoDPort int
}
