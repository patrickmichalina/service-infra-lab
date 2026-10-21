// Package promotion decides whether a revision of a service has soaked long
// enough in the source environment (stg) to be promoted to prd.
//
// The pipeline is deliberately linear so each step can be read and tested on
// its own:
//
//	ParseEvents   raw lines   -> Events + Defects (nothing is dropped silently)
//	Ingest        Events      -> deduplicated Events + more Defects
//	Timeline      Events, now -> the Stints of one service (continuous live+healthy spans)
//	Gate.Decide   Query       -> Decision (PROMOTABLE / WAIT / BLOCKED + reason)
package promotion

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"
)

// Kind is what happened to a revision.
type Kind string

const (
	Deployed  Kind = "deployed"  // revision is now the live revision, synced and healthy
	Degraded  Kind = "degraded"  // pods of revision went unhealthy
	Recovered Kind = "recovered" // pods of revision are healthy again
)

func (k Kind) valid() bool { return k == Deployed || k == Degraded || k == Recovered }

// Event is one well-formed deployment event, with At normalized to UTC.
type Event struct {
	ID       string
	At       time.Time
	Env      string
	Service  string
	Kind     Kind
	Revision string
	Line     int // 1-based line in the input, for diagnostics only
}

// key is the identity of an event's content. Two deliveries with the same key
// are the same fact; Line is not part of it.
func (e Event) key() string {
	return strings.Join([]string{e.ID, e.At.Format(time.RFC3339), e.Env, e.Service, string(e.Kind), e.Revision}, "|")
}

type rawEvent struct {
	ID       string `json:"id"`
	At       string `json:"at"`
	Env      string `json:"env"`
	Service  string `json:"service"`
	Kind     string `json:"kind"`
	Revision string `json:"revision"`
}

// ParseEvents reads one JSON object per line. Lines that cannot be used become
// Defects carrying a Taint: whatever the line still says about which env,
// service and time it concerns, so the gate can fail closed for exactly the
// queries the lost event might have changed.
func ParseEvents(r io.Reader) ([]Event, []Defect, error) {
	var events []Event
	var defects []Defect
	sc := bufio.NewScanner(r)
	line := 0
	for sc.Scan() {
		line++
		text := strings.TrimSpace(sc.Text())
		if text == "" {
			continue
		}
		var raw rawEvent
		if err := json.Unmarshal([]byte(text), &raw); err != nil {
			t := salvage(text)
			defects = append(defects, Defect{
				Line:   line,
				Kind:   DefectUnreadable,
				ID:     t.ID,
				Detail: fmt.Sprintf("not valid JSON (%v); recoverable fields: %s", err, t),
				Taint:  &t,
			})
			continue
		}
		ev, problem := validate(raw, line)
		if problem != "" {
			t := Taint{ID: raw.ID, Env: raw.Env, Service: raw.Service}
			if at, err := time.Parse(time.RFC3339, raw.At); err == nil {
				t.At = at.UTC()
			}
			defects = append(defects, Defect{Line: line, Kind: DefectInvalid, ID: raw.ID, Detail: problem, Taint: &t})
			continue
		}
		if !strings.HasSuffix(raw.At, "Z") {
			defects = append(defects, Defect{
				Line: line, Kind: DefectNonUTC, ID: raw.ID,
				Detail: fmt.Sprintf("timestamp %q is not UTC; read as %s", raw.At, ev.At.Format(time.RFC3339)),
			})
		}
		events = append(events, ev)
	}
	return events, defects, sc.Err()
}

func validate(raw rawEvent, line int) (Event, string) {
	var missing []string
	for name, v := range map[string]string{"id": raw.ID, "at": raw.At, "env": raw.Env, "service": raw.Service, "kind": raw.Kind, "revision": raw.Revision} {
		if v == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		sortStrings(missing)
		return Event{}, "missing field(s): " + strings.Join(missing, ", ")
	}
	at, err := time.Parse(time.RFC3339, raw.At)
	if err != nil {
		return Event{}, fmt.Sprintf("unparseable timestamp %q", raw.At)
	}
	if !Kind(raw.Kind).valid() {
		return Event{}, fmt.Sprintf("unknown kind %q", raw.Kind)
	}
	return Event{ID: raw.ID, At: at.UTC(), Env: raw.Env, Service: raw.Service, Kind: Kind(raw.Kind), Revision: raw.Revision, Line: line}, ""
}

var (
	completeField  = regexp.MustCompile(`"(\w+)"\s*:\s*"([^"]*)"`)
	truncatedField = regexp.MustCompile(`"(\w+)"\s*:\s*"([^"]*)$`)
)

// salvage recovers what a truncated line still says. A field cut off mid-value
// is kept as a prefix (only the service name matters for that).
func salvage(text string) Taint {
	var t Taint
	set := func(k, v string, truncated bool) {
		switch k {
		case "id":
			if !truncated {
				t.ID = v
			}
		case "env":
			if !truncated {
				t.Env = v
			}
		case "service":
			t.Service, t.ServiceIsPrefix = v, truncated
		case "at":
			if at, err := time.Parse(time.RFC3339, v); err == nil && !truncated {
				t.At = at.UTC()
			}
		}
	}
	for _, m := range completeField.FindAllStringSubmatch(text, -1) {
		set(m[1], m[2], false)
	}
	if m := truncatedField.FindStringSubmatch(text); m != nil {
		set(m[1], m[2], true)
	}
	return t
}
