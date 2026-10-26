package promotion

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadPolicyReadsTheTargetConfig(t *testing.T) {
	p, err := LoadPolicy("../inputs/platform-config/prd.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if p.From != "stg" || p.Soak != 120*time.Minute {
		t.Fatalf("policy = %+v", p)
	}
}

func TestLoadPolicyRefusesAConfigWithoutPromotion(t *testing.T) {
	if _, err := LoadPolicy("../inputs/platform-config/stg.yaml"); err == nil || !strings.Contains(err.Error(), "not a promotion target") {
		t.Fatalf("err = %v", err)
	}
	bad := filepath.Join(t.TempDir(), "x.yaml")
	os.WriteFile(bad, []byte("env: prd\npromotion: {from: stg, soakMinutes: 0}\n"), 0o600)
	if _, err := LoadPolicy(bad); err == nil || !strings.Contains(err.Error(), "must be positive") {
		t.Fatalf("err = %v", err)
	}
}

func TestParseQueries(t *testing.T) {
	qs, err := ParseQueries(strings.NewReader("# now service revision\n\n2026-10-08T12:00:00Z  a  r1\n"))
	if err != nil || len(qs) != 1 || qs[0].Service != "a" || qs[0].Revision != "r1" {
		t.Fatalf("qs = %+v, err = %v", qs, err)
	}
	for _, bad := range []string{"2026-10-08T12:00:00Z a", "yesterday a r1"} {
		if _, err := ParseQueries(strings.NewReader(bad)); err == nil {
			t.Errorf("%q: want error", bad)
		}
	}
}
