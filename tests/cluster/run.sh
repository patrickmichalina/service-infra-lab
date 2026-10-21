#!/usr/bin/env bash
# Runs the platform's policies and rendered objects against real Kubernetes: a throwaway
# kind cluster (deleted on exit unless KEEP=1) with the real CRDs of ACK, External
# Secrets and Argo CD, and a NetworkPolicy dataplane. Nothing here talks to AWS.
#
#   tests/cluster/run.sh            everything; the transcript is the evidence
#   KEEP=1 tests/cluster/run.sh     leave the cluster up afterwards
#
# Every check prints "ok" or "FAIL" with what it saw; the script exits non-zero if any
# check failed. It uses its own kubeconfig and never touches ~/.kube/config.
set -euo pipefail
cd "$(dirname "$0")/../.."
here=tests/cluster
work=$here/work
mkdir -p "$work"
export KUBECONFIG=$PWD/$here/.kubeconfig
cluster=takehome
fqdn_cluster=takehome-fqdn
venue=takehome-venue # a stand-in for venue A's FIX gateway, on its catalog range (stg)

ack_iam=1.10.1 ack_eks=1.24.3 ack_ecr=1.10.1
eso=2.13.0 argocd=v3.5.6 cilium=1.20.3
agnhost=registry.k8s.io/e2e-test-images/agnhost:2.53

passed=0 failed=0
ok() { passed=$((passed + 1)); printf 'ok    %s\n' "$*"; }
bad() { failed=$((failed + 1)); printf 'FAIL  %s\n' "$*"; }
step() { printf '\n## %s\n' "$*"; }

teardown() {
  if [ "${KEEP:-}" = 1 ]; then
    echo "KEEP=1: cluster $cluster left running (kind delete cluster --name $cluster)"
    return
  fi
  docker rm -f "$venue" >/dev/null 2>&1 || true
  kind delete cluster --name "$cluster" --kubeconfig "$KUBECONFIG" >/dev/null 2>&1 || true
  docker network rm "$venue" >/dev/null 2>&1 || true
  kind delete cluster --name "$fqdn_cluster" --kubeconfig "$work/fqdn.kubeconfig" >/dev/null 2>&1 || true
  rm -f "$KUBECONFIG" "$work/fqdn.kubeconfig"
  echo "cluster deleted"
}
trap teardown EXIT

# ---------------------------------------------------------------------------------------
step "cluster"
kind get clusters 2>/dev/null | grep -qx "$cluster" || kind create cluster --config "$here/kind.yaml" --kubeconfig "$KUBECONFIG" >/dev/null 2>&1
# A kubectl matching the server: the system one may be several minor versions behind.
server=$(docker exec "$cluster-control-plane" kubelet --version | awk '{print $2}')
os=$(uname -s | tr '[:upper:]' '[:lower:]') arch=$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')
[ -x "$work/kubectl-$server" ] || { curl -fsSL "https://dl.k8s.io/release/$server/bin/$os/$arch/kubectl" -o "$work/kubectl-$server" && chmod +x "$work/kubectl-$server"; }
ln -sf "kubectl-$server" "$work/kubectl"
export PATH=$PWD/$work:$PATH
kubectl wait --for=condition=Ready node --all --timeout=180s >/dev/null
echo "kind $(kind version | awk '{print $2}'), Kubernetes $(kubectl version -o json | sed -n 's/.*"gitVersion": "\(v[^"]*\)".*/\1/p' | tail -1), CNI kindnet ($(kubectl -n kube-system get ds kindnet -o jsonpath='{.spec.template.spec.containers[0].image}'))"

for ns in argocd ack-services external-secrets monitoring charts kube-system; do
  kubectl get namespace "$ns" >/dev/null 2>&1 || kubectl create namespace "$ns" >/dev/null
  kubectl label namespace "$ns" platform.example.org/platform-namespace=true --overwrite >/dev/null
done
for ns in ingestion state normalization distribution sandbox; do
  kubectl get namespace "$ns" >/dev/null 2>&1 || kubectl create namespace "$ns" >/dev/null
done

step "real CRDs: ACK iam $ack_iam, eks $ack_eks, ecr $ack_ecr; External Secrets $eso; Argo CD $argocd"
for c in iam:$ack_iam eks:$ack_eks ecr:$ack_ecr; do
  n=${c%%:*} v=${c##*:}
  [ -d "$work/$n-chart" ] || { helm pull "oci://public.ecr.aws/aws-controllers-k8s/$n-chart" --version "$v" -d "$work" >/dev/null 2>&1 && tar -xzf "$work/$n-chart-$v.tgz" -C "$work"; }
  kubectl apply --server-side -f "$work/$n-chart/crds/" >/dev/null
done
helm repo add external-secrets https://charts.external-secrets.io >/dev/null 2>&1 || true
helm upgrade --install external-secrets external-secrets/external-secrets --version "$eso" -n external-secrets --wait --timeout 5m >/dev/null
kubectl apply -n argocd --server-side --force-conflicts -f "https://raw.githubusercontent.com/argoproj/argo-cd/$argocd/manifests/install.yaml" >/dev/null
kubectl wait --for=condition=Established crd --all --timeout=120s >/dev/null
kubectl -n argocd rollout status deploy/argocd-repo-server --timeout=5m >/dev/null
kubectl -n argocd rollout status statefulset/argocd-application-controller --timeout=5m >/dev/null
echo "installed"

step "admission policies (stg parameters)"
kubectl apply -f policies/admission/params/stg.yaml >/dev/null
kubectl apply -f policies/admission/networkpolicy.yaml -f policies/admission/iam-role.yaml -f policies/admission/application.yaml >/dev/null
# A policy takes effect once the API server has compiled it; wait until a known-bad object is refused.
for _ in $(seq 60); do
  kubectl apply --dry-run=server -f - >/dev/null 2>&1 <<'EOF' || break
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata: {name: probe, namespace: sandbox}
spec: {podSelector: {}, ingress: [{}]}
EOF
  sleep 2
done
echo "active: $(kubectl get validatingadmissionpolicies -o name | tr '\n' ' ')"

# ---------------------------------------------------------------------------------------
step "everything the platform applies in stg is admitted, and the CRDs know every field"
# --validate=strict: an unknown field (a wrong ACK or External Secrets field name) fails.
apply_strict() { kubectl apply --server-side --field-manager=platform --validate=strict "$@" 2>&1; }
objects=$(find out/render-fixed/stg -name '*.yaml' | sort)
for f in policies/network/default-deny.yaml $objects; do
  if out=$(apply_strict -f "$f"); then ok "admitted $f"; else bad "refused $f: $out"; fi
done
# The services-infra project's sourceRepos and the team projects' chart repository point at
# ECR; here the charts come from an in-cluster repository.
sed 's|111111111111.dkr.ecr.us-east-1.amazonaws.com/charts|http://charts.charts.svc.cluster.local|' policies/argocd/projects.yaml > "$work/projects.yaml"
if out=$(apply_strict -f "$work/projects.yaml"); then ok "admitted policies/argocd/projects.yaml (5 AppProjects)"; else bad "refused projects: $out"; fi
for f in $(find out/render/stg -name '*.yaml' | sort); do
  if out=$(kubectl apply --server-side --field-manager=platform --force-conflicts --dry-run=server --validate=strict -f "$f" 2>&1); then ok "admitted (dry run) as-requested $f"; else bad "refused $f: $out"; fi
done
if out=$(kubectl apply --server-side --dry-run=server --validate=strict -f policies/admission/tests/good.yaml 2>&1); then ok "admitted policies/admission/tests/good.yaml"; else bad "refused good.yaml: $out"; fi

step "every hostile object is refused, by the policy meant to refuse it"
python3 -I - policies/admission/tests/bad.yaml "$work/bad" <<'EOF'
import os, re, sys
src, out = sys.argv[1], sys.argv[2]
os.makedirs(out, exist_ok=True)
for i, doc in enumerate(d for d in open(src).read().split("\n---\n") if "kind:" in d):
    kind = re.search(r"^kind: (\w+)", doc, re.M).group(1)
    name = re.search(r"name: ([\w.-]+)", doc[doc.index("metadata"):]).group(1)
    comments = re.findall(r"^# (.*)$", doc.split("apiVersion")[0], re.M)
    why = comments[-1] if comments else ""
    open(f"{out}/{i:02d}-{kind}-{name}.yaml", "w").write(f"# why: {why}\n{doc}\n")
EOF
for f in "$work"/bad/*.yaml; do
  why=$(sed -n 's/^# why: //p' "$f")
  kind=$(basename "$f" .yaml | cut -d- -f2)
  case $kind in
    NetworkPolicy) want=service-networkpolicy ;;
    Role) want=service-iam-role ;;
    PodIdentityAssociation) want=service-pod-identity ;;
    Application) want=service-application ;;
    AppProject) want=team-appproject ;;
  esac
  if out=$(kubectl apply --server-side --field-manager=platform --force-conflicts --dry-run=server -f "$f" 2>&1); then
    bad "ADMITTED $(basename "$f") ($why)"
  elif echo "$out" | grep -q "ValidatingAdmissionPolicy '$want'"; then
    ok "refused $(basename "$f" .yaml) [$why]: $(echo "$out" | sed -n "s/.*denied request: //p" | head -1 | cut -c1-220)"
  else
    bad "refused $(basename "$f"), but not by $want: $out"
  fi
done

step "prd parameters: prd objects are admitted, stg objects are not"
kubectl apply -f policies/admission/params/prd.yaml >/dev/null
sleep 2
for f in $(find out/render-fixed/prd out/render/prd -name '*.yaml' | sort); do
  if out=$(kubectl apply --server-side --field-manager=platform --force-conflicts --dry-run=server --validate=strict -f "$f" 2>&1); then ok "admitted (dry run, prd) $f"; else bad "refused (prd) $f: $out"; fi
done
for f in out/render-fixed/stg/odds-ingest/iam-role.yaml out/render-fixed/stg/odds-ingest/networkpolicy.yaml out/render-fixed/stg/odds-ingest/pod-identity-association.yaml; do
  if out=$(kubectl apply --server-side --field-manager=platform --force-conflicts --dry-run=server -f "$f" 2>&1); then bad "a stg object passed prd admission: $f"; else ok "stg object refused under prd parameters: $f: $(echo "$out" | sed -n 's/.*denied request: //p' | cut -c1-160)"; fi
done
kubectl apply -f policies/admission/params/stg.yaml >/dev/null
sleep 2

# ---------------------------------------------------------------------------------------
step "External Secrets: the rendered ExternalSecrets sync (fake provider standing in for SSM)"
for es in $(find out/render-fixed/stg -name externalsecret.yaml | sort); do
  ns=$(sed -n 's/^  namespace: //p' "$es" | head -1)
  keys=$(sed -n 's/^ *key: \(\/stg\/.*\)/\1/p' "$es")
  {
    printf 'apiVersion: external-secrets.io/v1\nkind: SecretStore\nmetadata: {name: ssm, namespace: %s}\nspec:\n  provider:\n    fake:\n      data:\n' "$ns"
    for k in $keys; do printf '        - {key: "%s", value: "value-of-%s"}\n' "$k" "$k"; done
  } | kubectl apply -f - >/dev/null
done
for es in $(find out/render-fixed/stg -name externalsecret.yaml | sort); do
  ns=$(sed -n 's/^  namespace: //p' "$es" | head -1); name=$(sed -n 's/^  name: //p' "$es" | head -1)
  kubectl -n "$ns" annotate externalsecret "$name" force-sync="$(date +%s)" --overwrite >/dev/null
  if kubectl -n "$ns" wait externalsecret "$name" --for=condition=Ready --timeout=90s >/dev/null 2>&1; then
    got=$(kubectl -n "$ns" get secret "$name" -o go-template='{{range $k, $v := .data}}{{$k}}={{$v | base64decode}} {{end}}')
    ok "ExternalSecret $ns/$name is Ready; Secret $name has: $got"
  else
    bad "ExternalSecret $ns/$name not Ready: $(kubectl -n "$ns" get externalsecret "$name" -o jsonpath='{.status.conditions}')"
  fi
done

# ---------------------------------------------------------------------------------------
step "Argo CD: team projects deploy a plain chart and refuse the platform's kinds"
rm -rf "$work/repo"; mkdir -p "$work/repo"
for c in "$here"/charts/*/; do helm package "$c" -d "$work/repo" >/dev/null; done
helm repo index "$work/repo" --url http://charts.charts.svc.cluster.local >/dev/null
kubectl -n charts create configmap charts --from-file="$work/repo" --dry-run=client -o yaml | kubectl apply --server-side --force-conflicts -f - >/dev/null
kubectl apply -f - >/dev/null <<'EOF'
apiVersion: apps/v1
kind: Deployment
metadata: {name: charts, namespace: charts}
spec:
  selector: {matchLabels: {app: charts}}
  template:
    metadata: {labels: {app: charts}}
    spec:
      containers:
        - {name: nginx, image: nginx:1.29-alpine, ports: [{containerPort: 80}], volumeMounts: [{name: c, mountPath: /usr/share/nginx/html}]}
      volumes: [{name: c, configMap: {name: charts}}]
---
apiVersion: v1
kind: Service
metadata: {name: charts, namespace: charts}
spec: {selector: {app: charts}, ports: [{port: 80}]}
EOF
kubectl -n charts rollout status deploy/charts --timeout=3m >/dev/null
for c in good with-networkpolicy with-clusterrole with-serviceaccount; do
  kubectl apply -f - >/dev/null <<EOF
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata: {name: ingestion-$c, namespace: argocd}
spec:
  project: ingestion
  destination: {server: https://kubernetes.default.svc, namespace: ingestion}
  source: {repoURL: http://charts.charts.svc.cluster.local, chart: $c, targetRevision: 1.0.0}
  syncPolicy: {automated: {prune: true}, retry: {limit: 1}}
EOF
done
for c in good with-networkpolicy with-clusterrole with-serviceaccount; do
  for _ in $(seq 60); do
    phase=$(kubectl -n argocd get application "ingestion-$c" -o jsonpath='{.status.operationState.phase}' 2>/dev/null)
    [ -n "$phase" ] && [ "$phase" != Running ] && break
    sleep 3
  done
  sync=$(kubectl -n argocd get application "ingestion-$c" -o jsonpath='{.status.sync.status}')
  msg=$(kubectl -n argocd get application "ingestion-$c" -o jsonpath='{.status.operationState.message}')
  if [ "$c" = good ]; then
    if [ "$phase" = Succeeded ] && kubectl -n ingestion get configmap good >/dev/null 2>&1; then ok "ingestion-good synced ($sync): configmap ingestion/good exists"; else bad "ingestion-good: phase=$phase sync=$sync $msg"; fi
  elif echo "$msg" | grep -q "not permitted in project"; then
    ok "ingestion-$c refused by Argo CD: $(echo "$msg" | cut -c1-200)"
  else
    bad "ingestion-$c: expected a project refusal, got phase=$phase sync=$sync: $msg"
  fi
done
kubectl -n ingestion get networkpolicy allow-all >/dev/null 2>&1 && bad "the chart's allow-all NetworkPolicy exists" || ok "no allow-all NetworkPolicy reached ingestion"
kubectl get clusterrole chart-clusterrole >/dev/null 2>&1 && bad "the chart's ClusterRole exists" || ok "no chart ClusterRole reached the cluster"

# ---------------------------------------------------------------------------------------
step "network: the rendered stg policies, enforced (pods have VPC-range addresses here)"
pod() { # namespace name label [ports...]
  local ns=$1 name=$2 label=$3; shift 3
  local env="" sa="" p
  for p in "$@"; do env="$env{name: SERVE_PORT_$p, value: \"$name:$p\"}, "; done
  kubectl -n "$ns" get serviceaccount "$name" >/dev/null 2>&1 && sa="serviceAccountName: $name"
  kubectl apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Pod
metadata: {name: $name, namespace: $ns, labels: {app.kubernetes.io/name: $label}}
spec:
  $sa
  containers: [{name: c, image: $agnhost, args: [porter], env: [${env%, }]}]
EOF
}
pod ingestion odds-ingest odds-ingest 8080 9090
pod state settlement-writer settlement-writer 8080 9090
pod distribution trader-ui trader-ui 3000
pod normalization rfq-router rfq-router 8080
pod ingestion stray stray 8080
pod state victim victim 9098
pod sandbox sandbox-victim sandbox-victim 9098
pod sandbox sandbox-client sandbox-client 8081
pod monitoring prometheus prometheus 8081
for ns in ingestion state distribution normalization sandbox monitoring; do kubectl -n "$ns" wait --for=condition=Ready pod --all --timeout=3m >/dev/null; done
ip() { kubectl -n "$1" get pod "$2" -o jsonpath='{.status.podIP}'; }
can() { kubectl -n "$1" exec "$2" -- /agnhost connect "$3" --timeout=3s >/dev/null 2>&1; }
check() { # expect from-ns from-pod target description
  local expect=$1 ns=$2 from=$3 target=$4; shift 4
  if can "$ns" "$from" "$target"; then got=allowed; else got=denied; fi
  if [ "$got" = "$expect" ]; then ok "$expect: $ns/$from -> $target ($*)"; else bad "$got, want $expect: $ns/$from -> $target ($*)"; fi
}
echo "pod addresses: odds-ingest $(ip ingestion odds-ingest), victim $(ip state victim), sandbox-victim $(ip sandbox sandbox-victim) (stg VPC 10.20.0.0/16)"
check allowed monitoring prometheus "$(ip ingestion odds-ingest):8080" "declared: Prometheus scrape"
check denied sandbox sandbox-client "$(ip ingestion odds-ingest):8080" "undeclared namespace"
check denied monitoring prometheus "$(ip ingestion odds-ingest):9090" "undeclared port"
check allowed distribution trader-ui "$(ip state settlement-writer):9090" "declared on both sides: trader-ui egress, settlement-writer ingress"
check denied ingestion odds-ingest "$(ip state settlement-writer):9090" "odds-ingest declares no egress to state"
check allowed ingestion odds-ingest "kube-dns.kube-system.svc.cluster.local:53" "platform DNS baseline (the name resolves, then TCP 53)"
check allowed ingestion odds-ingest "1.1.1.1:443" "internet egress on 443"
check denied ingestion odds-ingest "1.1.1.1:80" "internet egress on another port"
check denied monitoring prometheus "$(ip ingestion stray):8080" "a pod no service policy selects: platform default-deny"
check denied ingestion stray "1.1.1.1:443" "default-deny egress for the stray pod"
check denied monitoring prometheus "$(ip distribution trader-ui):3000" "trader-ui admits {albSubnets} only; no pod is in those subnets"
echo "A13: what {vpcCidr} means when pods have VPC addresses"
check allowed ingestion odds-ingest "$(ip sandbox sandbox-victim):9098" "odds-ingest egress {vpcCidr}:9098 reaches a pod in a namespace without a baseline"
check denied ingestion odds-ingest "$(ip state victim):9098" "the same egress, to a pod in a team namespace: the default-deny baseline stops it"
kubectl -n distribution patch networkpolicy trader-ui --type=json -p '[{"op":"replace","path":"/spec/ingress/0/from","value":[{"ipBlock":{"cidr":"10.20.0.0/16"}}]}]' >/dev/null
check allowed monitoring prometheus "$(ip distribution trader-ui):3000" "trader-ui ingress from {vpcCidr} instead: every pod in the cluster gets in"
kubectl apply --server-side --field-manager=platform --force-conflicts -f out/render-fixed/stg/trader-ui/networkpolicy.yaml >/dev/null
check denied monitoring prometheus "$(ip distribution trader-ui):3000" "restored to {albSubnets}"

echo "counterparty: venue A's FIX gateway, simulated on its catalog range (a Docker network the node is attached to)"
venue_cidr=$(sed -n '/venue-a-fix:/,/cidrs:/s/.*cidrs: \[\(.*\)\]/\1/p' platform/catalog/stg.yaml)
venue_ip=198.51.100.20
docker network inspect "$venue" >/dev/null 2>&1 || docker network create --subnet "$venue_cidr" "$venue" >/dev/null
docker network connect "$venue" "$cluster-control-plane" >/dev/null 2>&1 || true
docker rm -f "$venue" >/dev/null 2>&1 || true
docker pull -q "$agnhost" >/dev/null
docker run -d --name "$venue" --network "$venue" --ip "$venue_ip" -e SERVE_PORT_9880=venue-a-fix -e SERVE_PORT_9881=other "$agnhost" porter >/dev/null
for _ in $(seq 30); do can sandbox sandbox-client "$venue_ip:9880" && break; sleep 1; done
check allowed sandbox sandbox-client "$venue_ip:9881" "control: a pod with no policy reaches the venue on any port, so a denial below is the policy"
check allowed normalization rfq-router "$venue_ip:9880" "rfq-router's counterparty: venue-a-fix rule, rendered as $venue_cidr:9880"
check denied normalization rfq-router "$venue_ip:9881" "the venue's address on a port the catalog does not list"
check denied ingestion odds-ingest "$venue_ip:9880" "a team the catalog does not clear for venue-a-fix"

# ---------------------------------------------------------------------------------------
step "alternative design: R8 at admission (an update that removes access must say so)"
kubectl apply -f alternative/admission/acknowledge-removal.yaml >/dev/null
sleep 3
for _ in $(seq 30); do kubectl get validatingadmissionpolicy acknowledge-removal -o jsonpath='{.status.typeChecking}' >/dev/null 2>&1 && break; sleep 1; done
sleep 3
np_drop() { kubectl -n state get networkpolicy settlement-writer -o json | python3 -I -c 'import json,sys; o=json.load(sys.stdin); o["spec"]["egress"]=o["spec"]["egress"][:-1]; print(json.dumps(o))'; }
if out=$(np_drop | kubectl apply --dry-run=server -f - 2>&1); then bad "dropping an egress rule was admitted without an acknowledgement"; else ok "dropping an egress rule is refused: $(echo "$out" | sed -n 's/.*denied request: //p' | cut -c1-200)"; fi
if np_drop | kubectl annotate --local -f - platform.example.org/acknowledge-removal="moved to the settlement-events-v2 endpoint" -o json | kubectl apply --dry-run=server -f - >/dev/null 2>&1; then ok "the same update with an acknowledgement is admitted"; else bad "acknowledged removal refused"; fi
role_drop() { kubectl -n ack-services get role.iam.services.k8s.aws svc-state-settlement-writer -o json | python3 -I -c '
import json,sys; o=json.load(sys.stdin); d=json.loads(o["spec"]["inlinePolicies"]["service"])
d["Statement"]=[s for s in d["Statement"] if "kafka-cluster:WriteData" not in s["Action"]]
o["spec"]["inlinePolicies"]["service"]=json.dumps(d, indent=2); print(json.dumps(o))'; }
if out=$(role_drop | kubectl apply --dry-run=server -f - 2>&1); then bad "dropping kafka-cluster:WriteData was admitted"; else ok "dropping kafka-cluster:WriteData from a role is refused: $(echo "$out" | sed -n 's/.*denied request: //p' | cut -c1-200)"; fi
if role_drop | kubectl annotate --local -f - platform.example.org/acknowledge-removal="writes moved to settlement-publisher" -o json | kubectl apply --dry-run=server -f - >/dev/null 2>&1; then ok "the same role update with an acknowledgement is admitted"; else bad "acknowledged role change refused"; fi
kubectl delete -f alternative/admission/acknowledge-removal.yaml >/dev/null

# ---------------------------------------------------------------------------------------
step "alternative design: FQDN egress (a second cluster with Cilium $cilium)"
cat > "$work/fqdn-kind.yaml" <<EOF
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
name: $fqdn_cluster
networking: {disableDefaultCNI: true}
EOF
kind get clusters 2>/dev/null | grep -qx "$fqdn_cluster" || kind create cluster --config "$work/fqdn-kind.yaml" --kubeconfig "$work/fqdn.kubeconfig" >/dev/null 2>&1
helm repo add cilium https://helm.cilium.io >/dev/null 2>&1 || true
KUBECONFIG=$work/fqdn.kubeconfig helm upgrade --install cilium cilium/cilium --version "$cilium" -n kube-system \
  --set image.pullPolicy=IfNotPresent --set ipam.mode=kubernetes --set operator.replicas=1 --wait --timeout 8m >/dev/null
KUBECONFIG=$work/fqdn.kubeconfig kubectl wait --for=condition=Ready node --all --timeout=180s >/dev/null
KUBECONFIG=$work/fqdn.kubeconfig kubectl apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Pod
metadata: {name: odds-ingest, namespace: default, labels: {app.kubernetes.io/name: odds-ingest}}
spec: {containers: [{name: c, image: $agnhost, args: [pause]}]}
EOF
KUBECONFIG=$work/fqdn.kubeconfig kubectl wait --for=condition=Ready pod/odds-ingest --timeout=3m >/dev/null
KUBECONFIG=$work/fqdn.kubeconfig kubectl wait --for=condition=Established crd/ciliumnetworkpolicies.cilium.io --timeout=120s >/dev/null
KUBECONFIG=$work/fqdn.kubeconfig kubectl apply -f alternative/network/fqdn-egress.yaml >/dev/null
sleep 5
fq() { KUBECONFIG=$work/fqdn.kubeconfig kubectl exec odds-ingest -- /agnhost connect "$1" --timeout=5s >/dev/null 2>&1; }
fcheck() { if fq "$2"; then got=allowed; else got=denied; fi; if [ "$got" = "$1" ]; then ok "$1: odds-ingest -> $2 ($3)"; else bad "$got, want $1: odds-ingest -> $2 ($3)"; fi; }
fcheck allowed example.com:443 "the host the rule names"
fcheck denied example.org:443 "another host on the same port"
fcheck denied 1.1.1.1:443 "an address with no name: 0.0.0.0/0:443 would have allowed it"
kind delete cluster --name "$fqdn_cluster" --kubeconfig "$work/fqdn.kubeconfig" >/dev/null 2>&1

# ---------------------------------------------------------------------------------------
step "result"
echo "$passed passed, $failed failed"
[ "$failed" = 0 ]
