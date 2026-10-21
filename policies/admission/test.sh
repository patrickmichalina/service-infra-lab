#!/bin/sh
# Offline admission tests (kyverno CLI, no cluster): every object the renderer
# produced must pass the backstops, and every object in tests/bad.yaml must fail.
# Run from the repository root: policies/admission/test.sh
set -eu
dir=policies/admission
apply() {
  # --resource does not walk directories: pass every file
  resources=$(find "$1" -name '*.yaml' | sort | tr '\n' ',' | sed 's/,$//')
  kyverno apply "$dir/networkpolicy.yaml" "$dir/iam-role.yaml" "$dir/application.yaml" \
    --crd-paths "$dir/tests/crds.yaml" --values-file "$dir/tests/values.yaml" --resource "$resources" 2>&1
}
status=0
# One environment at a time, as each is a separate cluster (names repeat across them).
for out in out/render/stg out/render/prd out/render-fixed/stg out/render-fixed/prd; do
  summary=$(apply "$out" | tail -1)
  case "$summary" in
    "pass: 0,"*) echo "FAIL  rendered $out: nothing was checked"; status=1 ;;
    *"fail: 0,"*"error: 0"*) echo "ok    rendered $out: $summary" ;;
    *) echo "FAIL  rendered $out: $summary"; apply "$out" | grep -A1 failed; status=1 ;;
  esac
done
want=$(grep -c '^kind:' "$dir/tests/bad.yaml")
summary=$(apply "$dir/tests/bad.yaml" | tail -1)
case "$summary" in
  "pass: 0, fail: $want,"*) echo "ok    refused all $want bad objects: $summary" ;;
  *) echo "FAIL  bad objects: want $want refused, got $summary"; status=1 ;;
esac
exit $status
