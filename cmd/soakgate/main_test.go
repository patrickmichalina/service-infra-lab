package main

import (
	"bytes"
	"flag"
	"os"
	"testing"
)

var update = flag.Bool("update", false, "rewrite the golden file")

// TestGolden pins the gate's output on the real inputs. The expected verdicts
// were worked out by hand first (docs/notes.md); this keeps them from drifting.
func TestGolden(t *testing.T) {
	var out bytes.Buffer
	if err := run(&out, "../../inputs/platform-config/prd.yaml", "../../inputs/events/deploy-events.jsonl", "../../inputs/events/queries.txt"); err != nil {
		t.Fatal(err)
	}
	const golden = "../../out/promotion/verdicts.txt"
	if *update {
		if err := os.WriteFile(golden, out.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("%v (run: go test ./cmd/soakgate -update)", err)
	}
	if !bytes.Equal(out.Bytes(), want) {
		t.Errorf("output differs from %s; if the change is intended, run: go test ./cmd/soakgate -update\n%s", golden, out.String())
	}
}
