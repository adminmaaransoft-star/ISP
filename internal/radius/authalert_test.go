// Per-NAS auth failure alerting — FR-OBS-005 | SAD §3.2.
//
// The rule is easy to state and easy to get subtly wrong. The two failure
// modes that matter are opposites: computing the rate from cumulative
// counters, which averages over the process lifetime and never recovers; and
// alerting on tiny samples, which fires forever on an idle NAS and teaches
// everyone to ignore it.
package radius

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"layeh.com/radius"
)

// ── Doubles ─────────────────────────────────────────────────────────────────

type recordingAlerter struct {
	mu     sync.Mutex
	events []alertEvent
}

type alertEvent struct {
	Name   string
	Detail map[string]any
}

func (a *recordingAlerter) Trigger(event string, detail any) {
	a.mu.Lock()
	defer a.mu.Unlock()
	d, _ := detail.(map[string]any)
	a.events = append(a.events, alertEvent{event, d})
}

func (a *recordingAlerter) names() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]string, 0, len(a.events))
	for _, e := range a.events {
		out = append(out, e.Name)
	}
	return out
}

func (a *recordingAlerter) count(name string) int {
	n := 0
	for _, e := range a.names() {
		if e == name {
			n++
		}
	}
	return n
}

// authFixture drives the monitor against an isolated registry, so these tests
// neither see nor disturb counters other tests in this package increment.
type authFixture struct {
	monitor *AuthFailureMonitor
	alerter *recordingAlerter
	vec     *prometheus.CounterVec
	now     time.Time
}

func newAuthFixture(t *testing.T) *authFixture {
	t.Helper()
	reg := prometheus.NewRegistry()
	vec := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "radius_auth_outcome_total",
		Help: "test",
	}, []string{"nas", "result"})
	reg.MustRegister(vec)

	alerter := &recordingAlerter{}
	return &authFixture{
		monitor: NewAuthFailureMonitor(alerter, reg),
		alerter: alerter,
		vec:     vec,
		now:     time.Now(),
	}
}

// observe records outcomes then evaluates, advancing the clock so successive
// readings land inside the window.
//
// Both series are touched with Add(0) first. Prometheus creates a series only
// on its first increment, so without this a NAS whose baseline reading is
// "nothing yet" has no series to read and the monitor sees it appear from
// nowhere one tick later — leaving a single sample, which is not enough to
// difference. That is correct behaviour (a rate needs two readings, exactly as
// Prometheus cannot rate() a series' first sample) but it is not the situation
// under test here, which is a NAS that has been running and then goes bad.
func (f *authFixture) observe(nasIP string, accepts, rejects int, advance time.Duration) {
	f.vec.WithLabelValues(nasIP, "accept").Add(0)
	f.vec.WithLabelValues(nasIP, "reject").Add(0)
	for i := 0; i < accepts; i++ {
		f.vec.WithLabelValues(nasIP, "accept").Inc()
	}
	for i := 0; i < rejects; i++ {
		f.vec.WithLabelValues(nasIP, "reject").Inc()
	}
	f.now = f.now.Add(advance)
	f.monitor.CheckOnce(f.now)
}

// ── Tests ───────────────────────────────────────────────────────────────────

// TestFR_OBS_005_AlertsWhenOneNASExceedsTheThreshold is the requirement.
func TestFR_OBS_005_AlertsWhenOneNASExceedsTheThreshold(t *testing.T) {
	f := newAuthFixture(t)

	// A baseline reading, then a burst that is 80% rejects.
	f.observe("10.0.0.1", 0, 0, 0)
	f.observe("10.0.0.1", 20, 80, time.Minute)

	if f.alerter.count(AuthFailureAlertEvent) != 1 {
		t.Fatalf("want one alert, got %v", f.alerter.names())
	}
	detail := f.alerter.events[0].Detail
	if detail["nas"] != "10.0.0.1" {
		t.Errorf("the alert must name the NAS, got %v", detail["nas"])
	}
	// Naming which NAS is the whole point: a global counter could already say
	// "failures are up" and left an operator with nowhere to look.
	if rate, _ := detail["failure_rate"].(float64); rate < 0.79 || rate > 0.81 {
		t.Errorf("failure_rate: want ~0.80, got %v", rate)
	}
}

// TestFR_OBS_005_HealthyNASDoesNotAlert — 10% failures is normal traffic:
// wrong passwords happen.
func TestFR_OBS_005_HealthyNASDoesNotAlert(t *testing.T) {
	f := newAuthFixture(t)

	f.observe("10.0.0.2", 0, 0, 0)
	f.observe("10.0.0.2", 90, 10, time.Minute)

	if got := f.alerter.names(); len(got) != 0 {
		t.Errorf("a 10%% failure rate is ordinary traffic and must not alert, got %v", got)
	}
}

// TestFR_OBS_005_QuietNASDoesNotAlertOnTinySamples is the noise guard. One
// failed authentication on an idle NAS is a 100% failure rate.
func TestFR_OBS_005_QuietNASDoesNotAlertOnTinySamples(t *testing.T) {
	f := newAuthFixture(t)

	f.observe("10.0.0.3", 0, 0, 0)
	f.observe("10.0.0.3", 0, 3, time.Minute)

	if got := f.alerter.names(); len(got) != 0 {
		t.Errorf("three failed attempts is not a failure rate; alerting on it would fire "+
			"constantly on quiet sites until nobody reads the alerts, got %v", got)
	}
}

// TestFR_OBS_005_OnlyTheFailingNASIsNamed — the requirement says "on any NAS",
// so a healthy neighbour must not be swept into the alert.
func TestFR_OBS_005_OnlyTheFailingNASIsNamed(t *testing.T) {
	f := newAuthFixture(t)

	f.observe("10.0.0.1", 0, 0, 0)
	f.observe("10.0.0.2", 100, 0, 0)

	// One NAS goes bad while the other keeps working.
	for i := 0; i < 90; i++ {
		f.vec.WithLabelValues("10.0.0.1", "reject").Inc()
	}
	for i := 0; i < 100; i++ {
		f.vec.WithLabelValues("10.0.0.2", "accept").Inc()
	}
	f.now = f.now.Add(time.Minute)
	f.monitor.CheckOnce(f.now)

	if f.alerter.count(AuthFailureAlertEvent) != 1 {
		t.Fatalf("want exactly one alert, got %v", f.alerter.names())
	}
	if nas := f.alerter.events[0].Detail["nas"]; nas != "10.0.0.1" {
		t.Errorf("the wrong NAS was named: %v", nas)
	}
}

// TestFR_OBS_005_RateIsWindowedNotCumulative is the subtle one. The counters
// never reset, so an outage an hour ago must not keep the alert firing, and a
// long healthy history must not dilute a failure happening right now.
func TestFR_OBS_005_RateIsWindowedNotCumulative(t *testing.T) {
	f := newAuthFixture(t)
	f.monitor.SetWindow(5 * time.Minute)

	// An outage: 100 rejects.
	f.observe("10.0.0.4", 0, 0, 0)
	f.observe("10.0.0.4", 0, 100, time.Minute)
	if f.alerter.count(AuthFailureAlertEvent) != 1 {
		t.Fatalf("the outage should alert, got %v", f.alerter.names())
	}

	// It recovers and stays healthy for long enough that the bad samples fall
	// out of the window.
	for i := 0; i < 6; i++ {
		f.observe("10.0.0.4", 100, 0, time.Minute)
	}

	if f.alerter.count(AuthFailureAlertEvent+"_resolved") == 0 {
		t.Error("a recovered NAS must clear its alert — an alert that only ever fires teaches " +
			"people to stop trusting it")
	}
	// And it must not re-fire on the strength of history alone.
	if f.alerter.count(AuthFailureAlertEvent) != 1 {
		t.Errorf("a cumulative rate would keep the old outage alive forever, got %v", f.alerter.names())
	}
}

// TestFR_OBS_005_SustainedOutageAlertsOnce — a NAS down for an hour should
// produce one alert, not one per evaluation.
func TestFR_OBS_005_SustainedOutageAlertsOnce(t *testing.T) {
	f := newAuthFixture(t)

	f.observe("10.0.0.5", 0, 0, 0)
	for i := 0; i < 5; i++ {
		f.observe("10.0.0.5", 0, 100, time.Minute)
	}

	if got := f.alerter.count(AuthFailureAlertEvent); got != 1 {
		t.Errorf("a sustained outage must alert once, not once per tick: got %d", got)
	}
}

// TestFR_OBS_005_RecoveryIsReportedThenCanAlertAgain — the alert has to be
// re-armed, or a second outage after a recovery goes unreported.
func TestFR_OBS_005_RecoveryIsReportedThenCanAlertAgain(t *testing.T) {
	f := newAuthFixture(t)
	f.monitor.SetWindow(3 * time.Minute)

	f.observe("10.0.0.6", 0, 0, 0)
	f.observe("10.0.0.6", 0, 100, time.Minute)
	for i := 0; i < 4; i++ {
		f.observe("10.0.0.6", 100, 0, time.Minute)
	}
	if f.alerter.count(AuthFailureAlertEvent+"_resolved") == 0 {
		t.Fatal("expected a resolution")
	}

	// Second outage.
	for i := 0; i < 4; i++ {
		f.observe("10.0.0.6", 0, 100, time.Minute)
	}
	if got := f.alerter.count(AuthFailureAlertEvent); got != 2 {
		t.Errorf("a second outage must alert again, got %d alerts total", got)
	}
}

// TestFR_OBS_005_UnregisteredNASSharesOneLabel keeps a spoofed source address
// from minting a time series per packet.
//
// Drives real requests from many distinct addresses, which is the case that
// matters: an earlier version of this test passed nil requests, so a defect
// that labelled by r.RemoteAddr never fired and the test passed against it.
func TestFR_OBS_005_UnregisteredNASSharesOneLabel(t *testing.T) {
	d := &RadiusDaemon{} // no resolver wired, so nothing is identifiable
	before := testCounterVecCount(t, radiusAuthOutcome)

	for i := 0; i < 50; i++ {
		req := &radius.Request{RemoteAddr: &net.UDPAddr{
			IP:   net.IPv4(203, 0, 113, byte(i)), //nolint:gosec // bounded loop
			Port: 40000 + i,
		}}
		d.recordAuthOutcome(req, i%2 == 0)
	}

	after := testCounterVecCount(t, radiusAuthOutcome)
	// At most two new series — accept and reject — under the single shared
	// label, however many distinct addresses turned up.
	if after-before > 2 {
		t.Errorf("50 unidentified source addresses created %d new series; they must collapse "+
			"into the %q bucket, or anyone spoofing a source address can exhaust memory "+
			"with cardinality", after-before, unregisteredNAS)
	}
}

// testCounterVecCount reports how many label combinations a vec currently has.
func testCounterVecCount(t *testing.T, vec *prometheus.CounterVec) int {
	t.Helper()
	ch := make(chan prometheus.Metric, 256)
	go func() {
		vec.Collect(ch)
		close(ch)
	}()
	n := 0
	for range ch {
		n++
	}
	return n
}

// TestFR_OBS_005_PromQLRuleMatchesTheInProcessThreshold — the two must agree,
// or a deployment gets a different answer depending on which one is running.
func TestFR_OBS_005_PromQLRuleMatchesTheInProcessThreshold(t *testing.T) {
	rules, err := readAlertRules()
	if err != nil {
		t.Fatalf("read alert rules: %v", err)
	}
	if !strings.Contains(rules, "radius_auth_outcome_total") {
		t.Error("the rule must read the per-NAS metric this package exports")
	}
	if !strings.Contains(rules, "> 0.20") {
		t.Errorf("the PromQL threshold must match AuthFailureThreshold (%v)", AuthFailureThreshold)
	}
	if !strings.Contains(rules, "[5m]") {
		t.Error("the PromQL window must match the 5-minute window the requirement names")
	}
	if !strings.Contains(rules, "by (nas)") {
		t.Error("the rule must group by NAS — 'on any NAS' is the part a global rate cannot express")
	}
}

// readAlertRules loads the shipped PromQL rules from the repository root.
func readAlertRules() (string, error) {
	b, err := os.ReadFile(filepath.Join("..", "..", "deploy", "prometheus", "radius_alerts.yml"))
	return string(b), err
}
