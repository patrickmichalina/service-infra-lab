package promotion

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// DefectKind classifies a problem found in the input.
type DefectKind string

const (
	DefectUnreadable DefectKind = "unreadable"  // line is not valid JSON
	DefectInvalid    DefectKind = "invalid"     // JSON, but a field is missing or wrong
	DefectNonUTC     DefectKind = "non-utc"     // valid, but not in UTC; normalized
	DefectDuplicate  DefectKind = "duplicate"   // same event delivered again; ignored
	DefectIDConflict DefectKind = "id-conflict" // same id, different content; both kept

	DefectUnknownField DefectKind = "unknown-field"        // valid, with fields outside the contract; reported
	DefectUnknownEnv   DefectKind = "unknown-env"          // names an environment the gate does not know
	DefectSameInstant  DefectKind = "same-instant"         // two revisions deployed at one instant: order unknown
	DefectEarlyHealth  DefectKind = "health-before-deploy" // about a revision not yet deployed there
)

// Defect is something wrong with the input. Every defect is reported; only
// those with a Taint can change a verdict.
type Defect struct {
	Line   int
	Kind   DefectKind
	ID     string
	Detail string
	Taint  *Taint
}

func (d Defect) String() string {
	id := ""
	if d.ID != "" {
		id = " " + d.ID
	}
	return fmt.Sprintf("line %d%s: %s: %s", d.Line, id, d.Kind, d.Detail)
}

func (d Defect) problem() string {
	if d.Kind == DefectUnreadable {
		return "not valid JSON"
	}
	return d.Detail
}

// remedy says what makes a tainting defect stop blocking.
func (d Defect) remedy() string {
	if d.Kind == DefectSameInstant {
		return "The next deploy to that environment decides; until then, which revision soaked is unknown"
	}
	return "Have the producer redeliver it; it then decides"
}

// Taint is what we still know about an event we could not use. Zero fields
// mean "unknown", and unknown matches everything: we fail closed.
type Taint struct {
	ID              string
	At              time.Time
	Env             string
	Service         string
	ServiceIsPrefix bool      // the line was cut off inside the service name
	Until           time.Time // a later fact that makes the lost one irrelevant; zero: none
}

// Concerns reports whether the lost event could have changed a decision about
// service in env at time now.
func (t Taint) Concerns(env, service string, now time.Time) bool {
	if t.Env != "" && t.Env != env {
		return false
	}
	if !t.At.IsZero() && t.At.After(now) {
		return false
	}
	if !t.Until.IsZero() && !now.Before(t.Until) {
		return false
	}
	switch {
	case t.Service == "":
		return true
	case t.ServiceIsPrefix:
		return strings.HasPrefix(service, t.Service)
	default:
		return t.Service == service
	}
}

func (t Taint) String() string {
	or := func(v, unknown string) string {
		if v == "" {
			return unknown
		}
		return v
	}
	at := "time unknown"
	if !t.At.IsZero() {
		at = "at " + t.At.Format(time.RFC3339)
	}
	svc := or(t.Service, "service unknown")
	if t.ServiceIsPrefix {
		svc = fmt.Sprintf("service starting %q", t.Service)
	}
	return fmt.Sprintf("event %s %s, env %s, %s", or(t.ID, "(id unknown)"), at, or(t.Env, "unknown"), svc)
}

// Ingest removes repeated deliveries. At-least-once delivery means the same
// event can arrive twice with identical content; that is expected and safe to
// collapse. The same id with different content is a producer bug: we cannot
// tell which is true, so both are kept and the conflict is reported.
func Ingest(events []Event) ([]Event, []Defect) {
	var out []Event
	var defects []Defect
	byKey := map[string]Event{}
	byID := map[string]Event{}
	for _, e := range events {
		if first, seen := byKey[e.key()]; seen {
			defects = append(defects, Defect{Line: e.Line, Kind: DefectDuplicate, ID: e.ID,
				Detail: fmt.Sprintf("identical to line %d; counted once", first.Line)})
			continue
		}
		if other, seen := byID[e.ID]; seen {
			defects = append(defects, Defect{Line: e.Line, Kind: DefectIDConflict, ID: e.ID,
				Detail: fmt.Sprintf("id also used on line %d with different content (%s %s at %s); both kept, the producer must issue unique ids",
					other.Line, other.Kind, other.Revision, other.At.Format(time.RFC3339))})
		} else {
			byID[e.ID] = e
		}
		byKey[e.key()] = e
		out = append(out, e)
	}
	return out, defects
}

func sortDefects(ds []Defect) {
	sort.SliceStable(ds, func(i, j int) bool { return ds[i].Line < ds[j].Line })
}

func sortStrings(s []string) { sort.Strings(s) }
