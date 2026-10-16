#!/usr/bin/env bash
#
# Deploy idxa to a Kubernetes cluster in one command.
#
#     ./scripts/deploy.sh --image yourname/idxa:v0.1.0
#
# It shows you the cluster and namespace it is about to change and waits for
# you to confirm, because applying to the wrong cluster is an easy mistake to
# make and an unpleasant one to discover.
#
# idxa listens on the pod's loopback address and has no Service, so the only
# way in is `kubectl port-forward`, which already requires permission on the
# namespace. That makes cluster RBAC the access control, instead of a token
# shared with every pod that could reach a ClusterIP.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
MANIFEST="${REPO_ROOT}/deploy/kubernetes/indexbeacon.yaml"

IMAGE=""
NAMESPACE="idxa"
CONTEXT=""
STORAGE_CLASS=""
DRY_RUN=false
ASSUME_YES=false

usage() {
  cat >&2 <<'USAGE'
usage: ./scripts/deploy.sh --image <repo/idxa:tag> [options]

  --image IMAGE        container image to run (required)
  --namespace NAME     namespace to deploy into (default: idxa)
  --context NAME       kubectl context to use (default: the current one)
  --storage-class NAME storage class for the volume (default: the cluster's default)
  --dry-run            print what would be applied and change nothing
  --yes                skip the confirmation prompt
USAGE
  exit 2
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --image) IMAGE="${2:-}"; shift 2 ;;
    --namespace) NAMESPACE="${2:-}"; shift 2 ;;
    --context) CONTEXT="${2:-}"; shift 2 ;;
    --storage-class) STORAGE_CLASS="${2:-}"; shift 2 ;;
    --dry-run) DRY_RUN=true; shift ;;
    --yes) ASSUME_YES=true; shift ;;
    -h|--help) usage ;;
    *) echo "unknown option: $1" >&2; usage ;;
  esac
done

[[ -n "$IMAGE" ]] || { echo "--image is required" >&2; usage; }

command -v kubectl >/dev/null || { echo "kubectl is not installed" >&2; exit 1; }

KUBECTL=(kubectl)
[[ -n "$CONTEXT" ]] && KUBECTL+=(--context "$CONTEXT")

CURRENT_CONTEXT="${CONTEXT:-$("${KUBECTL[@]}" config current-context 2>/dev/null || echo unknown)}"
SERVER="$("${KUBECTL[@]}" config view --minify -o jsonpath='{.clusters[0].cluster.server}' 2>/dev/null || echo unknown)"

# Fill in the manifest.
RENDERED="$(mktemp)"
trap 'rm -f "$RENDERED"' EXIT

sed -e "s|IDXA_IMAGE|${IMAGE}|g" \
    -e "s|^  name: idxa$|  name: ${NAMESPACE}|" \
    -e "s|namespace: idxa|namespace: ${NAMESPACE}|g" \
    "$MANIFEST" > "$RENDERED"

if [[ -n "$STORAGE_CLASS" ]]; then
  # Insert the storage class into the volume claim.
  awk -v sc="$STORAGE_CLASS" '
    /^kind: PersistentVolumeClaim$/ { inpvc = 1 }
    /^---$/ { inpvc = 0 }
    { print }
    inpvc && /^  accessModes:/ { print "  storageClassName: " sc }
  ' "$RENDERED" > "${RENDERED}.sc" && mv "${RENDERED}.sc" "$RENDERED"
fi

echo "About to deploy idxa:"
echo "  context:   ${CURRENT_CONTEXT}"
echo "  server:    ${SERVER}"
echo "  namespace: ${NAMESPACE}"
echo "  image:     ${IMAGE}"
echo "  replicas:  1"
echo "  storage:   1Gi persistent volume${STORAGE_CLASS:+ (${STORAGE_CLASS})}"
echo "  access:    port-forward only, no Service, no token"
echo

if [[ "$DRY_RUN" == true ]]; then
  echo "--- dry run, nothing will be applied ---"
  cat "$RENDERED"
  exit 0
fi

if [[ "$ASSUME_YES" != true ]]; then
  read -r -p "Apply to ${CURRENT_CONTEXT}? [y/N] " reply
  case "$reply" in
    [yY]|[yY][eE][sS]) ;;
    *) echo "cancelled"; exit 1 ;;
  esac
fi

"${KUBECTL[@]}" apply -f "$RENDERED"

echo
echo "waiting for idxa to be ready..."
if ! "${KUBECTL[@]}" -n "$NAMESPACE" rollout status deployment/idxa --timeout=180s; then
  echo
  echo "It did not become ready. What the pod says:" >&2
  "${KUBECTL[@]}" -n "$NAMESPACE" describe pod -l app.kubernetes.io/name=idxa | tail -25 >&2
  "${KUBECTL[@]}" -n "$NAMESPACE" logs -l app.kubernetes.io/name=idxa --tail=40 >&2 || true
  exit 1
fi

echo
echo "idxa is running."
echo
echo "Open it:"
echo "  kubectl -n ${NAMESPACE} port-forward deploy/idxa 8777:8777"
echo "  then visit http://127.0.0.1:8777/"
echo
echo "There is no token and no Service. idxa listens on the pod's loopback"
echo "address, so a port-forward is the only way in, and that already requires"
echo "permission on this namespace."
echo
echo "To point it at a database whose password should not be written to the volume,"
echo "put the password in the secret and reference it in the connection string:"
echo "  kubectl -n ${NAMESPACE} patch secret idxa --type merge \\"
echo "    -p '{\"stringData\":{\"PGPASSWORD_PROD\":\"...\"}}'"
echo "  then use: postgres://user:\${PGPASSWORD_PROD}@host:5432/dbname"
