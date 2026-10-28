#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/../.."
name="platform-kargo-${RANDOM}"
work=$(mktemp -d)
export KUBECONFIG="$work/kubeconfig"
cleanup() { kind delete cluster --name "$name"; rm -rf "$work"; }
trap cleanup EXIT
kind create cluster --name "$name" --image kindest/node:v1.34.0 --kubeconfig "$KUBECONFIG" --wait 90s
kubectl apply -f https://github.com/cert-manager/cert-manager/releases/download/v1.16.1/cert-manager.yaml
kubectl create namespace argo-rollouts
kubectl apply -n argo-rollouts -f https://github.com/argoproj/argo-rollouts/releases/download/v1.8.3/install.yaml
kubectl -n cert-manager rollout status deploy/cert-manager-webhook --timeout=180s
kubectl -n argo-rollouts rollout status deploy/argo-rollouts --timeout=180s
helm install kargo oci://ghcr.io/akuity/kargo-charts/kargo --version 1.8.3 \
  -n kargo --create-namespace --set api.enabled=false \
  --set externalWebhooksServer.enabled=false --set controller.argocd.integrationEnabled=false \
  --wait --timeout 5m
python3 tests/kargo/check.py
