package tr069_test

import (
	"strings"
	"testing"

	"github.com/maaransoft/isp-bss-oss/internal/tr069"
)

// CWMP codec tests — FR-CPE-001..003 | MDS §4.19.
//
// The decoder reads XML from unauthenticated field hardware, so its
// tolerance and its failure modes both matter. Devices in the wild use
// cwmp-1-0 through cwmp-1-4 and a variety of namespace prefixes; a decoder
// that accepted only one spelling would reject a large slice of real CPE.

// A realistic Inform as a TR-098 device sends it, with the "cwmp:" and
// "SOAP-ENV:" prefixes most vendors use.
const informTR098 = `<?xml version="1.0" encoding="UTF-8"?>
<SOAP-ENV:Envelope xmlns:SOAP-ENV="http://schemas.xmlsoap.org/soap/envelope/"
                   xmlns:cwmp="urn:dslforum-org:cwmp-1-0">
  <SOAP-ENV:Header><cwmp:ID SOAP-ENV:mustUnderstand="1">sess-42</cwmp:ID></SOAP-ENV:Header>
  <SOAP-ENV:Body>
    <cwmp:Inform>
      <DeviceId>
        <Manufacturer>TP-Link</Manufacturer>
        <OUI>001122</OUI>
        <ProductClass>ArcherC6</ProductClass>
        <SerialNumber>SN-ACS-001</SerialNumber>
      </DeviceId>
      <Event SOAP-ENV:arrayType="cwmp:EventStruct[1]">
        <EventStruct><EventCode>1 BOOT</EventCode><CommandKey></CommandKey></EventStruct>
      </Event>
      <MaxEnvelopes>1</MaxEnvelopes>
      <CurrentTime>2026-08-14T10:00:00Z</CurrentTime>
      <RetryCount>0</RetryCount>
      <ParameterList SOAP-ENV:arrayType="cwmp:ParameterValueStruct[3]">
        <ParameterValueStruct>
          <Name>InternetGatewayDevice.DeviceInfo.SoftwareVersion</Name><Value>1.4.2</Value>
        </ParameterValueStruct>
        <ParameterValueStruct>
          <Name>InternetGatewayDevice.DeviceInfo.HardwareVersion</Name><Value>v3</Value>
        </ParameterValueStruct>
        <ParameterValueStruct>
          <Name>InternetGatewayDevice.ManagementServer.ConnectionRequestURL</Name>
          <Value>http://192.168.1.1:7547/cwmp</Value>
        </ParameterValueStruct>
      </ParameterList>
    </cwmp:Inform>
  </SOAP-ENV:Body>
</SOAP-ENV:Envelope>`

func TestParseEnvelope_Inform(t *testing.T) {
	env, err := tr069.ParseEnvelope([]byte(informTR098))
	if err != nil {
		t.Fatalf("ParseEnvelope: %v", err)
	}
	if env == nil || env.Body.Inform == nil {
		t.Fatal("want an Inform")
	}

	inform := env.Body.Inform
	if inform.DeviceID.SerialNumber != "SN-ACS-001" {
		t.Errorf("serial = %q", inform.DeviceID.SerialNumber)
	}
	if inform.DeviceID.OUI != "001122" {
		t.Errorf("OUI = %q", inform.DeviceID.OUI)
	}
	if env.Header.ID != "sess-42" {
		t.Errorf("session ID = %q, want sess-42", env.Header.ID)
	}
	if !inform.HasEvent(tr069.EventBoot) {
		t.Errorf("want the 1 BOOT event, got %v", inform.Event)
	}
	if inform.HasEvent(tr069.EventBootstrap) {
		t.Error("must not report an event the device did not send")
	}
}

// TestInform_ParamMatchesAcrossDataModels: the same logical parameter sits at
// InternetGatewayDevice.* on TR-098 and Device.* on TR-181. Requiring an
// exact path would mean maintaining two lookups for every field.
func TestInform_ParamMatchesAcrossDataModels(t *testing.T) {
	env, err := tr069.ParseEnvelope([]byte(informTR098))
	if err != nil {
		t.Fatalf("ParseEnvelope: %v", err)
	}
	inform := env.Body.Inform

	if got := inform.Param(".SoftwareVersion"); got != "1.4.2" {
		t.Errorf("SoftwareVersion = %q, want 1.4.2", got)
	}
	if got := inform.Param(".ConnectionRequestURL"); got != "http://192.168.1.1:7547/cwmp" {
		t.Errorf("ConnectionRequestURL = %q", got)
	}
	if got := inform.Param(".NoSuchParameter"); got != "" {
		t.Errorf("an absent parameter must return empty, got %q", got)
	}
}

// TestParseEnvelope_ToleratesNamespaceVariants is the interop property. Same
// message, three prefix spellings and two CWMP revisions — all must decode.
func TestParseEnvelope_ToleratesNamespaceVariants(t *testing.T) {
	variants := map[string]string{
		"soap prefix, cwmp-1-2": strings.NewReplacer(
			"SOAP-ENV", "soap", "cwmp-1-0", "cwmp-1-2").Replace(informTR098),
		"s prefix, cwmp-1-4": strings.NewReplacer(
			"SOAP-ENV", "s", "cwmp-1-0", "cwmp-1-4").Replace(informTR098),
	}
	for name, raw := range variants {
		t.Run(name, func(t *testing.T) {
			env, err := tr069.ParseEnvelope([]byte(raw))
			if err != nil {
				t.Fatalf("ParseEnvelope: %v", err)
			}
			if env == nil || env.Body.Inform == nil {
				t.Fatal("want an Inform regardless of prefix or CWMP revision")
			}
			if env.Body.Inform.DeviceID.SerialNumber != "SN-ACS-001" {
				t.Errorf("serial = %q", env.Body.Inform.DeviceID.SerialNumber)
			}
		})
	}
}

// TestParseEnvelope_EmptyBodyIsNotAnError: a CPE POSTing nothing is how it
// says "I have no more requests" — a meaningful protocol signal, not a
// malformed request.
func TestParseEnvelope_EmptyBodyIsNotAnError(t *testing.T) {
	for _, raw := range []string{"", "   ", "\n\t "} {
		env, err := tr069.ParseEnvelope([]byte(raw))
		if err != nil {
			t.Errorf("an empty body must not be an error: %v", err)
		}
		if env != nil {
			t.Error("an empty body must decode to a nil envelope")
		}
	}
}

func TestParseEnvelope_RejectsMalformedXML(t *testing.T) {
	if _, err := tr069.ParseEnvelope([]byte(`<Envelope><unclosed>`)); err == nil {
		t.Error("want an error for malformed XML")
	}
}

func TestParseEnvelope_Fault(t *testing.T) {
	const faultBody = `<?xml version="1.0"?>
<soap:Envelope xmlns:soap="http://schemas.xmlsoap.org/soap/envelope/"
               xmlns:cwmp="urn:dslforum-org:cwmp-1-0">
  <soap:Header><cwmp:ID>s1</cwmp:ID></soap:Header>
  <soap:Body>
    <soap:Fault>
      <faultcode>Client</faultcode>
      <faultstring>CWMP fault</faultstring>
      <detail>
        <cwmp:Fault>
          <FaultCode>9003</FaultCode>
          <FaultString>Invalid arguments</FaultString>
        </cwmp:Fault>
      </detail>
    </soap:Fault>
  </soap:Body>
</soap:Envelope>`

	env, err := tr069.ParseEnvelope([]byte(faultBody))
	if err != nil {
		t.Fatalf("ParseEnvelope: %v", err)
	}
	if env.Body.Fault == nil {
		t.Fatal("want a Fault")
	}
	if env.Body.Fault.Detail.Fault.FaultCode != "9003" {
		t.Errorf("CWMP fault code = %q, want 9003", env.Body.Fault.Detail.Fault.FaultCode)
	}
}

// ── RPC builders ────────────────────────────────────────────────────────────

// TestBuildSetParameterValues_EscapesInterpolatedValues is a real safety
// property: an SSID or Wi-Fi password containing "&" or "<" would otherwise
// produce an envelope the device rejects as malformed, and a crafted one
// could inject elements into the message.
func TestBuildSetParameterValues_EscapesInterpolatedValues(t *testing.T) {
	out := tr069.BuildSetParameterValues("s1", map[string]string{
		"Device.WiFi.SSID": `Bob & Alice's "Net" <home>`,
	}, "task-1")

	if strings.Contains(out, `Bob & Alice`) {
		t.Error("a raw ampersand reached the envelope; the value was not escaped")
	}
	if !strings.Contains(out, "&amp;") {
		t.Errorf("want an escaped ampersand in:\n%s", out)
	}
	if strings.Contains(out, "<home>") {
		t.Error("a raw angle bracket reached the envelope — element injection is possible")
	}
}

// TestBuildSetParameterValues_IsDeterministic: the same logical change must
// produce byte-identical envelopes so packet captures stay diffable when a
// device misbehaves.
func TestBuildSetParameterValues_IsDeterministic(t *testing.T) {
	params := map[string]string{
		"Device.WiFi.SSID":      "MyNet",
		"Device.PPP.Username":   "user@isp",
		"Device.QoS.Downstream": "100000",
	}
	first := tr069.BuildSetParameterValues("s1", params, "k")
	for i := 0; i < 20; i++ {
		if got := tr069.BuildSetParameterValues("s1", params, "k"); got != first {
			t.Fatal("parameter ordering is not stable across calls")
		}
	}
}

func TestBuildRPCs_ShapeAndContent(t *testing.T) {
	t.Run("InformResponse echoes the session id", func(t *testing.T) {
		out := tr069.BuildInformResponse("sess-42")
		if !strings.Contains(out, "InformResponse") || !strings.Contains(out, "sess-42") {
			t.Errorf("unexpected InformResponse:\n%s", out)
		}
	})

	t.Run("Reboot carries the command key for correlation", func(t *testing.T) {
		out := tr069.BuildReboot("s1", "task-77")
		if !strings.Contains(out, "cwmp:Reboot") || !strings.Contains(out, "task-77") {
			t.Errorf("unexpected Reboot:\n%s", out)
		}
	})

	t.Run("Download names the firmware file type", func(t *testing.T) {
		out := tr069.BuildDownload("s1", "https://fw.isp/v2.bin", "task-9", 0)
		if !strings.Contains(out, "1 Firmware Upgrade Image") {
			t.Error("Download must declare the firmware file type")
		}
		if !strings.Contains(out, "https://fw.isp/v2.bin") {
			t.Error("Download must carry the URL")
		}
	})

	t.Run("GetParameterValues lists every requested name", func(t *testing.T) {
		out := tr069.BuildGetParameterValues("s1", []string{"Device.A", "Device.B"})
		if !strings.Contains(out, "Device.A") || !strings.Contains(out, "Device.B") {
			t.Errorf("unexpected GetParameterValues:\n%s", out)
		}
	})
}

// ── Provisioning ────────────────────────────────────────────────────────────

// TestRenderTemplate_DropsUnresolvedValues is the safety property that
// matters most here. Subscriber passwords are bcrypt, so {{pppoe_password}}
// usually has nothing to substitute — and pushing an EMPTY PPPoE password
// would disconnect the subscriber while reporting a successful provisioning.
// Omitting the parameter leaves whatever the device already has.
func TestRenderTemplate_DropsUnresolvedValues(t *testing.T) {
	template := map[string]string{
		"Device.PPP.Username": "{{pppoe_username}}",
		"Device.PPP.Password": "{{pppoe_password}}", // no value available
		"Device.WiFi.SSID":    "{{ssid}}",
	}

	got := tr069.RenderTemplate(template, tr069.ProvisioningContext{
		PPPoEUsername: "ravi@isp",
		SSID:          "ISP-Fibre",
		// PPPoEPassword deliberately unset, as it always is in practice.
	})

	if _, present := got["Device.PPP.Password"]; present {
		t.Error("an unresolvable password must be DROPPED, not pushed empty — that would disconnect the subscriber")
	}
	if got["Device.PPP.Username"] != "ravi@isp" {
		t.Errorf("username = %q, want ravi@isp", got["Device.PPP.Username"])
	}
	if got["Device.WiFi.SSID"] != "ISP-Fibre" {
		t.Errorf("SSID = %q", got["Device.WiFi.SSID"])
	}
}

// TestRenderTemplate_ShapingDerivesFromThePlan is FR-CPE-002: the CPE-side
// shaping must come from the same plan value that drives the NAS-side RADIUS
// limit, or the two ends of the link disagree.
func TestRenderTemplate_ShapingDerivesFromThePlan(t *testing.T) {
	up, down := tr069.RateLimitToKbps("100M/50M")
	if up != "100000" || down != "50000" {
		t.Fatalf("RateLimitToKbps(100M/50M) = (%q, %q), want (100000, 50000)", up, down)
	}

	got := tr069.RenderTemplate(map[string]string{
		"Device.QoS.Up":   "{{upstream_kbps}}",
		"Device.QoS.Down": "{{downstream_kbps}}",
	}, tr069.ProvisioningContext{UpstreamKbps: up, DownstreamKbps: down})

	if got["Device.QoS.Up"] != "100000" || got["Device.QoS.Down"] != "50000" {
		t.Errorf("shaping did not render: %+v", got)
	}
}

func TestRateLimitToKbps(t *testing.T) {
	cases := []struct{ in, up, down string }{
		{"100M/100M", "100000", "100000"},
		{"512k/256k", "512", "256"},
		{"1G/1G", "1000000", "1000000"},
		{"", "", ""},
		{"garbage", "", ""},
		// A malformed rate must yield empty rather than a wrong number: a
		// wrong shaping value silently throttles a subscriber.
		{"100X/50M", "", "50000"},
	}
	for _, tc := range cases {
		up, down := tr069.RateLimitToKbps(tc.in)
		if up != tc.up || down != tc.down {
			t.Errorf("RateLimitToKbps(%q) = (%q,%q), want (%q,%q)", tc.in, up, down, tc.up, tc.down)
		}
	}
}

// TestEveryBuilderDeclaresTheNamespacesItUses is a regression test for a
// defect that every other test in this file missed.
//
// The builders type their values with xsi:type="xsd:string" and size their
// arrays with soap-enc:arrayType, but the envelope originally declared only
// the "soap" and "cwmp" prefixes. Using an unbound prefix is a *fatal* error
// under the XML Namespaces spec, so a namespace-aware parser — expat and
// libxml2, which is what the CWMP stacks inside real routers are built on —
// rejects the whole message with "unbound prefix".
//
// Go's encoding/xml is lenient here: it treats an unknown prefix as if the
// prefix itself were the namespace URI and parses on. That leniency is
// precisely why this went unnoticed — the full suite passed against a decoder
// no CPE actually uses, while every real device would have refused to be
// provisioned. So this test checks the property directly rather than by
// round-tripping through a parser that forgives it.
func TestEveryBuilderDeclaresTheNamespacesItUses(t *testing.T) {
	envelopes := map[string]string{
		"InformResponse":     tr069.BuildInformResponse("sess-1"),
		"SetParameterValues": tr069.BuildSetParameterValues("sess-1", map[string]string{"Device.WiFi.SSID": "Home & Away"}, "task-1"),
		"GetParameterValues": tr069.BuildGetParameterValues("sess-1", []string{"Device.DeviceInfo.SoftwareVersion"}),
		"Reboot":             tr069.BuildReboot("sess-1", "task-2"),
		"FactoryReset":       tr069.BuildFactoryReset("sess-1"),
		"Download":           tr069.BuildDownload("sess-1", "https://fw.example.net/x.bin", "task-3", 0),
	}

	for name, xmlDoc := range envelopes {
		declared := declaredPrefixes(xmlDoc)
		for _, used := range usedPrefixes(xmlDoc) {
			if !declared[used] {
				t.Errorf("%s: uses prefix %q but never binds it — a namespace-aware "+
					"CPE parser rejects this envelope as an unbound prefix\n%s", name, used, xmlDoc)
			}
		}
	}
}

// declaredPrefixes collects the prefixes an envelope binds with xmlns:.
func declaredPrefixes(doc string) map[string]bool {
	declared := map[string]bool{"xml": true, "xmlns": true}
	for _, part := range strings.Split(doc, "xmlns:")[1:] {
		if i := strings.IndexByte(part, '='); i > 0 {
			declared[part[:i]] = true
		}
	}
	return declared
}

// usedPrefixes collects prefixes an envelope references — in element names,
// in attribute names, and in the values of QName-typed attributes such as
// xsi:type="xsd:string" and soap-enc:arrayType="cwmp:ParameterValueStruct[1]",
// whose values name a type and so bind just as strictly as an element does.
//
// Only those two attribute values are inspected. Scanning every value would
// misread the URI in xmlns:cwmp="urn:dslforum-org:cwmp-1-0" as a use of a
// prefix called "urn".
func usedPrefixes(doc string) []string {
	var used []string
	for i := 0; i < len(doc); i++ {
		switch doc[i] {
		case '<':
			j := i + 1
			if j < len(doc) && (doc[j] == '/' || doc[j] == '?') {
				j++
			}
			if p, ok := prefixAt(doc, j); ok {
				used = append(used, p)
			}
		case ' ':
			name, value, next, ok := attributeAt(doc, i+1)
			if !ok {
				continue
			}
			i = next
			if strings.HasPrefix(name, "xmlns") {
				continue
			}
			if p, ok := prefixAt(name, 0); ok {
				used = append(used, p)
			}
			if local := name[strings.IndexByte(name, ':')+1:]; local == "type" || local == "arrayType" {
				if p, ok := prefixAt(value, 0); ok {
					used = append(used, p)
				}
			}
		}
	}
	return used
}

// attributeAt reads a name="value" pair starting at i, returning the index of
// its closing quote.
func attributeAt(doc string, i int) (name, value string, end int, ok bool) {
	j := i
	for j < len(doc) && (isNameByte(doc[j]) || doc[j] == ':') {
		j++
	}
	if j == i || j+1 >= len(doc) || doc[j] != '=' || doc[j+1] != '"' {
		return "", "", 0, false
	}
	closing := strings.IndexByte(doc[j+2:], '"')
	if closing < 0 {
		return "", "", 0, false
	}
	return doc[i:j], doc[j+2 : j+2+closing], j + 2 + closing, true
}

// prefixAt reads a "prefix:" token starting at i, if there is one.
func prefixAt(doc string, i int) (string, bool) {
	j := i
	for j < len(doc) && isNameByte(doc[j]) {
		j++
	}
	if j == i || j >= len(doc) || doc[j] != ':' {
		return "", false
	}
	return doc[i:j], true
}

func isNameByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_'
}
