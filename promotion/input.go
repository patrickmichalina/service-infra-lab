package promotion

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// LoadPolicy reads the promotion block of the target environment's platform
// config (inputs/platform-config/prd.yaml).
func LoadPolicy(path string) (Policy, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Policy{}, err
	}
	var cfg struct {
		Env       string `yaml:"env"`
		Promotion *struct {
			From        string `yaml:"from"`
			SoakMinutes int    `yaml:"soakMinutes"`
		} `yaml:"promotion"`
	}
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return Policy{}, fmt.Errorf("%s: %w", path, err)
	}
	switch {
	case cfg.Promotion == nil:
		return Policy{}, fmt.Errorf("%s: no promotion block; %s is not a promotion target", path, cfg.Env)
	case cfg.Promotion.From == "":
		return Policy{}, fmt.Errorf("%s: promotion.from is empty", path)
	case cfg.Promotion.SoakMinutes <= 0:
		return Policy{}, fmt.Errorf("%s: promotion.soakMinutes must be positive, got %d", path, cfg.Promotion.SoakMinutes)
	}
	return Policy{From: cfg.Promotion.From, Soak: time.Duration(cfg.Promotion.SoakMinutes) * time.Minute}, nil
}

// ParseQueries reads "now service revision" lines; '#' starts a comment.
func ParseQueries(r io.Reader) ([]Query, error) {
	var qs []Query
	sc := bufio.NewScanner(r)
	line := 0
	for sc.Scan() {
		line++
		text := sc.Text()
		if i := strings.IndexByte(text, '#'); i >= 0 {
			text = text[:i]
		}
		f := strings.Fields(text)
		if len(f) == 0 {
			continue
		}
		if len(f) != 3 {
			return nil, fmt.Errorf("queries line %d: want \"now service revision\", got %q", line, sc.Text())
		}
		now, err := time.Parse(time.RFC3339, f[0])
		if err != nil {
			return nil, fmt.Errorf("queries line %d: now %q is not RFC 3339", line, f[0])
		}
		qs = append(qs, Query{Now: now.UTC(), Service: f[1], Revision: f[2]})
	}
	return qs, sc.Err()
}

// Load reads an event stream and returns a gate over it with every defect
// recorded.
func Load(p Policy, events io.Reader) (*Gate, error) {
	parsed, defects, err := ParseEvents(events)
	if err != nil {
		return nil, err
	}
	ingested, more := Ingest(parsed)
	return NewGate(p, ingested, append(defects, more...)), nil
}
