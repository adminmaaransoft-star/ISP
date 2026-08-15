package nas

import (
	"sort"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// nasUnclassifiedTotal counts NAS traffic seen with no nas_devices row,
	// currently served on the MikroTik-fallback default (MDS §4.11 rollout
	// note). Gives NOC an actionable list of devices worth registering
	// instead of a silent assumption.
	nasUnclassifiedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "nas_unclassified_total",
		Help: "NAS IPs seen with no nas_devices row, served on the MikroTik fallback default",
	}, []string{"nas_ip"})

	// nasAttributeBuildErrorsTotal counts failures building a vendor
	// attribute — e.g. a reference-vendor plan with no matching
	// plan_nas_profiles row. This must never be silent: it is exactly the
	// class of bug this package exists to stop reproducing.
	nasAttributeBuildErrorsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "nas_attribute_build_errors_total",
		Help: "Vendor attribute construction failures, by vendor and reason",
	}, []string{"vendor", "reason"})
)

var builders = map[Vendor]AttributeBuilder{
	VendorMikrotik: mikrotikBuilder{},
	VendorHuawei:   huaweiBuilder{},
	VendorZTE:      zteBuilder{},
	VendorCisco:    ciscoBuilder{},
	VendorJuniper:  juniperBuilder{},
	VendorCiscoWLC: ciscoWLCBuilder{},
	VendorAruba:    arubaBuilder{},
	VendorRuckus:   ruckusBuilder{},
}

// KnownVendor reports whether vendor has a builder in this package.
//
// The set here and the CHECK constraint on nas_devices.vendor (migration 022)
// must agree. Validating at the API edge turns a vendor typo into a readable
// 422 instead of either a constraint violation surfacing as a 500, or — worse —
// a row that stores fine and then silently authenticates on the MikroTik
// fallback that BuilderFor applies to anything it does not recognise.
func KnownVendor(vendor Vendor) bool {
	_, ok := builders[vendor]
	return ok
}

// Vendors returns the supported vendor values, sorted, for error messages and
// operator-facing listings.
func Vendors() []string {
	out := make([]string, 0, len(builders))
	for v := range builders {
		out = append(out, string(v))
	}
	sort.Strings(out)
	return out
}

// BuilderFor returns the AttributeBuilder for vendor. An unrecognized
// vendor value (which should not happen given the DB CHECK constraint, but
// a stale binary reading a newer vendor enum value is possible during a
// rolling upgrade) falls back to MikroTik, the same safe default as an
// unregistered NAS.
func BuilderFor(vendor Vendor) AttributeBuilder {
	if b, ok := builders[vendor]; ok {
		return b
	}
	return builders[VendorMikrotik]
}

// BuildAcceptAttrs and BuildCoAAttrs are the entry points callers (internal/
// radius, internal/fup) should use rather than calling BuilderFor directly —
// they add the nas_attribute_build_errors_total accounting so a build
// failure is always a metric, never a silent no-op.

func BuildAcceptAttrs(vendor Vendor, p RateProfile) ([]Attr, error) {
	attrs, err := BuilderFor(vendor).BuildAccept(p)
	if err != nil {
		nasAttributeBuildErrorsTotal.WithLabelValues(string(vendor), "accept").Inc()
	}
	return attrs, err
}

func BuildCoAAttrs(vendor Vendor, p RateProfile) ([]Attr, error) {
	attrs, err := BuilderFor(vendor).BuildCoA(p)
	if err != nil {
		nasAttributeBuildErrorsTotal.WithLabelValues(string(vendor), "coa").Inc()
	}
	return attrs, err
}
