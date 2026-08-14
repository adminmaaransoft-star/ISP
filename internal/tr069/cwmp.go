// Package tr069 implements a minimal TR-069 (CWMP) Auto-Configuration
// Server: enough of the protocol to register, provision and remotely manage
// subscriber CPE, and deliberately no more.
//
// Scope note (decision 2026-08-14): this is not a general-purpose ACS. It
// speaks Inform, GetParameterValues, SetParameterValues, Reboot, Download and
// FactoryReset, which is what FR-CPE-001..003 need. Anything relying on the
// full TR-069 data model, or on vendor RPCs outside that set, is out of scope
// and would be better served by integrating GenieACS.
//
// FR: FR-CPE-001..003 | MDS §4.19 | DBD §6.2
package tr069

import (
	"encoding/xml"
	"fmt"
	"strings"
)

// CWMP namespaces. Devices in the field use several revisions and a server
// that accepted only one would reject a large slice of real hardware, so the
// decoder is namespace-tolerant (see StripNamespaces) while the encoder
// answers in cwmp-1-0, which every version understands.
const (
	NSSOAPEnv = "http://schemas.xmlsoap.org/soap/envelope/"
	NSSOAPEnc = "http://schemas.xmlsoap.org/soap/encoding/"
	NSXSD     = "http://www.w3.org/2001/XMLSchema"
	NSXSI     = "http://www.w3.org/2001/XMLSchema-instance"
	NSCWMP10  = "urn:dslforum-org:cwmp-1-0"
)

// Inform event codes (TR-069 Annex A.3.2.1). The three this ACS acts on:
//
//	0 BOOTSTRAP — first contact ever, or after a factory reset: provision.
//	1 BOOT      — the device rebooted: re-check provisioning, drain the queue.
//	2 PERIODIC  — the scheduled check-in: the workhorse, and the only one
//	              that reliably arrives when the device is behind CGNAT and
//	              a Connection Request cannot reach it.
const (
	EventBootstrap = "0 BOOTSTRAP"
	EventBoot      = "1 BOOT"
	EventPeriodic  = "2 PERIODIC"
	EventConnReq   = "6 CONNECTION REQUEST"
	EventTransfer  = "7 TRANSFER COMPLETE"
)

// Envelope is a CWMP SOAP envelope.
type Envelope struct {
	XMLName xml.Name `xml:"Envelope"`
	Header  Header   `xml:"Header"`
	Body    Body     `xml:"Body"`
}

// Header carries the CWMP session ID.
type Header struct {
	ID        string `xml:"ID,omitempty"`
	NoMoreReq int    `xml:"NoMoreRequests,omitempty"`
}

// Body holds whichever RPC this envelope carries. Every field is a pointer
// so "absent" and "present but empty" stay distinguishable — an empty POST
// body is how a CPE says "I have nothing more to send", which is a
// meaningful signal rather than a malformed request.
type Body struct {
	Inform                 *Inform                 `xml:"Inform"`
	InformResponse         *InformResponse         `xml:"InformResponse"`
	GetParameterValuesResp *GetParameterValuesResp `xml:"GetParameterValuesResponse"`
	SetParameterValuesResp *SetParameterValuesResp `xml:"SetParameterValuesResponse"`
	RebootResponse         *struct{}               `xml:"RebootResponse"`
	DownloadResponse       *DownloadResponse       `xml:"DownloadResponse"`
	FactoryResetResponse   *struct{}               `xml:"FactoryResetResponse"`
	TransferComplete       *TransferComplete       `xml:"TransferComplete"`
	Fault                  *SOAPFault              `xml:"Fault"`
}

// DeviceID is TR-069's device identity triple (plus Manufacturer).
type DeviceID struct {
	Manufacturer string `xml:"Manufacturer"`
	OUI          string `xml:"OUI"`
	ProductClass string `xml:"ProductClass"`
	SerialNumber string `xml:"SerialNumber"`
}

// EventStruct is one entry in an Inform's event list.
type EventStruct struct {
	EventCode  string `xml:"EventCode"`
	CommandKey string `xml:"CommandKey"`
}

// ParameterValue is a TR-069 parameter path and its value.
type ParameterValue struct {
	Name  string `xml:"Name"`
	Value string `xml:"Value"`
}

// Inform is the message that opens every CWMP session.
type Inform struct {
	DeviceID      DeviceID         `xml:"DeviceId"`
	Event         []EventStruct    `xml:"Event>EventStruct"`
	MaxEnvelopes  int              `xml:"MaxEnvelopes"`
	CurrentTime   string           `xml:"CurrentTime"`
	RetryCount    int              `xml:"RetryCount"`
	ParameterList []ParameterValue `xml:"ParameterList>ParameterValueStruct"`
}

// HasEvent reports whether the Inform carries a given event code.
func (i *Inform) HasEvent(code string) bool {
	for _, e := range i.Event {
		if e.EventCode == code {
			return true
		}
	}
	return false
}

// EventCodes returns the codes as a comma-joined string for logging and for
// cpe_devices.last_inform_event.
func (i *Inform) EventCodes() string {
	codes := make([]string, 0, len(i.Event))
	for _, e := range i.Event {
		codes = append(codes, e.EventCode)
	}
	return strings.Join(codes, ",")
}

// Param returns a parameter value from the Inform's list.
//
// Matches on suffix as well as exact path: the same logical parameter sits
// at InternetGatewayDevice.* on TR-098 devices and Device.* on TR-181 ones,
// and requiring an exact path would mean maintaining two lookups for every
// field we read.
func (i *Inform) Param(suffix string) string {
	for _, p := range i.ParameterList {
		if p.Name == suffix || strings.HasSuffix(p.Name, suffix) {
			return p.Value
		}
	}
	return ""
}

// InformResponse acknowledges an Inform.
type InformResponse struct {
	MaxEnvelopes int `xml:"MaxEnvelopes"`
}

// GetParameterValuesResp carries the values a device returned.
type GetParameterValuesResp struct {
	ParameterList []ParameterValue `xml:"ParameterList>ParameterValueStruct"`
}

// SetParameterValuesResp reports whether a set applied.
//
// Status 0 means applied immediately, 1 means it will apply after a reboot.
// Both are success — treating 1 as a failure would fail every device that
// defers a WAN change until restart, which many do.
type SetParameterValuesResp struct {
	Status int `xml:"Status"`
}

// DownloadResponse acknowledges a firmware download instruction.
type DownloadResponse struct {
	Status       int    `xml:"Status"`
	StartTime    string `xml:"StartTime"`
	CompleteTime string `xml:"CompleteTime"`
}

// TransferComplete is the device reporting the outcome of a Download.
type TransferComplete struct {
	CommandKey   string     `xml:"CommandKey"`
	FaultStruct  *CWMPFault `xml:"FaultStruct"`
	StartTime    string     `xml:"StartTime"`
	CompleteTime string     `xml:"CompleteTime"`
}

// CWMPFault is the CWMP-level fault detail inside a SOAP fault.
type CWMPFault struct {
	FaultCode   string `xml:"FaultCode"`
	FaultString string `xml:"FaultString"`
}

// SOAPFault is a SOAP-level fault carrying a CWMP fault in its detail.
type SOAPFault struct {
	FaultCode   string    `xml:"faultcode"`
	FaultString string    `xml:"faultstring"`
	Detail      FaultBody `xml:"detail"`
}

// FaultBody is the detail element of a SOAP fault.
type FaultBody struct {
	Fault CWMPFault `xml:"Fault"`
}

// ParseEnvelope decodes a CWMP request body.
//
// An empty body is not an error: a CPE POSTing nothing is how it says "I
// have no more requests", which is the signal that ends a session. Callers
// distinguish the two by the returned envelope being nil.
func ParseEnvelope(raw []byte) (*Envelope, error) {
	if len(strings.TrimSpace(string(raw))) == 0 {
		return nil, nil
	}

	var env Envelope
	// Namespace prefixes vary by device and by CWMP revision. Go's decoder
	// matches on local names once the prefixes are normalised, which is what
	// lets one decoder accept cwmp-1-0 through cwmp-1-4 hardware.
	if err := xml.Unmarshal(StripNamespaces(raw), &env); err != nil {
		return nil, fmt.Errorf("tr069: parse CWMP envelope: %w", err)
	}
	return &env, nil
}

// StripNamespaces rewrites namespaced element names to their local part.
//
// Devices in the field use cwmp-1-0 through cwmp-1-4 and a variety of prefix
// spellings ("cwmp:", "soap:", "SOAP-ENV:"). Rather than enumerate every
// combination in struct tags — and reject the one vendor that picked a
// different prefix — the decoder normalises first.
func StripNamespaces(raw []byte) []byte {
	dec := xml.NewDecoder(strings.NewReader(string(raw)))
	var out strings.Builder
	enc := xml.NewEncoder(&out)

	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			t.Name.Space = ""
			// Attributes carry namespace declarations that would otherwise
			// be re-emitted and re-bind the prefixes we just stripped.
			t.Attr = nil
			_ = enc.EncodeToken(t) //nolint:errcheck // building an in-memory buffer
		case xml.EndElement:
			t.Name.Space = ""
			_ = enc.EncodeToken(t) //nolint:errcheck
		default:
			_ = enc.EncodeToken(tok) //nolint:errcheck
		}
	}
	_ = enc.Flush() //nolint:errcheck
	return []byte(out.String())
}
