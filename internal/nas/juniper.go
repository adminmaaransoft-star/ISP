package nas

import (
	"fmt"

	"layeh.com/radius"
)

// juniperFilterIDType is the standard RFC 2865 attribute 11 (Filter-Id),
// not a vendor-specific one. Juniper (and several other BNG platforms)
// resolve it against a locally provisioned named filter/policer — this is
// the more broadly documented, portable choice over Juniper's
// vendor-specific hierarchical-policer VSA, which varies between JunOS and
// JunOSe. Confirm against the deployed platform (MDS §4.11).
const juniperFilterIDType = radius.Type(11)

type juniperBuilder struct{}

func (juniperBuilder) BuildAccept(p RateProfile) ([]Attr, error) {
	return juniperFilterAttrs(p.ProfileName)
}
func (juniperBuilder) BuildCoA(p RateProfile) ([]Attr, error) {
	return juniperFilterAttrs(p.ProfileName)
}

func juniperFilterAttrs(profileName string) ([]Attr, error) {
	if profileName == "" {
		return nil, fmt.Errorf("nas: juniper: empty QoS profile name — no plan_nas_profiles row for this plan/vendor")
	}
	return []Attr{{Type: juniperFilterIDType, Value: radius.Attribute(profileName)}}, nil
}
