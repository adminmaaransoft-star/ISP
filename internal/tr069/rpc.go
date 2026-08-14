package tr069

import (
	"fmt"
	"html"
	"strings"
)

// RPC request builders — the ACS → CPE direction | FR-CPE-001..003 | MDS §4.19.
//
// These are built as strings rather than marshalled from structs. CWMP is
// picky about namespace prefixes and attribute ordering in a way Go's
// encoder does not let you control precisely, and several fielded devices
// reject envelopes that are technically valid but shaped differently from
// what they expect. Emitting the exact bytes is the pragmatic choice.

// RPC type names, matching cpe_tasks.rpc_type.
const (
	RPCSetParameterValues = "SetParameterValues"
	RPCGetParameterValues = "GetParameterValues"
	RPCReboot             = "Reboot"
	RPCDownload           = "Download"
	RPCFactoryReset       = "FactoryReset"
)

// envelope wraps a body in the SOAP/CWMP scaffolding every message shares.
//
// All five prefixes are declared, not just the two the element names use.
// The RPC bodies below type their values with xsi:type="xsd:string" and size
// their arrays with soap-enc:arrayType, and an XML prefix that is used but
// never bound is a *fatal* error to a namespace-aware parser — which is what
// the expat and libxml2 stacks inside real CPE are. Go's decoder is lenient
// about this and accepted the malformed envelope happily, so the whole test
// suite passed while every real router would have rejected the message:
// verified against a namespace-aware parser, which reported "unbound prefix"
// until these declarations were added.
func envelope(sessionID, body string) string {
	return `<?xml version="1.0" encoding="UTF-8"?>` +
		`<soap:Envelope xmlns:soap="` + NSSOAPEnv + `"` +
		` xmlns:soap-enc="` + NSSOAPEnc + `"` +
		` xmlns:xsd="` + NSXSD + `"` +
		` xmlns:xsi="` + NSXSI + `"` +
		` xmlns:cwmp="` + NSCWMP10 + `">` +
		`<soap:Header><cwmp:ID soap:mustUnderstand="1">` + esc(sessionID) + `</cwmp:ID></soap:Header>` +
		`<soap:Body>` + body + `</soap:Body>` +
		`</soap:Envelope>`
}

// esc escapes text for XML content.
//
// Device-supplied and operator-supplied strings both end up in these
// envelopes — an SSID, a PPPoE username, a firmware URL — so anything
// interpolated is escaped. An unescaped "&" in a Wi-Fi password would
// otherwise produce an envelope the device rejects as malformed, and a
// crafted one could inject elements.
func esc(s string) string { return html.EscapeString(s) }

// BuildInformResponse acknowledges an Inform, which is always the ACS's
// first message in a session.
func BuildInformResponse(sessionID string) string {
	return envelope(sessionID,
		`<cwmp:InformResponse><MaxEnvelopes>1</MaxEnvelopes></cwmp:InformResponse>`)
}

// BuildSetParameterValues pushes configuration.
//
// Parameters are emitted in a stable (sorted) order so the same logical
// change produces byte-identical envelopes — which makes packet captures
// diffable when a device misbehaves.
func BuildSetParameterValues(sessionID string, params map[string]string, commandKey string) string {
	keys := sortedKeys(params)

	var b strings.Builder
	fmt.Fprintf(&b, `<cwmp:SetParameterValues><ParameterList soap-enc:arrayType="cwmp:ParameterValueStruct[%d]">`, len(keys))
	for _, k := range keys {
		fmt.Fprintf(&b, `<ParameterValueStruct><Name>%s</Name><Value xsi:type="xsd:string">%s</Value></ParameterValueStruct>`,
			esc(k), esc(params[k]))
	}
	fmt.Fprintf(&b, `</ParameterList><ParameterKey>%s</ParameterKey></cwmp:SetParameterValues>`, esc(commandKey))
	return envelope(sessionID, b.String())
}

// BuildGetParameterValues reads configuration or diagnostics back.
func BuildGetParameterValues(sessionID string, names []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, `<cwmp:GetParameterValues><ParameterNames soap-enc:arrayType="xsd:string[%d]">`, len(names))
	for _, n := range names {
		fmt.Fprintf(&b, `<string>%s</string>`, esc(n))
	}
	b.WriteString(`</ParameterNames></cwmp:GetParameterValues>`)
	return envelope(sessionID, b.String())
}

// BuildReboot instructs a device to restart.
//
// The CommandKey is echoed back in the device's next Inform, which is the
// only way to correlate "it came back" with "we told it to".
func BuildReboot(sessionID, commandKey string) string {
	return envelope(sessionID,
		`<cwmp:Reboot><CommandKey>`+esc(commandKey)+`</CommandKey></cwmp:Reboot>`)
}

// BuildFactoryReset wipes a device back to defaults.
func BuildFactoryReset(sessionID string) string {
	return envelope(sessionID, `<cwmp:FactoryReset></cwmp:FactoryReset>`)
}

// BuildDownload instructs a firmware upgrade.
//
// FileType "1 Firmware Upgrade Image" is the only type this ACS issues:
// config-file and vendor types have device-specific semantics that vary too
// much to support blind, and issuing one wrongly can brick a router.
func BuildDownload(sessionID, url, commandKey string, fileSize int) string {
	return envelope(sessionID, fmt.Sprintf(
		`<cwmp:Download>`+
			`<CommandKey>%s</CommandKey>`+
			`<FileType>1 Firmware Upgrade Image</FileType>`+
			`<URL>%s</URL>`+
			`<Username></Username><Password></Password>`+
			`<FileSize>%d</FileSize>`+
			`<TargetFileName></TargetFileName>`+
			`<DelaySeconds>0</DelaySeconds>`+
			`<SuccessURL></SuccessURL><FailureURL></FailureURL>`+
			`</cwmp:Download>`,
		esc(commandKey), esc(url), fileSize))
}

// BuildEmptyResponse is the zero-length body that ends a session.
//
// TR-069 says an ACS with nothing more to send returns 204 with no body; the
// CPE then closes. This is the normal, expected end of every session.
func BuildEmptyResponse() string { return "" }

// sortedKeys returns map keys in a stable order without pulling in sort for
// one call site — the maps here hold a handful of parameters at most.
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}
