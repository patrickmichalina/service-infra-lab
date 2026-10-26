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

// maxLine bounds one event. A longer line is a defect, not a reason to stop
// reading the stream: one producer's oversized payload must not take the gate
// down for every other service.
const maxLine = 1 << 20

// fields are the event contract. Anything else on a line is reported.
var fields = map[string]bool{"id": true, "at": true, "env": true, "service": true, "kind": true, "revision": true}

// ParseEvents reads one JSON object per line. Lines that cannot be used become
// Defects carrying a Taint: whatever the line still says about which env,
// service and time it concerns, so the gate can fail closed for exactly the
// queries the lost event might have changed.
func ParseEvents(r io.Reader) ([]Event, []Defect, error) {
	var events []Event
	var defects []Defect
	unreadable := func(line int, text, why string) {
		t := salvage(text)
		defects = append(defects, Defect{Line: line, Kind: DefectUnreadable, ID: t.ID,
			Detail: fmt.Sprintf("%s; recoverable fields: %s", why, t), Taint: &t})
	}
	br := bufio.NewReader(r)
	for line := 1; ; line++ {
		b, tooLong, err := readLine(br)
		if err != nil && err != io.EOF {
			return nil, nil, err
		}
		text := strings.TrimSpace(string(b))
		switch {
		case tooLong:
			unreadable(line, text, fmt.Sprintf("longer than %d bytes", maxLine))
		case text == "":
		default:
			parseLine(line, text, &events, &defects, unreadable)
		}
		if err == io.EOF {
			return events, defects, nil
		}
	}
}

func parseLine(line int, text string, events *[]Event, defects *[]Defect, unreadable func(int, string, string)) {
	var raw rawEvent
	if err := json.Unmarshal([]byte(text), &raw); err != nil {
		unreadable(line, text, fmt.Sprintf("not valid JSON (%v)", err))
		return
	}
	keys, dup := topLevelKeys(text)
	ev, problem := validate(raw, line)
	if problem == "" && dup != "" {
		problem = fmt.Sprintf("field %q appears twice, so which value is meant is unknown", dup)
	}
	if problem != "" {
		t := Taint{ID: raw.ID, Env: raw.Env, Service: raw.Service}
		if at, err := time.Parse(time.RFC3339, raw.At); err == nil {
			t.At = at.UTC()
		}
		*defects = append(*defects, Defect{Line: line, Kind: DefectInvalid, ID: raw.ID, Detail: problem, Taint: &t})
		return
	}
	var extra []string
	for _, k := range keys {
		if !fields[k] {
			extra = append(extra, k)
		}
	}
	if len(extra) > 0 {
		*defects = append(*defects, Defect{Line: line, Kind: DefectUnknownField, ID: raw.ID,
			Detail: fmt.Sprintf("field(s) %s are not in the event contract; ignored, the verdict follows kind %q", strings.Join(extra, ", "), raw.Kind)})
	}
	if !strings.HasSuffix(raw.At, "Z") {
		*defects = append(*defects, Defect{
			Line: line, Kind: DefectNonUTC, ID: raw.ID,
			Detail: fmt.Sprintf("timestamp %q is not UTC; read as %s", raw.At, ev.At.Format(time.RFC3339)),
		})
	}
	*events = append(*events, ev)
}

// readLine returns the next line without its newline. A line longer than
// maxLine is returned cut at maxLine with tooLong set; the rest is skipped.
func readLine(br *bufio.Reader) (b []byte, tooLong bool, err error) {
	for {
		chunk, isPrefix, err := br.ReadLine()
		if len(b) < maxLine {
			b = append(b, chunk...)
		}
		if len(b) > maxLine {
			b, tooLong = b[:maxLine], true
		}
		if err != nil || !isPrefix {
			if err == io.EOF && len(b) > 0 {
				err = nil // the last line has no newline; EOF comes on the next call
			}
			return b, tooLong, err
		}
	}
}

// topLevelKeys returns the object's keys in order and the first key that
// occurs twice. encoding/json keeps the last of two equal keys silently.
func topLevelKeys(text string) (keys []string, dup string) {
	dec := json.NewDecoder(strings.NewReader(text))
	if _, err := dec.Token(); err != nil { // {
		return nil, ""
	}
	seen := map[string]bool{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return keys, dup
		}
		k, _ := tok.(string)
		if seen[k] && dup == "" {
			dup = k
		}
		seen[k] = true
		keys = append(keys, k)
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			return keys, dup
		}
	}
	return keys, dup
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
