package nas

import "fmt"

// Wireless controllers are, across every major brand, role/contract-based
// rather than raw-numeric — the controller has pre-configured bandwidth
// contracts or roles, and RADIUS selects one by name. They use RateProfile.
// ProfileName, the same as Cisco/Juniper, not RateLimitString.
const (
	// Cisco WLC inherited the Airespace vendor ID (14179) from the
	// acquisition rather than using Cisco's own (9) — it is a genuinely
	// different dictionary, not an alias. Sub-type for the bandwidth
	// contract / QoS-level reference is a TODO-VERIFY: public documentation
	// gives inconsistent sub-type numbers across AireOS vs Cisco IOS-XE WLC
	// software (MDS §4.11).
	ciscoWLCVendorID = 14179
	ciscoWLCQoSType  = 2 // TODO-VERIFY against deployed controller software

	// Aruba-User-Role (vendor 14823, sub-type 1) is a well-documented,
	// stable attribute across ArubaOS and ClearPass-managed controllers.
	arubaVendorID     = 14823
	arubaUserRoleType = 1

	// Ruckus vendor ID is IANA-registered (25053) but the bandwidth/role
	// attribute sub-type is genuinely uncertain without vendor
	// documentation in hand — treat this builder as structurally correct
	// but unverified pending confirmation against real hardware (MDS §4.11).
	ruckusVendorID = 25053
	ruckusRoleType = 1 // TODO-VERIFY against deployed controller software
)

type ciscoWLCBuilder struct{}

func (ciscoWLCBuilder) BuildAccept(p RateProfile) ([]Attr, error) {
	return wirelessProfileAttrs("cisco_wlc", ciscoWLCVendorID, ciscoWLCQoSType, p.ProfileName)
}
func (ciscoWLCBuilder) BuildCoA(p RateProfile) ([]Attr, error) {
	return wirelessProfileAttrs("cisco_wlc", ciscoWLCVendorID, ciscoWLCQoSType, p.ProfileName)
}

type arubaBuilder struct{}

func (arubaBuilder) BuildAccept(p RateProfile) ([]Attr, error) {
	return wirelessProfileAttrs("aruba", arubaVendorID, arubaUserRoleType, p.ProfileName)
}
func (arubaBuilder) BuildCoA(p RateProfile) ([]Attr, error) {
	return wirelessProfileAttrs("aruba", arubaVendorID, arubaUserRoleType, p.ProfileName)
}

type ruckusBuilder struct{}

func (ruckusBuilder) BuildAccept(p RateProfile) ([]Attr, error) {
	return wirelessProfileAttrs("ruckus", ruckusVendorID, ruckusRoleType, p.ProfileName)
}
func (ruckusBuilder) BuildCoA(p RateProfile) ([]Attr, error) {
	return wirelessProfileAttrs("ruckus", ruckusVendorID, ruckusRoleType, p.ProfileName)
}

func wirelessProfileAttrs(vendorName string, vendorID uint32, subType byte, profileName string) ([]Attr, error) {
	if profileName == "" {
		return nil, fmt.Errorf("nas: %s: empty QoS profile/role name — no plan_nas_profiles row for this plan/vendor", vendorName)
	}
	attr, err := buildVendorSubAttr(vendorID, subType, []byte(profileName))
	if err != nil {
		return nil, fmt.Errorf("nas: %s: %w", vendorName, err)
	}
	return []Attr{attr}, nil
}
