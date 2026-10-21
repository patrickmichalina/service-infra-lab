// soakgate answers, for each query, whether a revision may be promoted to prd.
//
//	soakgate -config inputs/platform-config/prd.yaml \
//	         -events inputs/events/deploy-events.jsonl \
//	         -queries inputs/events/queries.txt
//
// It never reads the wall clock: every query carries its own "now".
// Exit status is 0 when it could answer every query, whatever the verdicts.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"text/tabwriter"

	"github.com/patrickmichalina/platform-takehome/promotion"
)

func main() {
	config := flag.String("config", "inputs/platform-config/prd.yaml", "platform config of the promotion target")
	events := flag.String("events", "inputs/events/deploy-events.jsonl", "deployment event stream (JSON lines)")
	queries := flag.String("queries", "inputs/events/queries.txt", "queries: now service revision")
	flag.Parse()
	if err := run(os.Stdout, *config, *events, *queries); err != nil {
		fmt.Fprintln(os.Stderr, "soakgate:", err)
		os.Exit(1)
	}
}

func run(w io.Writer, configPath, eventsPath, queriesPath string) error {
	policy, err := promotion.LoadPolicy(configPath)
	if err != nil {
		return err
	}
	ef, err := os.Open(eventsPath)
	if err != nil {
		return err
	}
	defer ef.Close()
	gate, err := promotion.Load(policy, ef)
	if err != nil {
		return err
	}
	qf, err := os.Open(queriesPath)
	if err != nil {
		return err
	}
	defer qf.Close()
	qs, err := promotion.ParseQueries(qf)
	if err != nil {
		return err
	}

	fmt.Fprintf(w, "# soak rule: live and healthy in %s without interruption for %dm, ending at now\n\n", policy.From, int(policy.Soak.Minutes()))
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NOW\tSERVICE\tREVISION\tVERDICT\tREASON")
	for _, q := range qs {
		d := gate.Decide(q)
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", q.Now.Format("2006-01-02T15:04:05Z07:00"), q.Service, q.Revision, d.Verdict, d.Reason)
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	fmt.Fprintln(w, "\n# input defects (none dropped silently)")
	for _, d := range gate.Defects() {
		fmt.Fprintln(w, "-", d)
	}
	fmt.Fprintln(w, "\n# past promotions the gate would have refused")
	findings := gate.Audit()
	if len(findings) == 0 {
		fmt.Fprintln(w, "- none")
	}
	for _, f := range findings {
		fmt.Fprintf(w, "- line %d %s: %s %s deployed to %s at %s: gate says %s: %s\n",
			f.Event.Line, f.Event.ID, f.Event.Service, f.Event.Revision, f.Event.Env,
			f.Event.At.Format("2006-01-02T15:04:05Z07:00"), f.Decision.Verdict, f.Decision.Reason)
	}
	return nil
}
