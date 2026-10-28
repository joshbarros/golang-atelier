#!/usr/bin/env bash
# What Kubernetes buys: kill an API replica and the RabbitMQ pod in the middle of a booking and check that nothing is lost or
# done twice. Five bundles (a seat with a laptop kit) compete for three kits: exactly three sell, the other two are refused
# whole and take no seat. Needs the cluster from `make cluster-up`.
set -euo pipefail

API="${API:-http://127.0.0.1:5234}"
CTX="${CTX:-kind-golang-atelier}"
K="kubectl --context $CTX -n atelier"
RUN="$(date +%s)$((RANDOM % 900 + 100))"
JSON='content-type: application/json'
say() { printf '  %s\n' "$*"; }
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
jget() { python3 -c "import sys,json; d=json.load(sys.stdin); print(d$1)"; }
token_of() { python3 -c "import sys,json; print(json.load(sys.stdin)['accessToken'])"; }
# every call retries a few times: for a moment after a pod dies the Service may still send a request to it
retry() { for _ in $(seq 1 20); do if out="$("$@" 2>/dev/null)"; then printf '%s' "$out"; return 0; fi; sleep 1; done; return 1; }

login() { retry curl -sf -X POST "$API/api/auth/login" -H "$JSON" -d "{\"email\":\"admin@atelier.local\",\"password\":\"${ADMIN_PASSWORD:-AtelierAdmin2026}\"}" | token_of; }
register() { retry curl -sf -X POST "$API/api/auth/register" -H "$JSON" -d "{\"email\":\"$1-$RUN@example.com\",\"password\":\"Resil-Passw0rd1\"}" | token_of; }
ADM="Authorization: Bearer $(login)"
TOKENS=()
for n in a b c d e f; do TOKENS+=("Authorization: Bearer $(register $n)"); done

echo "1. a workshop of 10 seats and 3 laptop kits, and three bundles in flight"
retry curl -sf -X POST "$API/api/workshops" -H "$JSON" -H "$ADM" -d "{\"code\":\"RES-$RUN\",\"name\":\"Resilience\",\"track\":\"Backend\",\"instructor\":\"Dr. Marlo\",\"seatsTotal\":10,\"priceCents\":5000,\"maxPerAttendee\":4,\"kitsTotal\":3,\"kitPriceCents\":1500}" >/dev/null || fail "could not open the workshop"
order() { retry curl -sf -X POST "$API/api/orders" -H "$JSON" -H "$1" -d "{\"workshopCode\":\"RES-$RUN\",\"reference\":\"ATT-$RUN-$2\",\"seats\":1,\"kit\":$3}" | jget "['id']"; }
IDS=("$(order "${TOKENS[0]}" a true)" "$(order "${TOKENS[1]}" b true)" "$(order "${TOKENS[2]}" c true)")

echo "2. kill an API replica and the RabbitMQ pod while the bundles are still in flight"
$K delete "$($K get pods -l app.kubernetes.io/name=api -o name | head -1)" --wait=false | sed 's/^/  /'
$K delete pod rabbitmq-0 --wait=false | sed 's/^/  /'
IDS+=("$(order "${TOKENS[3]}" d true)" "$(order "${TOKENS[4]}" e true)") # accepted even now: the order and its outbox row are one local transaction
say "two more bundles were accepted while the broker was down"

echo "3. everything comes back: exactly three bundles sell, the other two are refused whole"
$K rollout status statefulset/rabbitmq --timeout=180s >/dev/null
$K rollout status deployment/api --timeout=180s >/dev/null
field() { body="$(retry curl -sf -H "$1" "$API/api/orders/$2")" && printf '%s' "$body" | jget "['$3']" || echo unknown; }
settled() { s="$(field "$1" "$2" status)"; [ "$s" = Sold ] || [ "$s" = Rejected ]; }
for _ in $(seq 1 240); do
  n=0; for i in 0 1 2 3 4; do settled "${TOKENS[$i]}" "${IDS[$i]}" && n=$((n + 1)); done
  [ "$n" = 5 ] && break
  sleep 1
done
SOLD=0; REFUSED=0
for i in 0 1 2 3 4; do
  case "$(field "${TOKENS[$i]}" "${IDS[$i]}" status)" in
    Sold) SOLD=$((SOLD + 1)) ;;
    Rejected) REFUSED=$((REFUSED + 1)); case "$(field "${TOKENS[$i]}" "${IDS[$i]}" reason)" in Kits:*"No seat was taken.") ;; *) fail "a refused bundle must say Kits: $(field "${TOKENS[$i]}" "${IDS[$i]}" reason)" ;; esac ;;
    *) fail "a bundle is stuck: $(field "${TOKENS[$i]}" "${IDS[$i]}" status)" ;;
  esac
done
[ "$SOLD" = 3 ] && [ "$REFUSED" = 2 ] || fail "expected 3 sold and 2 refused, got $SOLD and $REFUSED"
W="$(retry curl -sf -H "$ADM" "$API/api/workshops/RES-$RUN")"
[ "$(printf '%s' "$W" | jget "['seatsLeft']")" = 7 ] && [ "$(printf '%s' "$W" | jget "['kitsLeft']")" = 0 ] || fail "3 seats and 3 kits are held, so 7 and 0 are left: $W"
say "3 sold, 2 refused for kits, 7 seats and 0 kits left"

echo "4. the refused bundles took no seat: a booking without a kit still sells"
PLAIN="$(retry curl -sf -X POST "$API/api/orders" -H "$JSON" -H "${TOKENS[5]}" -d "{\"workshopCode\":\"RES-$RUN\",\"reference\":\"ATT-$RUN-f\",\"seats\":2,\"kit\":false}" | jget "['id']")"
for _ in $(seq 1 60); do [ "$(field "${TOKENS[5]}" "$PLAIN" status)" = Sold ] && break; sleep 1; done
[ "$(field "${TOKENS[5]}" "$PLAIN" status)" = Sold ] || fail "a plain booking should sell"
[ "$(retry curl -sf -H "$ADM" "$API/api/workshops/RES-$RUN" | jget "['seatsLeft']")" = 5 ] || fail "5 seats must be left"
VOUCHERS=0
for i in 0 1 2 3 4; do
  if curl -sf -H "$ADM" "$API/api/vouchers/${IDS[$i]}" >/dev/null 2>&1; then VOUCHERS=$((VOUCHERS + 1)); fi
done
[ "$VOUCHERS" = 3 ] || fail "exactly three vouchers may exist, found $VOUCHERS"
say "the plain booking sold, 5 seats left, 3 vouchers"
echo "resilience passed"
