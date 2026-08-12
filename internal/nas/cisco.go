package nas

import "fmt"

// cisco-avpair: vendor 9 (IANA-registered, certain), sub-attribute type 1
// (certain — this is a stable, widely documented Cisco convention).
// "subscriber:sub-qos-policy-in/out" is the modern IOS-XE ISG/BNG
// convention; older IOS BBA/DSL deployments may instead expect
// "ip:sub-qos-policy-in/out" — confirm against the deployed platform
// (MDS §4.11). Unlike Huawei/ZTE, this is a policy-reference model: the
// named QoS policy-map must already exist on the router.
const (
	ciscoVendorID   = 9
	ciscoAVPairType = 1
)

type ciscoBuilder struct{}

func (ciscoBuilder) BuildAccept(p RateProfile) ([]Attr, error) {
	return ciscoAVPairAttrs(p.ProfileName)
}
func (ciscoBuilder) BuildCoA(p RateProfile) ([]Attr, error) { return ciscoAVPairAttrs(p.ProfileName) }

func ciscoAVPairAttrs(profileName string) ([]Attr, error) {
	if profileName == "" {
		return nil, fmt.Errorf("nas: cisco: empty QoS profile name — no plan_nas_profiles row for this plan/vendor")
	}
	in, err := buildAVPair("subscriber:sub-qos-policy-in=" + profileName)
	if err != nil {
		return nil, err
	}
	out, err := buildAVPair("subscriber:sub-qos-policy-out=" + profileName)
	if err != nil {
		return nil, err
	}
	return []Attr{in, out}, nil
}

func buildAVPair(pair string) (Attr, error) {
	attr, err := buildVendorSubAttr(ciscoVendorID, ciscoAVPairType, []byte(pair))
	if err != nil {
		return Attr{}, fmt.Errorf("nas: cisco: %w", err)
	}
	return attr, nil
}
