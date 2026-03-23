#!/usr/bin/env bash
# register-spire-entries.sh
# Registers SPIRE workload entries for svid-exchange, forge-api, and forge-worker.
# Run this after SPIRE server and agent are ready and all services are deployed.
#
# Usage: bash scripts/register-spire-entries.sh

set -euo pipefail

SPIRE_SERVER="statefulset/spire-server"
SPIRE_NS="spire"
TRUST_DOMAIN="${TRUST_DOMAIN:-cluster.local}"
SOCKET="-socketPath /run/spire/sockets/api.sock"

echo "Registering SPIRE entries (trust domain: ${TRUST_DOMAIN})"

# Derive parent ID from the live agent (k8s_psat SPIFFE ID includes the node UID)
PARENT_ID=$(kubectl exec -n "${SPIRE_NS}" "${SPIRE_SERVER}" -- \
  /opt/spire/bin/spire-server agent list ${SOCKET} 2>/dev/null \
  | grep "SPIFFE ID" | awk '{print $3}' | head -1)

if [[ -z "${PARENT_ID}" ]]; then
  echo "ERROR: No attested SPIRE agent found. Is the agent running?" >&2
  exit 1
fi

echo "Using agent parent ID: ${PARENT_ID}"

kubectl exec -n "${SPIRE_NS}" "${SPIRE_SERVER}" -- \
  /opt/spire/bin/spire-server entry create \
  ${SOCKET} \
  -spiffeID "spiffe://${TRUST_DOMAIN}/ns/default/sa/svid-exchange" \
  -parentID "${PARENT_ID}" \
  -selector "k8s:sa:svid-exchange" \
  -selector "k8s:ns:default" \
  2>&1 | grep -v "already exists" || true

echo "Registered svid-exchange"

kubectl exec -n "${SPIRE_NS}" "${SPIRE_SERVER}" -- \
  /opt/spire/bin/spire-server entry create \
  ${SOCKET} \
  -spiffeID "spiffe://${TRUST_DOMAIN}/ns/default/sa/forge-api" \
  -parentID "${PARENT_ID}" \
  -selector "k8s:sa:forge-api" \
  -selector "k8s:ns:default" \
  2>&1 | grep -v "already exists" || true

echo "Registered forge-api"

kubectl exec -n "${SPIRE_NS}" "${SPIRE_SERVER}" -- \
  /opt/spire/bin/spire-server entry create \
  ${SOCKET} \
  -spiffeID "spiffe://${TRUST_DOMAIN}/ns/default/sa/forge-worker" \
  -parentID "${PARENT_ID}" \
  -selector "k8s:sa:forge-worker" \
  -selector "k8s:ns:default" \
  2>&1 | grep -v "already exists" || true

echo "Registered forge-worker"
echo ""
echo "Restart deployments to pick up new SVIDs:"
echo "  kubectl rollout restart deployment/svid-exchange deployment/forge-api deployment/forge-worker"
