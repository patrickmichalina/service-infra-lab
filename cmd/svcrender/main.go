// svcrender checks service specs against the platform rules and renders the
// objects the platform applies.
//
//	svcrender check  -repo example-org/odds-ingest -spec specs/odds-ingest.yaml -env stg
//	svcrender render -repo example-org/odds-ingest -spec specs/odds-ingest.yaml -env prd -out /tmp/out
//	svcrender all    -specs specs -out out/render
//
// -repo is the repository the spec was read from. In CI it is the job's own
// project path, never something the spec says; the registry is keyed by it.
// all stands in for that by taking each file's repository from the registry
// entry named like the file (specs/odds-ingest.yaml: example-org/odds-ingest).
//
// It reads only local files and never the clock or the network, so the same
// inputs always give the same bytes. check and render exit 1 when anything is
// refused; all always writes its report.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/patrickmichalina/service-infra-lab/internal/svc"
)

var envs = []string{"stg", "prd"}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	fl := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
	inputs := fl.String("inputs", "inputs", "directory holding platform-config/")
	platformDir := fl.String("platform", "platform", "platform-owned registry, catalog, usage and pointers")
	spec := fl.String("spec", "", "spec file (check, render)")
	repo := fl.String("repo", "", "repository the spec was read from (check, render)")
	env := fl.String("env", "", "environment (check, render)")
	specs := fl.String("specs", "specs", "directory of spec files (all)")
	out := fl.String("out", "", "output directory (render, all)")
	fl.Parse(os.Args[2:])

	p, err := svc.LoadPlatform(*inputs, *platformDir, envs...)
	if err != nil {
		fail(err)
	}
	switch os.Args[1] {
	case "check", "render":
		if *spec == "" || *env == "" || *repo == "" {
			fail(fmt.Errorf("%s needs -repo, -spec and -env", os.Args[1]))
		}
		s, err := svc.LoadSpec(*spec, *repo)
		if err != nil {
			fail(err)
		}
		r := svc.Evaluate(s, p, *env)
		writeResults(os.Stdout, []svc.Result{r}, nil, "")
		if os.Args[1] == "render" && *out != "" {
			if err := writeObjects(*out, r); err != nil {
				fail(err)
			}
		}
		if r.Errors() > 0 {
			os.Exit(1)
		}
	case "all":
		if *out == "" {
			fail(fmt.Errorf("all needs -out"))
		}
		if err := all(os.Stdout, p, *specs, *out); err != nil {
			fail(err)
		}
	default:
		usage()
	}
}

func all(w io.Writer, p *svc.Platform, specDir, outDir string) error {
	files, err := filepath.Glob(filepath.Join(specDir, "*.yaml"))
	if err != nil {
		return err
	}
	sort.Strings(files)
	var specs []*svc.Spec
	for _, f := range files {
		name := strings.TrimSuffix(filepath.Base(f), ".yaml")
		reg, ok := p.Registry[name]
		if !ok {
			return fmt.Errorf("%s: no registry entry named %s to take its repository from", f, name)
		}
		s, err := svc.LoadSpec(f, reg.Repo)
		if err != nil {
			return err
		}
		specs = append(specs, s)
	}
	if err := os.RemoveAll(outDir); err != nil {
		return err
	}
	fleet := svc.CheckFleet(specs, p)
	var results []svc.Result
	for _, s := range specs {
		for _, env := range envs {
			r := svc.Evaluate(s, p, env)
			if err := writeObjects(outDir, r); err != nil {
				return err
			}
			results = append(results, r)
		}
		if name, reg, ok := p.Lookup(s.Repo); ok {
			if err := writeObject(filepath.Join(outDir, "dev", name), svc.RenderECR(name, reg)); err != nil {
				return err
			}
		}
	}
	f, err := os.Create(filepath.Join(outDir, "verdicts.txt"))
	if err != nil {
		return err
	}
	defer f.Close()
	writeResults(io.MultiWriter(f, w), results, fleet, outDir)
	return nil
}

func writeObjects(outDir string, r svc.Result) error {
	for _, o := range r.Objects {
		if err := writeObject(filepath.Join(outDir, r.Env, r.Service), o); err != nil {
			return err
		}
	}
	return nil
}

func writeObject(dir string, o svc.Object) error {
	b, err := o.YAML()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, o.Name), b, 0o644)
}

// writeResults prints, per service, one verdict line per environment and then
// every finding once (a finding that holds in both environments is shown once).
func writeResults(w io.Writer, results []svc.Result, fleet map[string][]svc.Finding, outDir string) {
	var order []string
	byService := map[string][]svc.Result{}
	for _, r := range results {
		if _, ok := byService[r.Service]; !ok {
			order = append(order, r.Service)
		}
		byService[r.Service] = append(byService[r.Service], r)
	}
	for i, name := range order {
		if i > 0 {
			fmt.Fprintln(w)
		}
		fmt.Fprintf(w, "== %s\n", name)
		index := map[string]int{} // finding without its envs -> position in findings
		var findings []svc.Finding
		for _, r := range byService[name] {
			where := ""
			if outDir != "" && len(r.Objects) > 0 {
				where = fmt.Sprintf(" -> %s/", filepath.Join(r.Env, r.Service)) // relative to the output directory
			}
			fmt.Fprintf(w, "   %s  %-7s  %d error(s), %d object(s)%s\n", r.Env, r.Verdict, r.Errors(), len(r.Objects), where)
			for _, n := range r.Notes {
				fmt.Fprintf(w, "        note: %s\n", n)
			}
			for _, f := range r.Findings {
				key := f
				key.Envs = nil
				if i, ok := index[key.String()]; ok {
					if len(f.Envs) > 0 {
						findings[i].Envs = append(findings[i].Envs, f.Envs...)
					}
					continue
				}
				index[key.String()] = len(findings)
				findings = append(findings, f)
			}
		}
		findings = append(findings, fleet[name]...)
		for _, f := range findings {
			fmt.Fprintf(w, "   - %s\n", strings.ReplaceAll(f.String(), "\n", "\n  "))
		}
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: svcrender check|render|all [flags]  (svcrender <cmd> -h for flags)")
	os.Exit(2)
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "svcrender:", err)
	os.Exit(1)
}
