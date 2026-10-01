#!/bin/bash
# Dedicated script for testing Large HTTP Payload detection (P4b)
# Triggers outbound HTTP spans that cross BOTH thresholds:
#   - largeHttpPayloadMinBytes      = 300_000 (300KB)
#   - largeHttpPayloadMinDurationMs = 100     (100ms)
#
# Usage: ./run-large-http-payload.sh [count] [size_kb] [delay_ms]
#   count    — number of requests to send (default: 20)
#   size_kb  — payload size in KB (default: 400, must be > 300)
#   delay_ms — server-side delay in ms (default: 150, must be > 100)

MW_URL="http://localhost:3011"
SENTRY_URL="http://localhost:3012"

COUNT="${1:-20}"
SIZE_KB="${2:-400}"
DELAY_MS="${3:-150}"

GREEN='\033[0;32m'
YELLOW='\033[1;33m'
CYAN='\033[0;36m'
RED='\033[0;31m'
DIM='\033[2m'
BOLD='\033[1m'
NC='\033[0m'

echo ""
echo -e "${BOLD}Large HTTP Payload Test (P4b)${NC}"
echo -e "${DIM}Thresholds: payload > 300KB AND duration > 100ms${NC}"
echo -e "${DIM}Settings:   size=${SIZE_KB}KB  delay=${DELAY_MS}ms  count=${COUNT}${NC}"
echo -e "${DIM}MW app:     $MW_URL${NC}"
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

PATH_QUERY="/test/large-http-payload?size=${SIZE_KB}&delay=${DELAY_MS}"

echo ""
echo -e "${CYAN}══════════════════════════════════════════════════${NC}"
echo -e "${CYAN}  P4b: Large HTTP Payload (outbound)${NC}"
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
    print(f'    response_bytes:   {d.get(\"response_bytes\", \"?\")}')
    print(f'    duration_ms:      {d.get(\"duration_ms\", \"?\")}')
    print(f'    crossed_size:     {d.get(\"crossed_size\", \"?\")}')
    print(f'    crossed_duration: {d.get(\"crossed_duration\", \"?\")}')
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
    print(f'    response_bytes:   {d.get(\"response_bytes\", \"?\")}')
    print(f'    duration_ms:      {d.get(\"duration_ms\", \"?\")}')
    print(f'    crossed_size:     {d.get(\"crossed_size\", \"?\")}')
    print(f'    crossed_duration: {d.get(\"crossed_duration\", \"?\")}')
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
echo -e "  ${BOLD}Expected payload:${NC}   ~${SIZE_KB}KB (threshold: 300KB)"
echo -e "  ${BOLD}Expected duration:${NC}  ~${DELAY_MS}ms+ (threshold: 100ms)"
echo ""
echo -e "  ${DIM}Check dashboards in 2-3 min for detection:${NC}"
echo "    MW OpsAI:      https://advait.beta.env.middleware.io/ops-ai"
echo "    Sentry Issues: https://sentry.io → go-gin → Performance Issues"
echo ""
