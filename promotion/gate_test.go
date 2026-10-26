package promotion

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// Fixtures are written as the producer would send them, so every test goes
// through parsing and ingestion exactly like the real input does.

const day = "2026-10-08T"

func ev(id, at, env, svc, kind, rev string) string {
	return fmt.Sprintf(`{"id":%q,"at":%q,"env":%q,"service":%q,"kind":%q,"revision":%q}`, id, day+at, env, svc, kind, rev)
}

// stg is shorthand for the common case: an stg event for service "svc".
func stg(id, at, kind, rev string) string { return ev(id, at+"Z", "stg", "svc", kind, rev) }

var policy = Policy{From: "stg", To: "prd", Soak: 120 * time.Minute}

func gate(t *testing.T, lines ...string) *Gate {
	t.Helper()
	g, err := Load(policy, strings.NewReader(strings.Join(lines, "\n")))
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func at(hhmm string) time.Time {
	t, err := time.Parse(time.RFC3339, day+hhmm+":00Z")
	if err != nil {
		panic(err)
	}
	return t
}

type want struct {
	verdict      Verdict
	promotableAt string   // "HH:MM" for WAIT
	reason       []string // substrings the reason must contain
}

func check(t *testing.T, g *Gate, now, service, rev string, w want) {
	t.Helper()
	d := g.Decide(Query{Now: at(now), Service: service, Revision: rev})
	if d.Verdict != w.verdict {
		t.Fatalf("%s %s at %s: verdict %s, want %s (reason: %s)", service, rev, now, d.Verdict, w.verdict, d.Reason)
	}
	if w.promotableAt != "" && !d.PromotableAt.Equal(at(w.promotableAt)) {
		t.Errorf("promotable at %s, want %s", ts(d.PromotableAt), w.promotableAt)
	}
	if w.verdict != Wait && !d.PromotableAt.IsZero() {
		t.Errorf("PromotableAt set on %s", d.Verdict)
	}
	for _, s := range w.reason {
		if !strings.Contains(d.Reason, s) {
			t.Errorf("reason %q does not mention %q", d.Reason, s)
		}
	}
}

func TestSoakRule(t *testing.T) {
	g := gate(t, stg("e1", "08:00:00", "deployed", "r1"))
	t.Run("not yet soaked is WAIT with the time it becomes promotable", func(t *testing.T) {
		check(t, g, "09:00", "svc", "r1", want{verdict: Wait, promotableAt: "10:00", reason: []string{"60m of 120m"}})
	})
	t.Run("exactly the soak time is enough", func(t *testing.T) {
		check(t, g, "10:00", "svc", "r1", want{verdict: Promotable})
	})
	t.Run("one minute short is not", func(t *testing.T) {
		check(t, g, "09:59", "svc", "r1", want{verdict: Wait, promotableAt: "10:00"})
	})
}

// Each test below is one way the input misleads a naive implementation.

func TestDuplicateDeliveryIsCountedOnce(t *testing.T) {
	g := gate(t,
		stg("e1", "08:00:00", "deployed", "r1"),
		stg("e1", "08:00:00", "deployed", "r1"),
	)
	check(t, g, "10:00", "svc", "r1", want{verdict: Promotable})
	assertDefect(t, g, DefectDuplicate, 2)
}

func TestSameIDDifferentContentKeepsBoth(t *testing.T) {
	// Deduplicating by id alone would drop the recovery and block r1 forever.
	g := gate(t,
		stg("e1", "08:00:00", "deployed", "r1"),
		stg("e2", "08:30:00", "degraded", "r1"),
		stg("e2", "09:00:00", "recovered", "r1"),
	)
	check(t, g, "11:00", "svc", "r1", want{verdict: Promotable})
	assertDefect(t, g, DefectIDConflict, 3)
}

func TestLinesOutOfTimeOrder(t *testing.T) {
	// File order says r1 replaced r2; time order says r2 replaced r1.
	g := gate(t,
		stg("e2", "09:00:00", "deployed", "r2"),
		stg("e1", "08:00:00", "deployed", "r1"),
	)
	check(t, g, "11:00", "svc", "r2", want{verdict: Promotable})
	check(t, g, "11:00", "svc", "r1", want{verdict: Blocked, reason: []string{"replaced by r2 at 2026-10-08T09:00:00Z"}})
}

func TestTimestampWithOffsetIsNormalized(t *testing.T) {
	// 10:30+01:00 is 09:30Z. Comparing strings, or dropping the offset, would
	// say the deploy happened an hour later than it did.
	g := gate(t, ev("e1", "10:30:00+01:00", "stg", "svc", "deployed", "r1"))
	check(t, g, "11:30", "svc", "r1", want{verdict: Promotable, reason: []string{"since 2026-10-08T09:30:00Z"}})
	assertDefect(t, g, DefectNonUTC, 1)
}

func TestEventsAfterNowAreNotYetKnown(t *testing.T) {
	g := gate(t,
		stg("e1", "08:00:00", "deployed", "r1"),
		stg("e2", "09:00:00", "degraded", "r1"),
		stg("e3", "09:15:00", "recovered", "r1"),
	)
	check(t, g, "09:10", "svc", "r1", want{verdict: Blocked, reason: []string{"unhealthy in stg since 2026-10-08T09:00:00Z"}})
	// A replacement in the future does not interrupt a soak that ends now.
	g = gate(t,
		stg("e1", "08:00:00", "deployed", "r1"),
		stg("e2", "12:00:00", "deployed", "r2"),
	)
	check(t, g, "10:00", "svc", "r1", want{verdict: Promotable})
}

func TestHealthEventForAnotherRevisionDoesNotInterrupt(t *testing.T) {
	// r1's late degradation arrives after r2 went live. It is about r1.
	g := gate(t,
		stg("e1", "06:00:00", "deployed", "r1"),
		stg("e2", "08:00:00", "deployed", "r2"),
		stg("e3", "09:00:00", "degraded", "r1"),
	)
	check(t, g, "10:00", "svc", "r2", want{verdict: Promotable})
}

func TestRecoveryOfAnotherRevisionDoesNotResume(t *testing.T) {
	g := gate(t,
		stg("e1", "06:00:00", "deployed", "r1"),
		stg("e2", "07:00:00", "degraded", "r1"),
		stg("e3", "07:30:00", "deployed", "r2"),
		stg("e4", "08:00:00", "recovered", "r1"),
	)
	check(t, g, "12:00", "svc", "r1", want{verdict: Blocked, reason: []string{"replaced by r2"}})
}

func TestRevisionThatComesBackStartsAgain(t *testing.T) {
	// A rollback to r1: its first stint does not count.
	g := gate(t,
		stg("e1", "06:00:00", "deployed", "r1"),
		stg("e2", "09:00:00", "deployed", "r2"),
		stg("e3", "10:30:00", "deployed", "r1"),
	)
	check(t, g, "12:00", "svc", "r1", want{verdict: Wait, promotableAt: "12:30"})
	check(t, g, "12:00", "svc", "r2", want{verdict: Blocked, reason: []string{"replaced by r1 at 2026-10-08T10:30:00Z", "promote the live revision r1 instead (WAIT until 2026-10-08T12:30:00Z)"}})
}

func TestHealthyTimeDoesNotAccumulateAcrossInterruptions(t *testing.T) {
	// 90m healthy, 15m degraded, 75m healthy: 165m in total, but the rule
	// wants 120m without interruption.
	g := gate(t,
		stg("e1", "09:00:00", "deployed", "r1"),
		stg("e2", "10:30:00", "degraded", "r1"),
		stg("e3", "10:45:00", "recovered", "r1"),
	)
	check(t, g, "12:00", "svc", "r1", want{verdict: Wait, promotableAt: "12:45"})
}

func TestOnlySourceEnvironmentCounts(t *testing.T) {
	// Without the env filter, prd's redeploy at 10:00 makes r1 look live for
	// exactly 120m at 12:00.
	g := gate(t,
		stg("e1", "05:00:00", "deployed", "r1"),
		stg("e2", "08:00:00", "deployed", "r2"),
		ev("e3", "10:00:00Z", "prd", "svc", "deployed", "r1"),
	)
	check(t, g, "12:00", "svc", "r1", want{verdict: Blocked, reason: []string{"replaced by r2"}})
}

func TestRevisionOnlyInPrdIsBlocked(t *testing.T) {
	g := gate(t, ev("e1", "08:15:00Z", "prd", "svc", "deployed", "r1"))
	check(t, g, "12:00", "svc", "r1", want{verdict: Blocked, reason: []string{"never been live and healthy in stg", "already deployed to prd"}})
}

func TestUnknownRevisionAndServiceAreBlocked(t *testing.T) {
	g := gate(t, stg("e1", "06:00:00", "deployed", "r1"))
	check(t, g, "12:00", "svc", "r9", want{verdict: Blocked, reason: []string{"Deploy it to stg"}})
	check(t, g, "12:00", "other", "r1", want{verdict: Blocked, reason: []string{"never been live"}})
}

func TestRecoveredWithoutDegradedChangesNothing(t *testing.T) {
	g := gate(t,
		stg("e1", "08:00:00", "deployed", "r1"),
		stg("e2", "09:00:00", "recovered", "r1"),
	)
	check(t, g, "10:00", "svc", "r1", want{verdict: Promotable})
}

func TestResyncOfLiveRevisionDoesNotRestartTheClock(t *testing.T) {
	g := gate(t,
		stg("e1", "08:00:00", "deployed", "r1"),
		stg("e2", "09:00:00", "deployed", "r1"),
	)
	check(t, g, "10:00", "svc", "r1", want{verdict: Promotable})
}

func TestRedeployOfDegradedRevisionStartsAgain(t *testing.T) {
	g := gate(t,
		stg("e1", "06:00:00", "deployed", "r1"),
		stg("e2", "07:00:00", "degraded", "r1"),
		stg("e3", "09:00:00", "deployed", "r1"),
	)
	check(t, g, "10:00", "svc", "r1", want{verdict: Wait, promotableAt: "11:00"})
}

func TestSameTimestampLeavesThePessimisticState(t *testing.T) {
	g := gate(t,
		stg("e2", "09:00:00", "degraded", "r1"),
		stg("e1", "09:00:00", "deployed", "r1"),
	)
	check(t, g, "12:00", "svc", "r1", want{verdict: Blocked, reason: []string{"unhealthy"}})
	g = gate(t,
		stg("e1", "06:00:00", "deployed", "r1"),
		stg("e2", "09:00:00", "recovered", "r1"),
		stg("e3", "09:00:00", "degraded", "r1"),
	)
	check(t, g, "12:00", "svc", "r1", want{verdict: Blocked})
}

func TestUnreadableLineFailsClosedForWhatItMayConcern(t *testing.T) {
	truncated := `{"id":"e9","at":"2026-10-08T11:05:00Z","env":"stg","service":"set`
	lines := []string{
		ev("e1", "08:00:00Z", "stg", "settlement-writer", "deployed", "r1"),
		ev("e2", "08:00:00Z", "stg", "odds-ingest", "deployed", "r1"),
		truncated,
	}
	g := gate(t, lines...)
	check(t, g, "12:00", "settlement-writer", "r1", want{verdict: Blocked, reason: []string{"line 3", "redeliver", "Without it the gate would say PROMOTABLE"}})
	t.Run("another service is not affected", func(t *testing.T) {
		check(t, g, "12:00", "odds-ingest", "r1", want{verdict: Promotable})
	})
	t.Run("before the lost event happened it cannot matter", func(t *testing.T) {
		check(t, g, "11:00", "settlement-writer", "r1", want{verdict: Promotable})
	})
	t.Run("nothing recoverable blocks everything", func(t *testing.T) {
		g := gate(t, ev("e1", "08:00:00Z", "stg", "odds-ingest", "deployed", "r1"), `{"id":`)
		check(t, g, "12:00", "odds-ingest", "r1", want{verdict: Blocked})
	})
	t.Run("an unreadable prd line does not block stg soak", func(t *testing.T) {
		g := gate(t, ev("e1", "08:00:00Z", "stg", "odds-ingest", "deployed", "r1"), `{"id":"e5","at":"2026-10-08T09:00:00Z","env":"prd","serv`)
		check(t, g, "12:00", "odds-ingest", "r1", want{verdict: Promotable})
	})
	assertDefect(t, g, DefectUnreadable, 3)
}

func TestInvalidEventFailsClosed(t *testing.T) {
	g := gate(t,
		stg("e1", "08:00:00", "deployed", "r1"),
		stg("e2", "09:00:00", "restarted", "r1"),
	)
	check(t, g, "12:00", "svc", "r1", want{verdict: Blocked, reason: []string{`unknown kind "restarted"`}})
	assertDefect(t, g, DefectInvalid, 2)
}

func TestAuditFlagsPromotionsWithoutSoak(t *testing.T) {
	g := gate(t,
		stg("e1", "05:00:00", "deployed", "r1"),
		ev("e2", "07:00:00Z", "prd", "svc", "deployed", "r1"), // soaked exactly 120m: fine
		ev("e3", "10:00:00Z", "prd", "svc", "deployed", "r1"), // re-sync, not a promotion
		ev("e4", "08:15:00Z", "prd", "ui", "deployed", "r5"),  // never in stg
	)
	findings := g.Audit()
	if len(findings) != 1 || findings[0].Event.ID != "e4" {
		t.Fatalf("audit = %+v, want only e4", findings)
	}
}

// The tests below came from an adversarial pass over the gate (docs/notes.md):
// each input was once accepted silently.

func TestUnknownEnvironmentFailsClosed(t *testing.T) {
	// "STG" is not stg, but it may well have been meant as stg.
	g := gate(t,
		stg("e1", "06:00:00", "deployed", "r1"),
		ev("e2", "09:00:00Z", "STG", "svc", "degraded", "r1"),
	)
	check(t, g, "12:00", "svc", "r1", want{verdict: Blocked, reason: []string{`env "STG"`, "Without it the gate would say PROMOTABLE"}})
	check(t, g, "08:59", "svc", "r1", want{verdict: Promotable}) // before the unusable event
	assertDefect(t, g, DefectUnknownEnv, 2)
}

func TestTwoRevisionsDeployedAtOneInstantBlockUntilTheNextDeploy(t *testing.T) {
	// Sorting by id would pick r1, because "e-10" < "e-9" as strings.
	g := gate(t,
		stg("e-9", "08:00:00", "deployed", "r1"),
		stg("e-10", "08:00:00", "deployed", "r2"),
		stg("e-11", "11:00:00", "deployed", "r3"),
	)
	check(t, g, "10:30", "svc", "r1", want{verdict: Blocked, reason: []string{"which one is live is unknown"}})
	check(t, g, "10:30", "svc", "r2", want{verdict: Blocked, reason: []string{"which one is live is unknown"}})
	check(t, g, "13:00", "svc", "r3", want{verdict: Promotable})
	assertDefect(t, g, DefectSameInstant, 1)
}

func TestOversizedLineIsADefectNotAnOutage(t *testing.T) {
	long := `{"id":"e2","at":"2026-10-08T09:00:00Z","env":"stg","service":"svc","kind":"degraded","revision":"r1","pad":"` + strings.Repeat("x", 2<<20) + `"}`
	g := gate(t, stg("e1", "06:00:00", "deployed", "r1"), long, ev("e3", "06:00:00Z", "stg", "other", "deployed", "r1"))
	check(t, g, "12:00", "svc", "r1", want{verdict: Blocked, reason: []string{"event e2 at 2026-10-08T09:00:00Z"}})
	check(t, g, "12:00", "other", "r1", want{verdict: Promotable})
	assertDefect(t, g, DefectUnreadable, 2)
}

func TestDuplicateFieldFailsClosed(t *testing.T) {
	// encoding/json keeps the last of two equal keys without a word.
	g := gate(t, `{"id":"e1","at":"2026-10-08T06:00:00Z","env":"stg","service":"svc","kind":"deployed","revision":"r1","revision":"r2"}`)
	check(t, g, "12:00", "svc", "r2", want{verdict: Blocked, reason: []string{`field "revision" appears twice`}})
	assertDefect(t, g, DefectInvalid, 1)
}

func TestUnknownFieldIsReportedAndKindDecides(t *testing.T) {
	g := gate(t, `{"id":"e1","at":"2026-10-08T06:00:00Z","env":"stg","service":"svc","kind":"deployed","revision":"r1","healthy":false}`)
	check(t, g, "12:00", "svc", "r1", want{verdict: Promotable})
	assertDefect(t, g, DefectUnknownField, 1)
}

func TestHealthEventBeforeItsDeployAppliesFromTheDeploy(t *testing.T) {
	// A producer whose clock is 1s behind reports the new revision degraded
	// before Argo CD reports it deployed.
	g := gate(t,
		stg("e2", "08:59:59", "degraded", "r1"),
		stg("e1", "09:00:00", "deployed", "r1"),
	)
	check(t, g, "12:00", "svc", "r1", want{verdict: Blocked, reason: []string{"unhealthy in stg since 2026-10-08T09:00:00Z (event e2)"}})
	assertDefect(t, g, DefectEarlyHealth, 1)

	g = gate(t,
		stg("e2", "08:59:59", "degraded", "r1"),
		stg("e1", "09:00:00", "deployed", "r1"),
		stg("e3", "09:30:00", "recovered", "r1"),
	)
	check(t, g, "11:30", "svc", "r1", want{verdict: Promotable})
}

func TestResyncAfterDegradedRestartsTheClock(t *testing.T) {
	// The contract says deployed means synced and healthy, so a re-sync after
	// a degradation is a recovery. This pins that reading; see docs/notes.md.
	g := gate(t,
		stg("e1", "06:00:00", "deployed", "r1"),
		stg("e2", "07:00:00", "degraded", "r1"),
		stg("e3", "07:01:00", "deployed", "r1"),
	)
	check(t, g, "09:00", "svc", "r1", want{verdict: Wait, promotableAt: "09:01"})
}

func assertDefect(t *testing.T, g *Gate, kind DefectKind, line int) {
	t.Helper()
	for _, d := range g.Defects() {
		if d.Kind == kind && d.Line == line {
			return
		}
	}
	t.Errorf("no %s defect on line %d; defects: %v", kind, line, g.Defects())
}
