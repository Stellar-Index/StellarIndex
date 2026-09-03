#!/usr/bin/env bash
# dns-perimeter-check.sh — assert the intended DNS/email perimeter for
# stellarindex.io against the authoritative nameservers.
#
# The email perimeter lives OUTSIDE this repo, so none of the repo's gate
# machinery can see it. #334 found the domain with no MX, no SPF, no DMARC and
# no CAA while it was sending magic-link auth email — spoofable, with a dead
# security@ letterbox. This script is the drift check that would have caught it,
# and it is the reason the intended record set is written down at all.
#
# Exit code = number of failed assertions, so cron and Healthchecks.io can
# consume it the way scripts/dev/r1-smoke.sh is consumed.
#
#   bash scripts/ops/dns-perimeter-check.sh
#   DOMAIN=stellarindex.io bash scripts/ops/dns-perimeter-check.sh
#
# Design notes for whoever changes a record and comes here to update the script:
#
#   * SPF is asserted as an EXACT string, not a substring. A second apex SPF
#     record is a permerror that silently disables SPF everywhere, and a
#     substring match would not see it. The apex record must stay singular.
#   * CAA is asserted as a SUPERSET check, not equality. Cloudflare injects its
#     own CA set (it added comodoca.com and digicert.com to ours), so pinning
#     equality would go red the next time Cloudflare rotates a partner. What
#     matters is that letsencrypt.org is present — r1's Caddy renews through it
#     and a CAA set that omitted it would take api.stellarindex.io TLS-dark
#     roughly 60 days later, silently.
#   * DMARC alignment is deliberately RELAXED. Resend's Return-Path is
#     @send.stellarindex.io while From: is @stellarindex.io, so aspf=s would
#     fail SPF alignment on every legitimate message we send. DKIM (d=
#     stellarindex.io) aligns strictly either way, so DMARC would still pass —
#     but there is no reason to build in that fragility. The original issue
#     suggested adkim=s/aspf=s; it was wrong for this topology.
set -uo pipefail

DOMAIN="${DOMAIN:-stellarindex.io}"
# Land the NS list before slicing it: `dig | sort | head -1` under
# `pipefail` makes head exit early, SIGPIPEs sort, and fails the script
# (scripts/ci/lint-shell-sigpipe.sh refuses that shape for this reason).
if [ -z "${NS:-}" ]; then
  ns_list="$(dig +short NS "$DOMAIN" | sort)"
  NS="${ns_list%%$'\n'*}"
fi
[ -z "$NS" ] && { echo "FATAL: no NS for $DOMAIN"; exit 99; }

FAILURES=0
pass() { printf '  \033[32mok\033[0m   %s\n' "$1"; }
fail() { printf '  \033[31mFAIL\033[0m %s\n' "$1"; FAILURES=$((FAILURES + 1)); }

q() { dig +short @"$NS" "$1" "$2" 2>/dev/null; }

echo "DNS perimeter check: $DOMAIN (authoritative: $NS)"
echo

# --- 1. Inbound mail exists at all -------------------------------------------
echo "inbound"
mx="$(q MX "$DOMAIN")"
if [ -z "$mx" ]; then
  fail "apex MX: none — security@ and hello@ are dead letterboxes (#334)"
else
  n=$(printf '%s\n' "$mx" | grep -c .)
  pass "apex MX: $n record(s)"
fi

# --- 2. SPF: exactly one apex record, hard fail ------------------------------
echo "spf"
spf_all="$(q TXT "$DOMAIN" | grep -c 'v=spf1' || true)"
spf="$(q TXT "$DOMAIN" | tr -d '"' | grep '^v=spf1' || true)"
if [ "$spf_all" -gt 1 ]; then
  fail "apex SPF: $spf_all records — more than one is a PERMERROR that disables SPF"
elif [ -z "$spf" ]; then
  fail "apex SPF: absent — receivers have no instruction and cannot reject a spoof"
else
  pass "apex SPF: $spf"
  case "$spf" in
    *" -all") pass "apex SPF ends in -all (hard fail)" ;;
    *)        fail "apex SPF does not end in -all: $spf" ;;
  esac
  # Resend rides SES; Cloudflare Email Routing rides its own include.
  for inc in "include:amazonses.com" "include:_spf.mx.cloudflare.net"; do
    case "$spf" in
      *"$inc"*) pass "apex SPF carries $inc" ;;
      *)        fail "apex SPF missing $inc — that sender will hard-fail" ;;
    esac
  done
fi

# The Return-Path subdomain Resend actually sends from.
send_spf="$(q TXT "send.$DOMAIN" | tr -d '"' | grep '^v=spf1' || true)"
if [ -n "$send_spf" ]; then
  pass "send. SPF: $send_spf"
else
  fail "send. SPF absent — Resend Return-Path unauthorised"
fi

# --- 3. DMARC ----------------------------------------------------------------
echo "dmarc"
dmarc="$(q TXT "_dmarc.$DOMAIN" | tr -d '"' | grep '^v=DMARC1' || true)"
if [ -z "$dmarc" ]; then
  fail "_dmarc: absent — a spoof passes with no policy to reject it"
else
  pass "_dmarc: $dmarc"
  case "$dmarc" in
    *"p=quarantine"*|*"p=reject"*) pass "DMARC policy is enforcing" ;;
    *"p=none"*) fail "DMARC p=none — reporting only, nothing is rejected" ;;
    *)          fail "DMARC has no p= tag" ;;
  esac
  case "$dmarc" in
    *"rua="*) pass "DMARC has an aggregate-report address" ;;
    *)        fail "DMARC has no rua= — policy cannot be tuned from evidence" ;;
  esac
  # See the header note: strict alignment breaks Resend's subdomain Return-Path.
  case "$dmarc" in
    *"aspf=s"*) fail "DMARC aspf=s — breaks SPF alignment for send.$DOMAIN Return-Path" ;;
    *)          pass "DMARC SPF alignment is relaxed (correct for this topology)" ;;
  esac
fi

# --- 4. DKIM -----------------------------------------------------------------
echo "dkim"
for sel in resend cf2024-1; do
  if q TXT "${sel}._domainkey.$DOMAIN" | grep -q 'p='; then
    pass "DKIM selector ${sel}: published"
  else
    fail "DKIM selector ${sel}: absent"
  fi
done

# --- 5. CAA ------------------------------------------------------------------
echo "caa"
caa="$(q CAA "$DOMAIN")"
if [ -z "$caa" ]; then
  fail "CAA: absent — any public CA may issue for $DOMAIN"
else
  pass "CAA: $(printf '%s\n' "$caa" | grep -c .) record(s)"
  # r1's Caddy renews through Let's Encrypt. Losing this entry takes
  # api.$DOMAIN TLS-dark at the next renewal, ~60 days later, silently.
  if printf '%s' "$caa" | grep -q 'letsencrypt.org'; then
    pass "CAA allows letsencrypt.org (r1 Caddy renewal)"
  else
    fail "CAA omits letsencrypt.org — r1 Caddy renewal WILL fail"
  fi
  if printf '%s' "$caa" | grep -q 'pki.goog'; then
    pass "CAA allows pki.goog (Cloudflare Universal SSL)"
  else
    fail "CAA omits pki.goog — Cloudflare Pages cert renewal may fail"
  fi
fi

# --- 6. DNSSEC (informational: the DS lives at the registrar) ----------------
echo "dnssec"
if dig +short DS "$DOMAIN" @1.1.1.1 | grep -q .; then
  pass "DS published at the registrar"
else
  printf '  \033[33mnote\033[0m no DS at the registrar — DNSSEC is inert (see the doc)\n'
fi

echo
if [ "$FAILURES" -eq 0 ]; then
  echo "ALL CHECKS PASSED"
else
  echo "$FAILURES check(s) failed"
fi
exit "$FAILURES"
