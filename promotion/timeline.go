package promotion

import (
	"sort"
	"time"
)

// Stint is one uninterrupted span during which Revision was the live, healthy
// revision of a service in an environment. This is the unit the soak rule is
// about: a revision soaks only within one stint, never across several.
type Stint struct {
	Revision string
	Start    time.Time
	End      time.Time // zero while the stint is still running
	EndedBy  *Event    // the event that interrupted it; nil while running
}

// Running reports whether the stint has not been interrupted.
func (s Stint) Running() bool { return s.EndedBy == nil }

// Timeline is the history of one service in one environment, up to a moment.
type Timeline struct {
	Stints []Stint
	// Live is the revision live at the end of the timeline ("" if none), and
	// Healthy whether its pods are healthy.
	Live    string
	Healthy bool
	// Ignored are health events about a revision that was not live when they
	// happened. They are facts about that revision, but cannot interrupt (or
	// resume) a stint of a revision they do not name.
	Ignored []Event
}

// kindOrder breaks ties between events with the same timestamp. A deploy is
// applied first; recovered before degraded, so that at a tie the pessimistic
// state (unhealthy) is the one left standing.
var kindOrder = map[Kind]int{Deployed: 0, Recovered: 1, Degraded: 2}

// BuildTimeline folds the events of service in env that happened at or before
// now. Events after now are not yet known at now and are excluded; that is
// what makes the result depend only on its inputs.
func BuildTimeline(events []Event, env, service string, now time.Time) Timeline {
	var evs []Event
	for _, e := range events {
		if e.Env == env && e.Service == service && !e.At.After(now) {
			evs = append(evs, e)
		}
	}
	sort.SliceStable(evs, func(i, j int) bool {
		a, b := evs[i], evs[j]
		if !a.At.Equal(b.At) {
			return a.At.Before(b.At)
		}
		if kindOrder[a.Kind] != kindOrder[b.Kind] {
			return kindOrder[a.Kind] < kindOrder[b.Kind]
		}
		if a.ID != b.ID {
			return a.ID < b.ID
		}
		return a.Line < b.Line
	})

	var t Timeline
	end := func(e Event) {
		s := &t.Stints[len(t.Stints)-1]
		ev := e
		s.End, s.EndedBy = e.At, &ev
	}
	start := func(e Event) {
		t.Stints = append(t.Stints, Stint{Revision: e.Revision, Start: e.At})
	}

	for _, e := range evs {
		switch e.Kind {
		case Deployed:
			if e.Revision == t.Live && t.Healthy {
				continue // a re-sync of the healthy live revision interrupts nothing
			}
			if t.Healthy {
				end(e)
			}
			t.Live, t.Healthy = e.Revision, true
			start(e)
		case Degraded:
			if e.Revision != t.Live {
				t.Ignored = append(t.Ignored, e)
				continue
			}
			if t.Healthy {
				end(e)
				t.Healthy = false
			}
		case Recovered:
			if e.Revision != t.Live {
				t.Ignored = append(t.Ignored, e)
				continue
			}
			if !t.Healthy {
				t.Healthy = true
				start(e)
			}
		}
	}
	return t
}

// Last returns the most recent stint of revision, if any.
func (t Timeline) Last(revision string) (Stint, bool) {
	for i := len(t.Stints) - 1; i >= 0; i-- {
		if t.Stints[i].Revision == revision {
			return t.Stints[i], true
		}
	}
	return Stint{}, false
}

// Current returns the running stint, if the live revision is healthy.
func (t Timeline) Current() (Stint, bool) {
	if n := len(t.Stints); n > 0 && t.Stints[n-1].Running() {
		return t.Stints[n-1], true
	}
	return Stint{}, false
}
