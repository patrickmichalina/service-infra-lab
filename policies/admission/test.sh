#!/bin/sh
# Offline admission tests (kyverno CLI, no cluster). For each environment, everything
# the platform applies there (rendered objects, the namespace baseline, the Argo CD
# projects, tests/good.yaml) must pass, and every object in tests/bad.yaml must be
# refused. tests/cluster/run.sh runs the same policies on a real API server.
# Run from the repository root: policies/admission/test.sh
set -eu
dir=policies/admission
apply() { # env, files...
  env=$1; shift
  # --resource does not walk directories: pass every file
  kyverno apply "$dir/networkpolicy.yaml" "$dir/iam-role.yaml" "$dir/application.yaml" \
    --crd-paths "$dir/tests/crds.yaml" --values-file "$dir/tests/values.yaml" \
    --parameter-resource "$dir/params/$env.yaml" --resource "$(echo "$@" | tr ' ' ',')" 2>&1
}
status=0
for env in stg prd; do
  for out in out/render out/render-fixed; do
    files="$(find "$out/$env" -name '*.yaml' | sort | tr '\n' ' ') policies/network/default-deny.yaml policies/argocd/projects.yaml $dir/tests/good.yaml"
    summary=$(apply $env $files | tail -1)
    case "$summary" in
      "pass: 0,"*) echo "FAIL  $env $out: nothing was checked"; status=1 ;;
      *"fail: 0,"*"error: 0,"*) echo "ok    $env $out + baseline, projects, good.yaml: $summary" ;;
      *) echo "FAIL  $env $out: $summary"; apply $env $files | grep -A2 -E 'failed|error'; status=1 ;;
    esac
  done
done
# One finding per bad object: each matches exactly one policy, and must not pass it.
want=$(grep -c '^kind:' "$dir/tests/bad.yaml")
result=$(apply stg "$dir/tests/bad.yaml" || true) # kyverno exits 1 when anything fails
summary=$(echo "$result" | tail -1)
refused=$(echo "$summary" | sed -n 's/^pass: 0, fail: \([0-9]*\), warn: 0, error: \([0-9]*\), skip: 0.*/\1 \2/p' | awk '{print $1+$2}')
if [ "$refused" = "$want" ]; then
  echo "ok    refused all $want bad objects: $summary"
else
  echo "FAIL  bad objects: want $want refused, got $summary"
  echo "$result" | grep -B1 -A1 -i 'pass\b\|passed' || true
  status=1
fi
exit $status
