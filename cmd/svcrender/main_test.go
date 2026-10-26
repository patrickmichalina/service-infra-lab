package main

import (
	"bytes"
	"flag"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/patrickmichalina/service-infra-lab/internal/svc"
)

var update = flag.Bool("update", false, "rewrite out/render and out/render-fixed")

// TestGolden pins every rendered object and verdict for the four requests as
// written (out/render) and as fixed after reading the verdicts (out/render-fixed).
func TestGolden(t *testing.T) {
	p, err := svc.LoadPlatform("../../inputs", "../../platform", envs...)
	if err != nil {
		t.Fatal(err)
	}
	for specs, golden := range map[string]string{"../../specs": "../../out/render", "../../specs/fixed": "../../out/render-fixed"} {
		got := filepath.Join(t.TempDir(), "out")
		if err := all(io.Discard, p, specs, got); err != nil {
			t.Fatal(err)
		}
		if *update {
			os.RemoveAll(golden)
			if err := os.Rename(got, golden); err != nil {
				t.Fatal(err)
			}
			continue
		}
		a, b := tree(t, got), tree(t, golden)
		for name, want := range b {
			if a[name] != want {
				t.Errorf("%s/%s differs; if intended, run: go test ./cmd/svcrender -update\n--- got\n%s", golden, name, a[name])
			}
		}
		for name := range a {
			if _, ok := b[name]; !ok {
				t.Errorf("%s/%s is new; run: go test ./cmd/svcrender -update", golden, name)
			}
		}
	}
}

func TestRenderIsDeterministic(t *testing.T) {
	p, err := svc.LoadPlatform("../../inputs", "../../platform", envs...)
	if err != nil {
		t.Fatal(err)
	}
	dirs := []string{filepath.Join(t.TempDir(), "a"), filepath.Join(t.TempDir(), "b")}
	for _, d := range dirs {
		if err := all(io.Discard, p, "../../specs", d); err != nil {
			t.Fatal(err)
		}
	}
	a, b := tree(t, dirs[0]), tree(t, dirs[1])
	if len(a) == 0 || len(a) != len(b) {
		t.Fatalf("%d vs %d files", len(a), len(b))
	}
	for name := range a {
		if a[name] != b[name] {
			t.Errorf("%s differs between two runs", name)
		}
	}
}

// TestFixedSpecsAreClean proves each refusal was actionable: following the
// fixes gives specs that render in full, with nothing left to look at.
func TestFixedSpecsAreClean(t *testing.T) {
	var out bytes.Buffer
	p, _ := svc.LoadPlatform("../../inputs", "../../platform", envs...)
	if err := all(&out, p, "../../specs/fixed", filepath.Join(t.TempDir(), "o")); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), " - ") || strings.Count(out.String(), "ACCEPT") != 8 {
		t.Fatalf("fixed specs are not all clean ACCEPTs:\n%s", out.String())
	}
}

func tree(t *testing.T, root string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		files[rel] = string(b)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}
