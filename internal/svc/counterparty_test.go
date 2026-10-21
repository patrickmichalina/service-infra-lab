package svc

import (
	"strings"
	"testing"
)

// router is the base spec as a normalization service, the team the catalog clears
// for venue-a-fix; extra egress rules go in place of the vendor feed.
func router(egress string) string {
	return strings.NewReplacer(
		"odds-ingest", "rfq-router", "team: ingestion", "team: normalization", "/ingestion/", "/normalization/",
		"odds-snapshots", "rfq-audit",
		"  - {cidr: 0.0.0.0/0, ports: [443], hosts: [feed.vendor-example.com, aws:s3], reason: vendor feed}\n",
		"  - {cidr: 0.0.0.0/0, ports: [443], hosts: [aws:s3], reason: S3 through its endpoint}\n"+egress,
	).Replace(base)
}

// A counterparty renders as the partner's endpoints in each environment, exactly,
// on the ports the spec asked for, and no address is in the spec.
func TestCounterpartyRendersEachEnvsEndpoints(t *testing.T) {
	p := platform(t)
	s := spec(t, router("  - {counterparty: venue-a-fix, ports: [9880], reason: FIX session to venue A}\n"))
	for env, cidr := range map[string]string{"stg": "198.51.100.16/28", "prd": "192.0.2.16/28"} {
		r := Evaluate(s, p, env)
		if r.Verdict != Accept {
			t.Fatalf("%s: %s %v", env, r.Verdict, r.Findings)
		}
		np := object(t, r, "networkpolicy.yaml")
		if !strings.Contains(np, "cidr: "+cidr+"\n") || strings.Contains(np, "except: []") {
			t.Errorf("%s: want an exact ipBlock %s:\n%s", env, cidr, np)
		}
		if !strings.Contains(np, "port: 9880") {
			t.Errorf("%s: want port 9880:\n%s", env, np)
		}
	}
}

func TestCounterpartyRefusals(t *testing.T) {
	p := platform(t)
	// A team the catalog does not clear: a partner session is a contract, not self-service.
	expectError(t, p, with("  - {cidr: \"{vpcCidr}\", ports: [9098]}\n",
		"  - {cidr: \"{vpcCidr}\", ports: [9098]}\n  - {counterparty: venue-a-fix, ports: [9880], reason: FIX}\n"),
		"R4", "egress[1].counterparty", "clear team ingestion for venue-a-fix")
	expectError(t, p, router("  - {counterparty: venue-b-fix, ports: [9880], reason: FIX}\n"),
		"R4", "egress[2].counterparty", "cleared for (venue-a-fix)")
	expectError(t, p, router("  - {counterparty: venue-a-fix, ports: [443], reason: FIX}\n"),
		"R4", "egress[2].ports[0]", "ports: [9880]")
	expectError(t, p, router("  - {counterparty: venue-a-fix, reason: FIX}\n"),
		"R4", "egress[2].ports", "list the ports")
	expectError(t, p, router("  - {counterparty: venue-a-fix, cidr: 0.0.0.0/0, ports: [9880], reason: FIX}\n"),
		"R4", "egress[2]", "one rule per destination")
	expectError(t, p, router("  - {counterparty: venue-a-fix, ports: [9880], hosts: [fix.venue-a.example], reason: FIX}\n"),
		"R4", "egress[2].hosts", "remove it")
	expectError(t, p, strings.Replace(router(""), "  - {namespace: monitoring, ports: [8080]}\n",
		"  - {namespace: monitoring, ports: [8080]}\n  - {counterparty: venue-a-fix, ports: [9880]}\n", 1),
		"R4", "ingress[1].counterparty", "inbound partner access")
}

// The partner's address written literally is refused (R4/R5), and the fix names the
// catalog entry it belongs to.
func TestLiteralCounterpartyAddressSuggestsTheEntry(t *testing.T) {
	p := platform(t)
	expectError(t, p, router("  - {cidr: 198.51.100.20/32, ports: [9880], reason: venue A}\n"),
		"R4", "egress[2].cidr", "counterparty: venue-a-fix, ports: [9880]")
}

// A refused counterparty rule is one refused item: the rest still renders, without it.
func TestRefusedCounterpartyIsPartial(t *testing.T) {
	p := platform(t)
	r := Evaluate(spec(t, router("  - {counterparty: venue-a-fix, ports: [443], reason: FIX}\n")), p, "stg")
	if r.Verdict != Partial {
		t.Fatalf("want PARTIAL, got %s %v", r.Verdict, r.Findings)
	}
	if np := object(t, r, "networkpolicy.yaml"); strings.Contains(np, "198.51.100.16/28") {
		t.Errorf("the refused rule was rendered:\n%s", np)
	}
}
