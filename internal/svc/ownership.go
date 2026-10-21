package svc

import (
	"fmt"
	"slices"
	"strings"
)

// ---- ownership: a service uses what its team owns, or what the owner shares --------
//
// R2 limits how broad a grant is, not whose resource it is on, so a spec could
// grant itself another team's bucket or every topic on the shared cluster. The
// registry records who owns each shared resource; the owner shares it by
// naming the consumer in its own shard of the registry, so sharing needs the
// owner's consent and not the platform team's.

// checkOwnership is part of R2: a permission may only touch resources the
// service's team owns, resources shared with this service, or identities that
// are the service's own (its consumer groups and database user).
func checkOwnership(s *Spec, p *Platform) []Finding {
	t := team(s, p)
	var fs []Finding
	for i, perm := range s.Permissions {
		for j, r := range perm.Resources {
			_, name, ok := splitRef(r)
			if !ok {
				continue // R2 already reports it
			}
			path := fmt.Sprintf("permissions[%d].resources[%d]", i, j)
			kind, _, _ := strings.Cut(r, ":")
			switch kind {
			case "msk-cluster":
				continue // the shared cluster itself; what it allows is decided per topic and group
			case "msk-group":
				_, group, _ := strings.Cut(name, "/")
				if !ownName(literalHead(group), s.Service) {
					fs = append(fs, errorf("R2", path, fmt.Sprintf("name your own consumer group: msk-group:%s/%s (or %s.<suffix>)", clusterOf(name), s.Service, s.Service),
						"%q is not this service's consumer group; a service reads and commits offsets only for groups named after it", r))
				}
				continue
			case "rds-dbuser":
				inst, user, _ := strings.Cut(name, "/")
				if want := dbUser(s.Service); user != want {
					fs = append(fs, errorf("R2", path, fmt.Sprintf("rds-dbuser:%s/%s", inst, want),
						"%q is not this service's database user; a service connects only as %s, so no service can log in as another (or as an admin)", r, want))
				}
				continue
			}
			fs = append(fs, checkOwned(s, p, t, perm, path, ownedRef(kind, name))...)
		}
	}
	return fs
}

func checkOwned(s *Spec, p *Platform, team string, perm Permission, path, ref string) []Finding {
	o, ok := owner(p, ref)
	if !ok {
		if strings.ContainsAny(ref, "*?") {
			return []Finding{errorf("R2", path, "keep a wildcard inside one name prefix a team owns, e.g. msk-topic:main/<your prefix>*",
				"%q spans resources of more than one owner", ref)}
		}
		return []Finding{errorf("R2", path, fmt.Sprintf("the team that owns it records it once in platform-gitops teams/<owner>/resources.yaml; if that is %s: - {ref: %q, owner: %s}", team, ref, team),
			"no team is recorded as the owner of %q, so nobody can have agreed to this use", ref)}
	}
	if o.Owner == team {
		return nil
	}
	i := slices.IndexFunc(o.Share, func(sh Share) bool { return sh.Service == s.Service })
	if i < 0 {
		return []Finding{errorf("R2", path,
			fmt.Sprintf("ask team %s to share it: in teams/%s/resources.yaml, under %q, add share: [{service: %s, access: read}]. Their leads approve it; the platform team is not involved", o.Owner, o.Owner, o.Ref, s.Service),
			"%q belongs to team %s, which has not shared it with %s", ref, o.Owner, s.Service)}
	}
	if o.Share[i].Access == "write" {
		return nil
	}
	var fs []Finding
	for _, a := range perm.Actions {
		if !readOnly(a) {
			fs = append(fs, errorf("R2", path,
				fmt.Sprintf("use only read actions on it, or ask team %s to share it with access: write", o.Owner),
				"team %s shares %q with %s for reading only, and %s is not a read", o.Owner, ref, s.Service, a))
		}
	}
	return fs
}

// ownedRef is the name ownership is recorded under: the bucket for both S3
// reference types, the reference itself otherwise.
func ownedRef(kind, name string) string {
	if kind == "s3-object" {
		bucket, _, _ := strings.Cut(name, "/")
		return "s3-bucket:" + bucket
	}
	return kind + ":" + name
}

// owner finds the ownership entry covering ref. A wildcard reference is
// covered only by a prefix entry its literal part already falls within.
func owner(p *Platform, ref string) (Ownership, bool) {
	head := literalHead(ref)
	wild := head != ref
	for _, o := range p.Resources {
		switch {
		case !o.Prefix && !wild && o.Ref == ref:
			return o, true
		case o.Prefix && strings.HasPrefix(head, o.Ref):
			return o, true
		}
	}
	return Ownership{}, false
}

// literalHead is s up to its first wildcard.
func literalHead(s string) string {
	if i := strings.IndexAny(s, "*?"); i >= 0 {
		return s[:i]
	}
	return s
}

func ownName(head, service string) bool {
	return head == service || strings.HasPrefix(head, service+".") || strings.HasPrefix(head, service+"-")
}

func clusterOf(name string) string { c, _, _ := strings.Cut(name, "/"); return c }

// dbUser is the database user a service connects as.
func dbUser(service string) string { return strings.ReplaceAll(service, "-", "_") }

// readOnly reports whether every action an action pattern can match only reads.
func readOnly(action string) bool {
	svc, name, _ := strings.Cut(strings.ToLower(action), ":")
	switch svc {
	case "s3":
		return strings.HasPrefix(name, "get") || strings.HasPrefix(name, "list")
	case "kafka-cluster":
		return strings.HasPrefix(name, "describe") || name == "readdata"
	case "kms":
		return name == "decrypt" || name == "describekey"
	}
	return false
}
