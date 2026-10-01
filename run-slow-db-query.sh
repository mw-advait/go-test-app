#!/bin/bash
# Dedicated script for testing Slow DB Query detection (P3)
# Triggers DB spans that cross the threshold:
#   - db.statement starts with SELECT, non-truncated
#   - span duration >= 1000ms
#
# Usage: ./run-slow-db-query.sh [count]
#   count — number of requests to send (default: 10)

MW_URL="http://localhost:3011"
SENTRY_URL="http://localhost:3012"

COUNT="${1:-10}"

GREEN='\033[0;32m'
YELLOW='\033[1;33m'
CYAN='\033[0;36m'
RED='\033[0;31m'
DIM='\033[2m'
BOLD='\033[1m'
NC='\033[0m'

echo ""
echo -e "${BOLD}Slow DB Query Test (P3)${NC}"
echo -e "${DIM}Threshold: SELECT query, duration >= 1000ms, non-truncated db.statement${NC}"
echo -e "${DIM}Settings:  count=${COUNT}${NC}"
echo -e "${DIM}MW app:    $MW_URL${NC}"
echo -e "${DIM}Sentry app: $SENTRY_URL${NC}"
echo ""

# Health check
mw_ok=false; sentry_ok=false
echo -n "  MW health...     "
curl -sf "$MW_URL/health" > /dev/null 2>&1 && { echo -e "${GREEN}OK${NC}"; mw_ok=true; } || echo -e "${RED}FAILED${NC}"
echo -n "  Sentry health... "
curl -sf "$SENTRY_URL/health" > /dev/null 2>&1 && { echo -e "${GREEN}OK${NC}"; sentry_ok=true; } || echo -e "${RED}FAILED${NC}"

if ! $mw_ok && ! $sentry_ok; then
    echo ""
    echo -e "${RED}Both apps are down. Run: docker compose up -d --build${NC}"
    exit 1
fi

PATH_QUERY="/test/slow-db"

echo ""
echo -e "${CYAN}══════════════════════════════════════════════════${NC}"
echo -e "${CYAN}  P3: Slow DB Query (>= 1000ms SELECT)${NC}"
echo -e "${CYAN}══════════════════════════════════════════════════${NC}"
echo ""

# First request with full output
echo -e "  ${BOLD}Sample request:${NC}"
if $mw_ok; then
    echo -e "  ${GREEN}MW${NC}:"
    curl -s "$MW_URL$PATH_QUERY" 2>/dev/null | python3 -c "
import sys, json
try:
    d = json.load(sys.stdin)
    print(f'    rule:        {d.get(\"rule\", \"?\")}')
    print(f'    query_ms:    {d.get(\"query_ms\", \"?\")}')
    print(f'    threshold:   {d.get(\"threshold\", \"?\")}')
except Exception as e:
    print(f'    parse error: {e}')
" 2>/dev/null
fi

if $sentry_ok; then
    echo -e "  ${YELLOW}Sentry${NC}:"
    curl -s "$SENTRY_URL$PATH_QUERY" 2>/dev/null | python3 -c "
import sys, json
try:
    d = json.load(sys.stdin)
    print(f'    rule:        {d.get(\"rule\", \"?\")}')
    print(f'    query_ms:    {d.get(\"query_ms\", \"?\")}')
    print(f'    threshold:   {d.get(\"threshold\", \"?\")}')
except Exception as e:
    print(f'    parse error: {e}')
" 2>/dev/null
fi

# Bulk requests
REMAINING=$((COUNT - 1))
if [ "$REMAINING" -gt 0 ]; then
    echo ""
    echo -n "  Sending $REMAINING more requests..."
    for i in $(seq 2 $COUNT); do
        $mw_ok && curl -sf "$MW_URL$PATH_QUERY" > /dev/null 2>&1 &
        $sentry_ok && curl -sf "$SENTRY_URL$PATH_QUERY" > /dev/null 2>&1 &
        wait
        if [ $((i % 5)) -eq 0 ]; then echo -n " $i"; fi
    done
    echo -e " ${GREEN}done${NC}"
fi

echo ""
echo -e "${CYAN}══════════════════════════════════════════════════${NC}"
echo -e "${CYAN}  Done${NC}"
echo -e "${CYAN}══════════════════════════════════════════════════${NC}"
echo ""
echo -e "  ${BOLD}Total requests sent:${NC} $COUNT per app"
echo -e "  ${BOLD}Expected duration:${NC}  ~1100ms+ per query (threshold: 1000ms)"
echo -e "  ${BOLD}db.statement:${NC}       Full SELECT JOIN (non-truncated)"
echo ""
echo -e "  ${DIM}Check dashboards in 2-3 min for detection:${NC}"
echo "    MW OpsAI:      https://advait.beta.env.middleware.io/ops-ai"
echo "    Sentry Issues: https://sentry.io → go-gin → Performance Issues"
echo ""
