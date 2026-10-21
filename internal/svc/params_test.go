package svc

import (
	"os"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The admission policies compare objects against per-cluster parameters. They
// are the same facts the renderer reads, so the two must never disagree.
func TestAdmissionParamsMatchPlatformConfig(t *testing.T) {
	p := platform(t)
	for _, env := range p.EnvOrder {
		b, err := os.ReadFile("../../policies/admission/params/" + env + ".yaml")
		if err != nil {
			t.Fatal(err)
		}
		var cm struct {
			Data map[string]string `yaml:"data"`
		}
		if err := yaml.Unmarshal(b, &cm); err != nil {
			t.Fatal(err)
		}
		e := p.Envs[env]
		var teams []string
		for name, t2 := range e.Config.Teams {
			if t2.Namespace != name || t2.Project != name {
				t.Errorf("%s: team %s has namespace %s and project %s; admission assumes all three are equal", env, name, t2.Namespace, t2.Project)
			}
			teams = append(teams, name)
		}
		got := strings.Split(cm.Data["teams"], ",")
		slices.Sort(teams)
		slices.Sort(got)
		want := map[string]string{
			"env": env, "awsAccountId": e.Config.AWSAccountID, "vpcCidr": e.Config.VPCCIDR,
			"albSubnets": strings.Join(e.Catalog.ALBSubnets, ","),
		}
		for k, v := range want {
			if cm.Data[k] != v {
				t.Errorf("%s: params %s = %q, platform config says %q", env, k, cm.Data[k], v)
			}
		}
		if !slices.Equal(got, teams) {
			t.Errorf("%s: params teams = %v, platform config has %v", env, got, teams)
		}
	}
}
