#!/usr/bin/env bash
#
# The demo, end to end: upload the corpus, break the lab, file an incident, and
# open the page while the agent investigates.
#
# Run it through `make demo`, which loads .env. It assumes `make up-lab` and the
# three binaries are already running — see the README.
#
# It is a script rather than a list of commands in a README because it is the
# first thing that breaks when something regresses, and a sequence nobody runs
# is a sequence that is already broken.

set -euo pipefail

API="${DEMO_API_URL:-http://127.0.0.1:8080}"
PAYMENT="${PAYMENT_URL:-http://127.0.0.1:8082}"
CORPUS="${DEMO_CORPUS:-testdata/knowledge}"

# How long the fault runs before the incident is filed. The agent's queries use
# five-minute rate windows, so a fault injected a moment ago is invisible to it.
WARMUP="${DEMO_WARMUP:-90}"

say() { printf '\n\033[1m%s\033[0m\n' "$*"; }

require() {
  if ! curl -fsS -o /dev/null "$1"; then
    echo "$2" >&2
    exit 1
  fi
}

say "0. Checking that everything is up"
require "$API/healthz"       "The API is not answering on $API. Start it with: go run ./cmd/api"
require "$PAYMENT/health"    "payment-service is not answering on $PAYMENT. Start the lab with: make up-lab"
echo "ok"

say "1. Uploading the knowledge corpus"
# Uploaded through the API, so the ingestion worker chunks and embeds them the
# way it would for anyone else's documents.
#
# A file that is already READY is skipped, so running the demo twice does not
# put a second copy of every runbook in the index. It did, once, and the agent's
# retrieval spent its top-k on the same document twice.
#
# What each document *is* comes from the corpus manifest rather than from its
# filename: cpu-saturation-runbook.md applies to either service and has no
# "checkout" in its name, so guessing would file it under payment-service and
# hide it from a search filtered to the other.
ready=$(curl -fsS "$API/api/documents?limit=100" |
  python3 -c 'import json,sys; print(" ".join(d["filename"] for d in json.load(sys.stdin)["items"] if d["status"] == "READY"))')

while IFS='|' read -r name kind service; do
  [ -n "$name" ] || continue
  case " $ready " in
    *" $name "*) printf '  %-40s %s skipped, already indexed\n' "$name" "$kind"; continue ;;
  esac

  args=(-F "file=@$CORPUS/$name" -F "title=${name%.md}" -F "document_type=$kind")
  # The manifest writes null for a runbook that belongs to no one service, and
  # the field is then left off rather than sent empty.
  [ -n "$service" ] && args+=(-F "service=$service")

  status=$(curl -fsS -X POST "$API/api/documents" "${args[@]}" \
    -o /dev/null -w '%{http_code}' || true)
  printf '  %-40s %s %s\n' "$name" "$kind" "$status"
done < <(python3 -c '
import json, sys
for d in json.load(open(sys.argv[1] + "/manifest.json"))["documents"]:
    print("%s|%s|%s" % (d["source"], d["document_type"], d["service"] or ""))
' "$CORPUS")

say "2. Waiting for the documents to be indexed"
# PENDING -> PROCESSING -> READY, through Kafka and the ingestion worker.
for _ in $(seq 60); do
  pending=$(curl -fsS "$API/api/documents?limit=100" |
    python3 -c 'import json,sys; print(sum(1 for d in json.load(sys.stdin)["items"] if d["status"] != "READY"))')
  [ "$pending" = "0" ] && break
  printf '  %s still not READY\n' "$pending"
  sleep 3
done
echo "  all READY"

say "3. Breaking payment-service on purpose"
curl -fsS -X POST "$PAYMENT/fault" -H 'Content-Type: application/json' \
  -d '{"kind":"latency","enabled":true,"delay_ms":2500,"jitter_ms":250}' >/dev/null
echo "  the card processor now takes ~2.5s, inside payment-service's connection pool"

# Cleared however this script exits, including a Ctrl-C: a lab left broken makes
# the next demo meaningless.
cleanup() {
  curl -fsS -X POST "$PAYMENT/fault" -H 'Content-Type: application/json' \
    -d '{"kind":"latency","enabled":false}' >/dev/null || true
  echo "  fault cleared"
}
trap cleanup EXIT

say "4. Generating traffic for ${WARMUP}s, so Prometheus has something to see"
end=$((SECONDS + WARMUP))
while [ $SECONDS -lt $end ]; do
  curl -fsS -m 10 -X POST "${CHECKOUT_URL:-http://127.0.0.1:8081}/orders" \
    -H 'Content-Type: application/json' \
    -d '{"customer_id":"demo","items":[{"sku":"sku-1","quantity":2,"unit_price_cents":625}]}' \
    -o /dev/null || true
  printf '.'
done
echo

say "5. Filing the incident"
incident=$(curl -fsS -X POST "$API/api/incidents" -H 'Content-Type: application/json' -d '{
  "title": "Checkout is timing out on payment",
  "description": "POST /orders on checkout-service has been returning 504 for the last ten minutes. payment-service still answers 200 but slowly. No deploy went out today.",
  "service": "checkout-service",
  "severity": "SEV2"
}' | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')
echo "  incident $incident"

run=$(curl -fsS -X POST "$API/api/incidents/$incident/runs" \
  -H 'Content-Type: application/json' -d '{}' |
  python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')
echo "  run $run"

say "6. Watch it"
echo "  browser:  $API/"
echo "  stream:   curl -N $API/api/runs/$run/events"
echo "  traces:   http://127.0.0.1:16686   (search for service agent-worker)"
echo "  metrics:  http://127.0.0.1:3000    (the incident_diag dashboards)"
echo
echo "Following the run. Ctrl-C to stop; the fault is cleared either way."

# The same stream the browser reads.
curl -N -sS "$API/api/runs/$run/events" || true
