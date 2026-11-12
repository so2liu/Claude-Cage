package workbench

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/sylvinus/agent-vm/internal/cage"
	"github.com/sylvinus/agent-vm/internal/guestos"
)

func TestLinuxDNSRequiresTCPObservations(t *testing.T) {
	var observation guestObservation
	if err := json.Unmarshal([]byte(`{"dns_udp_attempted":true,"resolvers":{"count":1,"confined":true}}`), &observation); err != nil {
		t.Fatal(err)
	}
	issue, confirmed := dnsBypassIssue(observation, true)
	if issue == "" {
		t.Fatalf("missing TCP/53 and DoT results accepted: issue=%q confirmed=%t", issue, confirmed)
	}
}

func TestLinuxBoundaryRequiresEveryObservation(t *testing.T) {
	fields := map[string]any{
		"link_local_connected": false, "proxy_endpoint_connected": false,
		"dns_udp_attempted": true, "dns_tcp_connected": false, "dot_connected": false,
		"resolvers": map[string]any{"count": 1, "confined": true}, "ipv6_global_addresses": 0,
	}
	before := cage.GateStatus{}
	after := cage.GateStatus{PolicyVerified: true}
	after.Counters.IPv6Denied = 1
	s := &Server{}
	for _, field := range []string{"link_local_connected", "proxy_endpoint_connected", "dns_tcp_connected", "dot_connected", "resolvers", "ipv6_global_addresses"} {
		for _, null := range []bool{false, true} {
			t.Run(field+"/null="+fmt.Sprint(null), func(t *testing.T) {
				input := make(map[string]any, len(fields))
				for key, value := range fields {
					input[key] = value
				}
				if null {
					input[field] = nil
				} else {
					delete(input, field)
				}
				data, _ := json.Marshal(input)
				var v guestObservation
				if err := json.Unmarshal(data, &v); err != nil {
					t.Fatal(err)
				}
				switch field {
				case "link_local_connected", "proxy_endpoint_connected":
					if issue, confirmed := hostReachIssue(v, true, true); issue == "" || confirmed {
						t.Fatalf("missing host evidence accepted: %q %t", issue, confirmed)
					}
				case "ipv6_global_addresses":
					if s.ipv6PolicyVerified(v, before, after, true) {
						t.Fatal("missing IPv6 evidence accepted despite denial counter")
					}
				default:
					if issue, confirmed := dnsBypassIssue(v, true); issue == "" || confirmed {
						t.Fatalf("missing DNS evidence accepted: %q %t", issue, confirmed)
					}
				}
			})
		}
	}
	for _, resolver := range []string{`{"count":1}`, `{"confined":true}`} {
		var v guestObservation
		data := `{"dns_udp_attempted":true,"dns_tcp_connected":false,"dot_connected":false,"resolvers":` + resolver + `}`
		if err := json.Unmarshal([]byte(data), &v); err != nil {
			t.Fatal(err)
		}
		if issue, confirmed := dnsBypassIssue(v, true); issue == "" || confirmed {
			t.Fatalf("partial resolver evidence accepted: %s", data)
		}
	}
	if issue, _ := hostReachIssue(guestObservation{LinkLocalConnected: boolp(false)}, true, false); issue != "" {
		t.Fatalf("unconfigured proxy demanded: %s", issue)
	}
}

func TestIPv6DenialCannotReplaceLinuxAddressEvidence(t *testing.T) {
	s := &Server{}
	before := cage.GateStatus{}
	after := cage.GateStatus{PolicyVerified: true}
	after.Counters.IPv6Denied = 1
	for _, addresses := range []*int{nil, intp(-1), intp(1), intp(0)} {
		v := guestObservation{IPv6GlobalAddresses: addresses, IPv6DefaultRoutes: 1}
		want := addresses != nil && *addresses == 0
		if got := s.ipv6PolicyVerified(v, before, after, true); got != want {
			t.Fatalf("addresses %v: got %t want %t", addresses, got, want)
		}
	}
	v := guestObservation{IPv6GlobalAddresses: intp(0)}
	if !s.ipv6PolicyVerified(v, before, cage.GateStatus{PolicyVerified: true}, true) {
		t.Fatal("zero addresses and routes rejected")
	}
	v.IPv6Connected = true
	if s.ipv6PolicyVerified(v, before, after, true) {
		t.Fatal("live IPv6 connection accepted")
	}
}

func TestNativeGuestsDoNotRequireLinuxEvidence(t *testing.T) {
	cfg := cage.DefaultConfig()
	v := guestObservation{TZ: cfg.Timezone, SystemTimezone: cfg.Timezone, LocaleValid: true,
		ProcessLocale: "en-US", SystemLocale: "en-US", SystemLanguage: "en-US"}
	for _, s := range []*Server{{windowsGuest: &guestos.WindowsProfile{}}, {macosGuest: &guestos.MacOSProfile{}}} {
		if !s.guestSettingsOK(v, cfg) {
			t.Fatal("native settings require Linux time observations")
		}
		if issue, _ := dnsBypassIssue(v, false); issue != "" {
			t.Fatal(issue)
		}
		if issue, _ := hostReachIssue(v, false, true); issue != "" {
			t.Fatal(issue)
		}
		after := cage.GateStatus{PolicyVerified: true}
		if s.macosGuest != nil {
			v.IPv6UnscopedAbsent = true
			v.IPv6Unbound.Result = "no-route"
		}
		if !s.ipv6PolicyVerified(v, cage.GateStatus{}, after, true) {
			t.Fatal("native IPv6 check requires Linux address observation")
		}
	}
}
