package svc

import (
	"fmt"
	"slices"
)

// Verdict is the answer to the engineer for one spec in one environment.
type Verdict string

const (
	Accept  Verdict = "ACCEPT"  // everything renders
	Partial Verdict = "PARTIAL" // refused items are left out; the rest renders
	Refuse  Verdict = "REFUSE"  // a spec-level error: nothing renders
)

// Result is the outcome for one spec in one environment.
type Result struct {
	Service  string
	Env      string
	Verdict  Verdict
	Findings []Finding
	Objects  []Object
	Notes    []string
}

// Errors counts the findings that refuse something.
func (r Result) Errors() int {
	n := 0
	for _, f := range r.Findings {
		if f.Severity == Error {
			n++
		}
	}
	return n
}

// Evaluate checks a spec for one environment and renders what may be applied.
// A spec-level error refuses everything, because a service in the wrong
// namespace or with the wrong environments is wrong as a whole. An error in
// one list item refuses only that item; the platform still never applies a
// partial spec on its own, the engineer sees what would render once they
// delete or fix the refused items.
func Evaluate(s *Spec, p *Platform, env string) Result {
	r := Result{Service: s.Service, Env: env}
	r.Findings = Check(s, p)
	if slices.Contains(s.Environments, env) {
		r.Findings = append(r.Findings, CheckEnv(s, p, env)...)
	} else {
		for _, f := range checkEnvRemoval(s, p, env) {
			f.Envs = []string{env}
			r.Findings = append(r.Findings, f)
		}
	}
	sortFindings(r.Findings)

	refused := map[string]bool{}
	for _, f := range r.Findings {
		if f.Severity != Error {
			continue
		}
		item := f.Item()
		if item == "" {
			r.Verdict = Refuse
			return r
		}
		refused[item] = true
	}
	r.Verdict = Accept
	if len(refused) > 0 {
		r.Verdict = Partial
	}
	if !slices.Contains(s.Environments, env) {
		r.Notes = append(r.Notes, fmt.Sprintf("%s is not listed in environments: nothing is rendered for it", env))
		return r
	}
	r.Objects, r.Notes = Render(s, p, env, refused)
	return r
}

// CheckFleet looks across specs for network rules with no matching rule on
// the other side. Policies are enforced at both ends, so an ingress rule with
// no caller declaring the egress (or the reverse) admits nothing.
func CheckFleet(specs []*Spec, p *Platform) map[string][]Finding {
	ns := func(s *Spec) string { return team(s, p) } // team namespace == team name in every env
	out := map[string][]Finding{}
	inNamespace := map[string][]*Spec{}
	for _, s := range specs {
		inNamespace[ns(s)] = append(inNamespace[ns(s)], s)
	}
	declares := func(peers []Peer, namespace string, port int) bool {
		return slices.ContainsFunc(peers, func(r Peer) bool { return r.Namespace == namespace && slices.Contains(r.Ports, port) })
	}
	for _, s := range specs {
		for i, r := range s.Ingress {
			callers, isTeam := inNamespace[r.Namespace]
			if !isTeam || r.Namespace == ns(s) {
				continue // a platform namespace such as monitoring, or our own
			}
			for _, port := range r.Ports {
				if !slices.ContainsFunc(callers, func(c *Spec) bool { return declares(c.Egress, ns(s), port) }) {
					out[s.Service] = append(out[s.Service], warnf("NET", fmt.Sprintf("ingress[%d]", i),
						fmt.Sprintf("the calling service in %s adds to its spec: egress: - namespace: %s, ports: [%d]", r.Namespace, ns(s), port),
						"no service in %s declares egress to %s on port %d, so its calls are still dropped by its own egress policy", r.Namespace, ns(s), port))
				}
			}
		}
		for i, r := range s.Egress {
			targets, isTeam := inNamespace[r.Namespace]
			if !isTeam || r.Namespace == ns(s) {
				continue
			}
			for _, port := range r.Ports {
				if !slices.ContainsFunc(targets, func(t *Spec) bool { return declares(t.Ingress, ns(s), port) }) {
					out[s.Service] = append(out[s.Service], warnf("NET", fmt.Sprintf("egress[%d]", i),
						fmt.Sprintf("the service you call in %s adds to its spec: ingress: - namespace: %s, ports: [%d]", r.Namespace, ns(s), port),
						"no service in %s admits ingress from %s on port %d", r.Namespace, ns(s), port))
				}
			}
		}
	}
	return out
}
