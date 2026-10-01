#!/bin/bash
# Run all rule triggers against MW and/or Sentry Go app instances side-by-side
# Usage: ./run-tests.sh [--mw-only|--sentry-only] [section]
#   ./run-tests.sh              — run both MW + Sentry (side-by-side)
#   ./run-tests.sh --mw-only    — run only against MW
#   ./run-tests.sh --sentry-only — run only against Sentry
#   ./run-tests.sh perf         — performance rules only (P1-P13)
#   ./run-tests.sh baseline     — existing MW detections (M1-M4)
#   ./run-tests.sh debug        — debug endpoints only
#   ./run-tests.sh p1           — single rule (p1-p13/m1-m4)

MW_URL="http://localhost:3011"
SENTRY_URL="http://localhost:3012"
MODE="both"  # both | mw | sentry

if [[ "$1" == "--mw-only" ]]; then
    MODE="mw"; shift
elif [[ "$1" == "--sentry-only" ]]; then
    MODE="sentry"; shift
fi

SECTION="${1:-all}"
TMPDIR_RUN=$(mktemp -d)
trap "rm -rf $TMPDIR_RUN" EXIT

GREEN='\033[0;32m'
YELLOW='\033[1;33m'
CYAN='\033[0;36m'
RED='\033[0;31m'
DIM='\033[2m'
BOLD='\033[1m'
NC='\033[0m'

# ─── Helpers ────────────────────────────────────

hit_both() {
    local path="$1"
    [[ "$MODE" != "sentry" ]] && curl -sf "$MW_URL$path" > /dev/null 2>&1 &
    [[ "$MODE" != "mw" ]]     && curl -sf "$SENTRY_URL$path" > /dev/null 2>&1 &
    wait
}

_parse_info() {
    python3 -c "
import sys,json
try:
    d=json.load(sys.stdin)
    parts=[]
    for k in ['rule','query_ms','duration_ms','total_queries','error_type','message','variant','scenario']:
        if k in d: parts.append(f'{k}={d[k]}')
    print(', '.join(parts[:3]) if parts else 'ok')
except: print('parse-error')
" 2>/dev/null
}

compare() {
    local path="$1"
    local label="$2"
    local mw_out="$TMPDIR_RUN/mw_cmp"
    local sentry_out="$TMPDIR_RUN/sentry_cmp"

    if [[ "$MODE" != "sentry" ]]; then
        curl -s -w '\n%{http_code}' "$MW_URL$path" 2>/dev/null > "$mw_out" &
        local mw_pid=$!
    fi
    if [[ "$MODE" != "mw" ]]; then
        curl -s -w '\n%{http_code}' "$SENTRY_URL$path" 2>/dev/null > "$sentry_out" &
        local sentry_pid=$!
    fi
    wait 2>/dev/null

    if [[ "$MODE" != "sentry" ]]; then
        local mw_code=$(tail -1 "$mw_out")
        local mw_body=$(sed '$d' "$mw_out")
        local mw_info=$(echo "$mw_body" | _parse_info)
        printf "  %-28s  ${GREEN}MW${NC} [%s] %-s\n" "$label" "$mw_code" "$mw_info"
    fi
    if [[ "$MODE" != "mw" ]]; then
        local sentry_code=$(tail -1 "$sentry_out")
        local sentry_body=$(sed '$d' "$sentry_out")
        local sentry_info=$(echo "$sentry_body" | _parse_info)
        printf "  %-28s  ${YELLOW}SE${NC} [%s] %-s\n" "$([[ "$MODE" == "both" ]] && echo '' || echo "$label")" "$sentry_code" "$sentry_info"
    fi
}

header() {
    echo ""
    echo -e "${CYAN}══════════════════════════════════════════════════${NC}"
    echo -e "${CYAN}  $1${NC}"
    echo -e "${CYAN}══════════════════════════════════════════════════${NC}"
    echo ""
}

# ─── Health Check ───────────────────────────────

echo ""
echo -e "${BOLD}Go App Test Runner — mode: $MODE${NC}"
[[ "$MODE" != "sentry" ]] && echo -e "${DIM}MW app:     $MW_URL  (port 3011)${NC}"
[[ "$MODE" != "mw" ]]     && echo -e "${DIM}Sentry app: $SENTRY_URL  (port 3012)${NC}"
echo ""

mw_ok=false; sentry_ok=false
if [[ "$MODE" != "sentry" ]]; then
    echo -n "  MW health...     "
    curl -sf "$MW_URL/health" > /dev/null 2>&1 && { echo -e "${GREEN}OK${NC}"; mw_ok=true; } || echo -e "${RED}FAILED${NC}"
fi
if [[ "$MODE" != "mw" ]]; then
    echo -n "  Sentry health... "
    curl -sf "$SENTRY_URL/health" > /dev/null 2>&1 && { echo -e "${GREEN}OK${NC}"; sentry_ok=true; } || echo -e "${RED}FAILED${NC}"
fi

if [[ "$MODE" == "both" ]] && { ! $mw_ok || ! $sentry_ok; }; then
    echo ""
    echo -e "${RED}One or both apps are down. Run: docker compose up -d --build${NC}"
    exit 1
elif [[ "$MODE" == "mw" ]] && ! $mw_ok; then
    echo ""
    echo -e "${RED}MW app is down. Run: docker compose up -d --build${NC}"
    exit 1
elif [[ "$MODE" == "sentry" ]] && ! $sentry_ok; then
    echo ""
    echo -e "${RED}Sentry app is down. Run: docker compose up -d --build${NC}"
    exit 1
fi

# ─── Debug ──────────────────────────────────────

run_debug() {
    header "Debug — Sentry Tracing Verification"

    echo -e "  ${BOLD}Sentry /debug-sentry-tracing:${NC}"
    curl -s "$SENTRY_URL/debug-sentry-tracing" 2>/dev/null | python3 -c "
import sys,json
d=json.load(sys.stdin)
for k,v in d.items():
    print(f'    {k}: {v}')
" 2>/dev/null || echo "    (no response or timeout)"

    echo ""
    echo -e "  ${BOLD}MW /debug-sentry-tracing:${NC}"
    curl -s "$MW_URL/debug-sentry-tracing" 2>/dev/null | python3 -c "
import sys,json
d=json.load(sys.stdin)
for k,v in d.items():
    print(f'    {k}: {v}')
" 2>/dev/null || echo "    (no response or timeout)"
}

# ─── P1: N+1 Queries ───────────────────────────

run_p1() {
    header "P1: N+1 Queries (threshold: 5+ spans, total > 100ms)"
    echo -e "  ${DIM}Each request does 1 source query + N individual queries with pg_sleep${NC}"
    echo ""
    compare "/test/n-plus-one?count=20" "count=20 (20 queries)"
    echo ""
    echo -n "  Bulk: 9 more with count=20"
    for i in $(seq 2 10); do hit_both "/test/n-plus-one?count=20"; done
    echo -e " ${GREEN}done${NC}"
    echo -n "  Bulk: 5 with count=50"
    for i in $(seq 1 5); do hit_both "/test/n-plus-one?count=50"; done
    echo -e " ${GREEN}done${NC}"
}

# ─── P2: Consecutive DB ────────────────────────

run_p2() {
    header "P2: Consecutive DB Queries (threshold: savings > 100ms, each > 30ms)"
    echo -e "  ${DIM}6 independent SELECTs with pg_sleep(0.04)${NC}"
    echo ""
    compare "/test/consecutive-db" "6 sequential SELECTs"
    echo ""
    echo -n "  Bulk: 14 more"
    for i in $(seq 2 15); do hit_both "/test/consecutive-db"; done
    echo -e " ${GREEN}done${NC}"
}

# ─── P3: Slow DB ───────────────────────────────

run_p3() {
    header "P3: Slow DB Queries (threshold: SELECT >= 500ms, 100+ times in 24h)"
    echo -e "  ${DIM}SELECT with pg_sleep(0.6) — guaranteed 600ms+ per query${NC}"
    echo ""
    compare "/test/slow-db" "slow SELECT (expect ~600ms)"
    echo ""
    echo -e "  ${DIM}Sending 109 more (need 100+ for Sentry recurrence threshold)${NC}"
    echo -n "  Progress:"
    for i in $(seq 2 110); do
        hit_both "/test/slow-db"
        if [ $((i % 10)) -eq 0 ]; then echo -n " $i"; fi
    done
    echo -e " ${GREEN} done${NC}"
}

# ─── P4: Large HTTP ────────────────────────────

run_p4() {
    header "P4: Large HTTP Payload (threshold: > 300KB AND > 100ms)"
    compare "/test/large-payload?size=400&delay=150" "400KB response, 150ms delay"
    echo ""
    echo -n "  Bulk: 4 more (direct)"
    for i in $(seq 2 5); do hit_both "/test/large-payload?size=400&delay=150"; done
    echo -e " ${GREEN}done${NC}"
    echo -n "  Bulk: 5 outbound (P4b)"
    for i in $(seq 1 5); do hit_both "/test/large-http-payload?size=400&delay=150"; done
    echo -e " ${GREEN}done${NC}"
    echo -n "  Bulk: 5 client-side"
    for i in $(seq 1 5); do hit_both "/test/external-http"; done
    echo -e " ${GREEN}done${NC}"
}

# ─── P6: Consecutive HTTP ─────────────────────

run_p6() {
    header "P6: Consecutive HTTP Calls (5 sequential outbound calls)"
    compare "/test/consecutive-http" "5 sequential HTTP calls"
    echo ""
    echo -n "  Bulk: 9 more"
    for i in $(seq 2 10); do hit_both "/test/consecutive-http"; done
    echo -e " ${GREEN}done${NC}"
}

# ─── P7: DB Connection Leak ───────────────────

run_p7() {
    header "P7: DB Connection Leak (hold 3 connections for 5s)"
    compare "/test/db-connection-leak?count=3&hold=5000" "3 leaked connections"
    echo ""
    echo -n "  Bulk: 2 more"
    for i in $(seq 2 3); do hit_both "/test/db-connection-leak?count=2&hold=3000"; done
    echo -e " ${GREEN}done${NC}"
}

# ─── P8: Long Transaction ─────────────────────

run_p8() {
    header "P8: Long Transaction (locks held ~800ms)"
    compare "/test/long-transaction" "800ms transaction"
    echo ""
    echo -n "  Bulk: 9 more"
    for i in $(seq 2 10); do hit_both "/test/long-transaction"; done
    echo -e " ${GREEN}done${NC}"
}

# ─── P9: Nested N+1 ───────────────────────────

run_p9() {
    header "P9: Nested N+1 (users → orders, two N+1 patterns)"
    compare "/test/nested-n-plus-one" "nested N+1 (10+10 queries)"
    echo ""
    echo -n "  Bulk: 4 more"
    for i in $(seq 2 5); do hit_both "/test/nested-n-plus-one"; done
    echo -e " ${GREEN}done${NC}"
}

# ─── P10: Retry Storm ─────────────────────────

run_p10() {
    header "P10: Retry Storm (5 retries with no backoff)"
    compare "/test/retry-storm?retries=5" "5 retries"
    echo ""
    echo -n "  Bulk: 9 more"
    for i in $(seq 2 10); do hit_both "/test/retry-storm?retries=5"; done
    echo -e " ${GREEN}done${NC}"
}

# ─── P11: N+1 HTTP ────────────────────────────

run_p11() {
    header "P11: N+1 HTTP Calls (15 sequential identical GETs)"
    compare "/test/n-plus-one-http?count=15" "15 sequential GETs"
    echo ""
    echo -n "  Bulk: 4 more"
    for i in $(seq 2 5); do hit_both "/test/n-plus-one-http?count=15"; done
    echo -e " ${GREEN}done${NC}"
}

# ─── P12: Slow HTTP ───────────────────────────

run_p12() {
    header "P12: Slow Outbound HTTP (>= 500ms)"
    compare "/test/slow-http?delay=2000" "2s outbound call"
    echo ""
    echo -n "  Bulk: 4 more"
    for i in $(seq 2 5); do hit_both "/test/slow-http?delay=2000"; done
    echo -e " ${GREEN}done${NC}"
}

# ─── P13: Uncompressed ────────────────────────

run_p13() {
    header "P13: Uncompressed Response (> 512KB without gzip)"
    compare "/test/uncompressed-response?size=600" "600KB uncompressed"
    echo ""
    echo -n "  Bulk: 4 more"
    for i in $(seq 2 5); do hit_both "/test/uncompressed-response?size=600"; done
    echo -e " ${GREEN}done${NC}"
}

# ─── M1: exception.message ─────────────────────

run_m1() {
    header "M1: Runtime Exceptions"
    for variant in null-ref async-reject deep-stack db-error; do
        compare "/test/existing/exception-message?variant=$variant" "variant=$variant"
    done
    echo ""
    echo -n "  Bulk: 4 more of each"
    for variant in null-ref async-reject deep-stack db-error; do
        for i in $(seq 2 5); do hit_both "/test/existing/exception-message?variant=$variant"; done
    done
    echo -e " ${GREEN}done${NC}"
}

# ─── M2: Business errors ───────────────────────

run_m2() {
    header "M2: Business Errors"
    for scenario in payment auth validation timeout; do
        compare "/test/existing/business-error?scenario=$scenario" "scenario=$scenario"
    done
    echo ""
    echo -n "  Bulk: 4 more of each"
    for scenario in payment auth validation timeout; do
        for i in $(seq 2 5); do hit_both "/test/existing/business-error?scenario=$scenario"; done
    done
    echo -e " ${GREEN}done${NC}"
}

# ─── M3: High Tail Latency ─────────────────────

run_m3() {
    header "M3: High Tail Latency (3s+ requests)"
    compare "/test/existing/high-latency?delay=3000" "3s delay"
    echo ""
    echo -n "  Bulk: 4 more"
    for i in $(seq 2 5); do hit_both "/test/existing/high-latency?delay=3000"; done
    echo -e " ${GREEN}done${NC}"
}

# ─── M4: Log Errors ────────────────────────────

run_m4() {
    header "M4: Log Errors"
    compare "/test/existing/log-errors?count=20" "20 error logs"
    echo ""
    echo -n "  Bulk: 4 more"
    for i in $(seq 2 5); do hit_both "/test/existing/log-errors?count=20"; done
    echo -e " ${GREEN}done${NC}"
}

# ─── Run sections ──────────────────────────────

case "$SECTION" in
    all)
        run_debug
        echo ""
        echo -e "${BOLD}─── Performance Rules ───${NC}"
        run_p1; run_p2; run_p3; run_p4; run_p6; run_p7; run_p8; run_p9; run_p10; run_p11; run_p12; run_p13
        echo ""
        echo -e "${BOLD}─── Existing MW Detections (baseline) ───${NC}"
        run_m1; run_m2; run_m3; run_m4
        ;;
    debug)    run_debug ;;
    perf)     run_p1; run_p2; run_p3; run_p4; run_p6; run_p7; run_p8; run_p9; run_p10; run_p11; run_p12; run_p13 ;;
    baseline) run_m1; run_m2; run_m3; run_m4 ;;
    p1) run_p1 ;; p2) run_p2 ;; p3) run_p3 ;; p4) run_p4 ;;
    p6) run_p6 ;; p7) run_p7 ;; p8) run_p8 ;; p9) run_p9 ;; p10) run_p10 ;;
    p11) run_p11 ;; p12) run_p12 ;; p13) run_p13 ;;
    m1) run_m1 ;; m2) run_m2 ;; m3) run_m3 ;; m4) run_m4 ;;
    *)
        echo -e "${RED}Unknown section: $SECTION${NC}"
        echo "Usage: ./run-tests.sh [all|debug|perf|baseline|p1..p13|m1..m4]"
        exit 1
        ;;
esac

header "Done — Compare Results"

echo -e "  ${BOLD}Check these dashboards (allow 2-3 min for data to appear):${NC}"
echo ""
echo "    MW OpsAI:         https://advait.beta.env.middleware.io/ops-ai"
echo "    Sentry Issues:    https://sentry.io → go-gin → Issues"
echo "    Sentry Traces:    https://sentry.io → Explore → Traces"
echo ""
