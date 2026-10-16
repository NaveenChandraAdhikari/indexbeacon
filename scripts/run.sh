#!/usr/bin/env bash
#
# Run idxa from the published image, with one command.
#
#     ./scripts/run.sh
#
# It pulls the image, starts it with a named volume so your configured
# databases survive a restart, waits until it answers, and prints the URL to
# open. Run it again to restart; run it with --stop to remove the container.

set -euo pipefail

IMAGE="${IDXA_IMAGE:-indexbeacon/indexbeacon:latest}"
NAME="${IDXA_CONTAINER:-idxa}"
PORT="${IDXA_PORT:-8777}"
VOLUME="${IDXA_VOLUME:-idxa-data}"
TOKEN_FILE="${HOME}/.config/idxa/container-token"

usage() {
  cat >&2 <<'USAGE'
usage: ./scripts/run.sh [options]

  (no options)   pull, start, and print the URL
  --stop         stop and remove the container (the volume is kept)
  --remove-data  also delete the volume, losing every configured database
  --logs         follow the container's output
  --url          print the URL again

environment:
  IDXA_IMAGE      image to run       (default: indexbeacon/indexbeacon:latest)
  IDXA_PORT       host port          (default: 8777)
  IDXA_VOLUME     named volume       (default: idxa-data)
  IDXA_TOKEN      access token       (default: generated once and remembered)
USAGE
  exit 2
}

command -v docker >/dev/null || { echo "docker is not installed" >&2; exit 1; }
docker info >/dev/null 2>&1 || { echo "docker is not running" >&2; exit 1; }

# The token is generated once and kept, so restarting does not invalidate the
# link you have open in a browser.
load_token() {
  if [[ -n "${IDXA_TOKEN:-}" ]]; then
    echo "$IDXA_TOKEN"
    return
  fi
  if [[ -f "$TOKEN_FILE" ]]; then
    cat "$TOKEN_FILE"
    return
  fi
  local generated
  generated="$(head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n')"
  mkdir -p "$(dirname "$TOKEN_FILE")"
  ( umask 077; printf '%s' "$generated" > "$TOKEN_FILE" )
  echo "$generated"
}

case "${1:-}" in
  --stop)
    docker rm -f "$NAME" >/dev/null 2>&1 && echo "stopped and removed ${NAME}" || echo "${NAME} was not running"
    echo "the volume ${VOLUME} was kept; use --remove-data to delete it"
    exit 0
    ;;
  --remove-data)
    docker rm -f "$NAME" >/dev/null 2>&1 || true
    read -r -p "Delete volume ${VOLUME}, losing every configured database? [y/N] " reply
    case "$reply" in
      [yY]|[yY][eE][sS]) docker volume rm "$VOLUME" >/dev/null && echo "removed ${VOLUME}" ;;
      *) echo "kept ${VOLUME}" ;;
    esac
    exit 0
    ;;
  --logs)
    exec docker logs -f "$NAME"
    ;;
  --url)
    echo "http://127.0.0.1:${PORT}/?token=$(load_token)"
    exit 0
    ;;
  "") ;;
  *) usage ;;
esac

TOKEN="$(load_token)"

echo "pulling ${IMAGE}"
docker pull -q "$IMAGE" >/dev/null

# Replace any previous container, keeping the volume and therefore the
# databases already configured.
docker rm -f "$NAME" >/dev/null 2>&1 || true

docker run -d \
  --name "$NAME" \
  --restart unless-stopped \
  -p "127.0.0.1:${PORT}:8777" \
  -v "${VOLUME}:/var/lib/idxa" \
  -e "IDXA_TOKEN=${TOKEN}" \
  "$IMAGE" >/dev/null

printf 'starting'
for _ in $(seq 1 45); do
  if curl -fsS "http://127.0.0.1:${PORT}/api/settings?token=${TOKEN}" >/dev/null 2>&1; then
    echo
    echo
     echo "idxa is running:  http://127.0.0.1:${PORT}/?token=${TOKEN}"
    echo
    echo "  logs:  ./scripts/run.sh --logs"
    echo "  stop:  ./scripts/run.sh --stop"
    echo
     echo "It only reads. idxa never creates, drops or alters anything in the"
    echo "databases you point it at."
    exit 0
  fi
  printf '.'
  sleep 1
done

echo
echo "idxa did not answer within 45 seconds. What it printed:" >&2
docker logs --tail 30 "$NAME" >&2
exit 1
