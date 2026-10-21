package svc

import (
	"fmt"
	"regexp"
	"strings"
)

// A resource reference is how a spec names an AWS resource without an ARN:
// <type>:<name>. The platform turns it into an ARN with ids from the catalog.
type refType struct {
	service string // the IAM service prefix whose actions apply to it
	form    string // shown to the engineer
	arn     func(name string, env *Env) (string, error)
}

var refTypes = map[string]refType{
	"s3-bucket": {"s3", "s3-bucket:<bucket>", func(n string, _ *Env) (string, error) {
		return "arn:aws:s3:::" + n, nil
	}},
	"s3-object": {"s3", "s3-object:<bucket>/<key or key-prefix*>", func(n string, _ *Env) (string, error) {
		if !strings.Contains(n, "/") {
			return "", fmt.Errorf("an s3-object reference needs a key: s3-object:%s/*", n)
		}
		return "arn:aws:s3:::" + n, nil
	}},
	"msk-cluster": {"kafka-cluster", "msk-cluster:<cluster>", func(n string, e *Env) (string, error) {
		c, ok := e.Catalog.MSK[n]
		if !ok {
			return "", unknownName("MSK cluster", n, sortedKeys(e.Catalog.MSK))
		}
		return fmt.Sprintf("arn:aws:kafka:%s:%s:cluster/%s/%s", e.Catalog.Region, e.Config.AWSAccountID, c.Name, c.UUID), nil
	}},
	"msk-topic": {"kafka-cluster", "msk-topic:<cluster>/<topic>", mskChild("topic")},
	"msk-group": {"kafka-cluster", "msk-group:<cluster>/<consumer group>", mskChild("group")},
	"rds-dbuser": {"rds-db", "rds-dbuser:<instance>/<db user>", func(n string, e *Env) (string, error) {
		inst, user, ok := strings.Cut(n, "/")
		if !ok || user == "" {
			return "", fmt.Errorf("an rds-dbuser reference names instance and user: rds-dbuser:%s/<user>", inst)
		}
		db, found := e.Catalog.RDS[inst]
		if !found {
			return "", unknownName("RDS instance", inst, sortedKeys(e.Catalog.RDS))
		}
		return fmt.Sprintf("arn:aws:rds-db:%s:%s:dbuser:%s/%s", e.Catalog.Region, e.Config.AWSAccountID, db.ResourceID, user), nil
	}},
	"kms-key": {"kms", "kms-key:<key>", func(n string, e *Env) (string, error) {
		k, ok := e.Catalog.KMS[n]
		if !ok {
			return "", unknownName("KMS key", n, sortedKeys(e.Catalog.KMS))
		}
		return fmt.Sprintf("arn:aws:kms:%s:%s:key/%s", e.Catalog.Region, e.Config.AWSAccountID, k.KeyID), nil
	}},
}

func mskChild(kind string) func(string, *Env) (string, error) {
	return func(n string, e *Env) (string, error) {
		cluster, child, ok := strings.Cut(n, "/")
		if !ok || child == "" {
			return "", fmt.Errorf("an msk-%s reference names cluster and %s: msk-%s:%s/<name>", kind, kind, kind, cluster)
		}
		c, found := e.Catalog.MSK[cluster]
		if !found {
			return "", unknownName("MSK cluster", cluster, sortedKeys(e.Catalog.MSK))
		}
		return fmt.Sprintf("arn:aws:kafka:%s:%s:%s/%s/%s/%s", e.Catalog.Region, e.Config.AWSAccountID, kind, c.Name, c.UUID, child), nil
	}
}

func unknownName(what, name string, known []string) error {
	return fmt.Errorf("no %s named %q in the platform catalog (known: %s); if it exists, the platform team adds it to platform/catalog once", what, name, strings.Join(known, ", "))
}

func refTypeNames() string {
	var forms []string
	for _, k := range sortedKeys(refTypes) {
		forms = append(forms, refTypes[k].form)
	}
	return strings.Join(forms, ", ")
}

// splitRef splits "type:name". ok is false for anything that is not a known type.
func splitRef(ref string) (refType, string, bool) {
	t, name, found := strings.Cut(ref, ":")
	rt, known := refTypes[t]
	return rt, name, found && known
}

// expandEnv substitutes the {env} token.
func expandEnv(s, env string) string { return strings.ReplaceAll(s, "{env}", env) }

// globMatch matches IAM-style patterns: * is any run of characters, "/"
// included, and ? is any one character.
func globMatch(pattern, s string) bool {
	re := "^" + strings.NewReplacer(`\*`, ".*", `\?`, ".").Replace(regexp.QuoteMeta(pattern)) + "$"
	return regexp.MustCompile(re).MatchString(s)
}
