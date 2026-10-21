package svc

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Severity says whether a finding stops something from being applied.
type Severity string

const (
	Error   Severity = "error"   // refused
	Warning Severity = "warning" // applied, but the engineer should look
)

// Finding is one problem, phrased for the engineer who wrote the spec: what is
// wrong, where, and exactly what to write instead.
type Finding struct {
	Rule     string // R1..R8, or SCHEMA / REF / NET for checks that are not a rule
	Severity Severity
	Envs     []string // environments it holds in; empty: every environment
	Path     string // location in the spec, e.g. permissions[0].actions[0]
	Problem  string
	Fix      string
}

func (f Finding) String() string {
	where := ""
	if len(f.Envs) > 0 {
		where = " [" + strings.Join(f.Envs, ", ") + "]"
	}
	s := fmt.Sprintf("%s %s%s at %s: %s", f.Severity, f.Rule, where, f.Path, f.Problem)
	if f.Fix != "" {
		s += "\n      fix: " + f.Fix
	}
	return s
}

var itemPath = regexp.MustCompile(`^(ingress|egress|permissions|secrets)\[\d+\]`)

// Item is the part of the spec an error refuses: one list entry
// ("egress[1]"), every permission ("permissions", for R8), or "" for the whole
// spec. Refusing an item leaves the rest of the spec renderable.
func (f Finding) Item() string {
	if m := itemPath.FindString(f.Path); m != "" {
		return m
	}
	if f.Path == "permissions" {
		return "permissions"
	}
	return ""
}

func errorf(rule, path, fix, problem string, args ...any) Finding {
	return Finding{Rule: rule, Severity: Error, Path: path, Problem: fmt.Sprintf(problem, args...), Fix: fix}
}

func warnf(rule, path, fix, problem string, args ...any) Finding {
	return Finding{Rule: rule, Severity: Warning, Path: path, Problem: fmt.Sprintf(problem, args...), Fix: fix}
}

var ruleOrder = map[string]int{"SCHEMA": 0, "R1": 1, "R2": 2, "R3": 3, "R4": 4, "R5": 5, "R6": 6, "R7": 7, "R8": 8, "REF": 9, "NET": 10}

func sortFindings(fs []Finding) {
	sort.SliceStable(fs, func(i, j int) bool {
		a, b := fs[i], fs[j]
		if a.Severity != b.Severity {
			return a.Severity == Error
		}
		if ruleOrder[a.Rule] != ruleOrder[b.Rule] {
			return ruleOrder[a.Rule] < ruleOrder[b.Rule]
		}
		return a.Path < b.Path
	})
}
