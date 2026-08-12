package nas

import (
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
