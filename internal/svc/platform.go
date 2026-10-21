package svc

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// EnvConfig is inputs/platform-config/<env>.yaml.
type EnvConfig struct {
	Env                    string `yaml:"env"`
	AWSAccountID           string `yaml:"awsAccountId"`
	ClusterName            string `yaml:"clusterName"`
	VPCCIDR                string `yaml:"vpcCidr"`
	SSMPrefix              string `yaml:"ssmPrefix"`
	ImageRegistry          string `yaml:"imageRegistry"`
	ChartRegistry          string `yaml:"chartRegistry"`
	PermissionsBoundaryARN string `yaml:"permissionsBoundaryArn"`
	ArgoCD                 struct {
		AutoSync        bool     `yaml:"autoSync"`
		AllowedProjects []string `yaml:"allowedProjects"`
	} `yaml:"argocd"`
	Teams map[string]struct {
		Namespace string `yaml:"namespace"`
		Project   string `yaml:"project"`
	} `yaml:"teams"`
	Promotion struct {
		From string `yaml:"from"`
	} `yaml:"promotion"`
}

// Catalog is platform/catalog/<env>.yaml: the named resources a spec may
// reference, with the ids only the platform knows.
type Catalog struct {
	Region    string `yaml:"region"`
	Endpoints struct {
		S3 string `yaml:"s3"` // "gateway" or "interface"
	} `yaml:"endpoints"`
	MSK map[string]struct {
		Name string `yaml:"name"`
		UUID string `yaml:"uuid"`
	} `yaml:"msk"`
	RDS map[string]struct {
		ResourceID string `yaml:"resourceId"`
	} `yaml:"rds"`
	KMS map[string]struct {
		KeyID string `yaml:"keyId"`
	} `yaml:"kms"`
}

// Registration is one entry of platform/registry.yaml.
type Registration struct {
	Repo  string `yaml:"repo"`
	Team  string `yaml:"team"`
	Chart string `yaml:"chart"`
	Image string `yaml:"image"`
}

// Lookup returns the service registered for repo.
func (p *Platform) Lookup(repo string) (string, Registration, bool) {
	for _, name := range sortedKeys(p.Registry) {
		if r := p.Registry[name]; r.Repo == repo {
			return name, r, true
		}
	}
	return "", Registration{}, false
}

// Usage is one permission observed in use by the running pod.
type Usage struct {
	Action   string `yaml:"action"`
	Resource string `yaml:"resource"`
}

// Observed is platform/observed/<env>/<service>.yaml.
type Observed struct {
	Identity string  `yaml:"identity"`
	Window   string  `yaml:"window"`
	Used     []Usage `yaml:"used"`
}

// Pointers are the Kargo-owned promoted versions of a service in one env.
type Pointers struct {
	ChartVersion string `yaml:"chartVersion"`
	ImageTag     string `yaml:"imageTag"`
}

// Env is everything the platform knows about one environment.
type Env struct {
	Config   EnvConfig
	Catalog  Catalog
	Observed map[string]*Observed // by service; nil entry: no usage data
	Pointers map[string]*Pointers // by service; nil entry: never promoted
}

// Platform is the trusted side: everything not from a service repository.
type Platform struct {
	Envs     map[string]*Env
	EnvOrder []string // stg before prd
	Registry map[string]Registration
}

// LoadPlatform reads platform config from the inputs directory and the
// platform-owned files from platformDir.
func LoadPlatform(inputsDir, platformDir string, envs ...string) (*Platform, error) {
	p := &Platform{Envs: map[string]*Env{}, EnvOrder: envs}
	var reg struct {
		Services map[string]Registration `yaml:"services"`
	}
	if err := readYAML(filepath.Join(platformDir, "registry.yaml"), &reg); err != nil {
		return nil, err
	}
	p.Registry = reg.Services
	for _, name := range envs {
		e := &Env{Observed: map[string]*Observed{}, Pointers: map[string]*Pointers{}}
		if err := readYAML(filepath.Join(inputsDir, "platform-config", name+".yaml"), &e.Config); err != nil {
			return nil, err
		}
		if err := readYAML(filepath.Join(platformDir, "catalog", name+".yaml"), &e.Catalog); err != nil {
			return nil, err
		}
		for svc := range p.Registry {
			var o Observed
			if ok, err := readOptional(filepath.Join(platformDir, "observed", name, svc+".yaml"), &o); err != nil {
				return nil, err
			} else if ok {
				e.Observed[svc] = &o
			}
			var ptr Pointers
			if ok, err := readOptional(filepath.Join(platformDir, "pointers", name, svc+".yaml"), &ptr); err != nil {
				return nil, err
			} else if ok {
				e.Pointers[svc] = &ptr
			}
		}
		p.Envs[name] = e
	}
	return p, nil
}

func readYAML(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := yaml.Unmarshal(b, v); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

func readOptional(path string, v any) (bool, error) {
	err := readYAML(path, v)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}
