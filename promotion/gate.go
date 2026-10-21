package promotion

import (
	"fmt"
	"sort"
	"time"
)

// Verdict is the gate's answer for one query.
type Verdict string

const (
	Promotable Verdict = "PROMOTABLE"
	Wait       Verdict = "WAIT"    // on track; PromotableAt says when, if nothing changes
	Blocked    Verdict = "BLOCKED" // waiting alone will not help; Reason says what will
)

// Policy is the soak rule: a revision must have been the live, healthy
// revision in From, without interruption, for at least Soak, ending at now.
type Policy struct {
	From string
	Soak time.Duration
}

// Query asks whether Revision of Service is promotable at Now.
type Query struct {
	Now      time.Time
	Service  string
	Revision string
}

// Decision is the gate's answer, with a reason an engineer can act on.
type Decision struct {
	Query
	Verdict      Verdict
	Reason       string
	PromotableAt time.Time // set for WAIT
}

// Gate answers queries against one ingested event stream.
type Gate struct {
	policy  Policy
	events  []Event
	defects []Defect
}

// NewGate parses-and-ingests nothing itself: give it events from Ingest and
// every defect found on the way, so tainted queries can fail closed.
func NewGate(p Policy, events []Event, defects []Defect) *Gate {
	ds := append([]Defect(nil), defects...)
	sortDefects(ds)
	return &Gate{policy: p, events: events, defects: ds}
}

// Defects returns every problem found in the input, in line order.
func (g *Gate) Defects() []Defect { return g.defects }

// Decide applies the soak rule to q.
func (g *Gate) Decide(q Query) Decision {
	t := BuildTimeline(g.events, g.policy.From, q.Service, q.Now)
	d := g.fromTimeline(t, q)

	// An event we could not read may be the one that interrupted this
	// revision. Fail closed, but say what the answer would otherwise be.
	for _, def := range g.defects {
		if def.Taint == nil || !def.Taint.Concerns(g.policy.From, q.Service, q.Now) {
			continue
		}
		note := fmt.Sprintf("line %d of the event stream is unusable (%s) and may be about this service: %s. Have the producer redeliver it; it then decides", def.Line, def.problem(), def.Taint)
		if d.Verdict == Blocked {
			d.Reason += "; also " + note
			continue
		}
		d.Reason = fmt.Sprintf("%s. Without it the gate would say %s: %s", note, d.Verdict, d.Reason)
		d.Verdict, d.PromotableAt = Blocked, time.Time{}
	}
	return d
}

func (g *Gate) fromTimeline(t Timeline, q Query) Decision {
	from, need := g.policy.From, minutes(g.policy.Soak)
	d := Decision{Query: q}

	if cur, ok := t.Current(); ok && cur.Revision == q.Revision {
		soaked := q.Now.Sub(cur.Start)
		if soaked >= g.policy.Soak {
			d.Verdict = Promotable
			d.Reason = fmt.Sprintf("live and healthy in %s since %s (%s, soak needs %s)", from, ts(cur.Start), minutes(soaked), need)
			return d
		}
		d.Verdict, d.PromotableAt = Wait, cur.Start.Add(g.policy.Soak)
		d.Reason = fmt.Sprintf("live and healthy in %s since %s (%s of %s); promotable at %s if it stays live and healthy",
			from, ts(cur.Start), minutes(soaked), need, ts(d.PromotableAt))
		return d
	}

	d.Verdict = Blocked
	last, seen := t.Last(q.Revision)
	switch {
	case !seen:
		d.Reason = fmt.Sprintf("%s has never been live and healthy in %s. Deploy it to %s; it becomes promotable %s after it is live there", q.Revision, from, from, need)
		if elsewhere := g.liveElsewhere(q); elsewhere != "" {
			d.Reason += ". Note: " + elsewhere
		}
	case t.Live == q.Revision && !t.Healthy:
		d.Reason = fmt.Sprintf("unhealthy in %s since %s (event %s). Fix the pods; once it recovers or is redeployed, the soak starts again and needs %s", from, ts(last.End), last.EndedBy.ID, need)
	default:
		d.Reason = fmt.Sprintf("no longer live in %s: %s. Redeploy %s to %s and let it soak %s again", from, g.replacedBy(t, last), q.Revision, from, need)
		if t.Live != "" {
			alt := g.fromTimeline(t, Query{Now: q.Now, Service: q.Service, Revision: t.Live})
			d.Reason += fmt.Sprintf(", or promote the live revision %s instead (%s)", t.Live, alt.short())
		}
	}
	return d
}

func (g *Gate) replacedBy(t Timeline, last Stint) string {
	for _, s := range t.Stints {
		if s.Revision != last.Revision && !s.Start.Before(last.End) {
			return fmt.Sprintf("replaced by %s at %s", s.Revision, ts(s.Start))
		}
	}
	return fmt.Sprintf("interrupted at %s and %s is now live", ts(last.End), t.Live)
}

// liveElsewhere describes where the revision already runs, if outside From.
func (g *Gate) liveElsewhere(q Query) string {
	var envs []string
	seen := map[string]bool{}
	for _, e := range g.events {
		if e.Kind == Deployed && e.Service == q.Service && e.Revision == q.Revision && e.Env != g.policy.From && !e.At.After(q.Now) && !seen[e.Env] {
			seen[e.Env] = true
			envs = append(envs, fmt.Sprintf("it is already deployed to %s (at %s) although it never soaked in %s", e.Env, ts(e.At), g.policy.From))
		}
	}
	sort.Strings(envs)
	if len(envs) == 0 {
		return ""
	}
	return envs[0]
}

func (d Decision) short() string {
	if d.Verdict == Wait {
		return "WAIT until " + ts(d.PromotableAt)
	}
	return string(d.Verdict)
}

// AuditFinding is a past promotion the gate would not have allowed.
type AuditFinding struct {
	Event    Event
	Decision Decision
}

// Audit re-checks every promotion already recorded outside From: each deploy
// of a revision that was not already live there. A re-sync of the revision
// already live is not a promotion and is not checked.
func (g *Gate) Audit() []AuditFinding {
	var deploys []Event
	for _, e := range g.events {
		if e.Env != g.policy.From && e.Kind == Deployed {
			deploys = append(deploys, e)
		}
	}
	sort.SliceStable(deploys, func(i, j int) bool {
		if !deploys[i].At.Equal(deploys[j].At) {
			return deploys[i].At.Before(deploys[j].At)
		}
		return deploys[i].Line < deploys[j].Line
	})
	var out []AuditFinding
	live := map[string]string{}
	for _, e := range deploys {
		k := e.Env + "/" + e.Service
		if live[k] == e.Revision {
			continue
		}
		live[k] = e.Revision
		d := g.Decide(Query{Now: e.At, Service: e.Service, Revision: e.Revision})
		if d.Verdict != Promotable {
			out = append(out, AuditFinding{Event: e, Decision: d})
		}
	}
	return out
}

func ts(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func minutes(d time.Duration) string { return fmt.Sprintf("%dm", int(d/time.Minute)) }
