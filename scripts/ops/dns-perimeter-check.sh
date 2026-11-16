#!/usr/bin/env bash
# dns-perimeter-check.sh — assert the intended DNS/email perimeter for
# stellarindex.io against the authoritative nameservers.
#
# The email perimeter lives outside this repo, so no repo gate sees it. A
# domain with no MX, SPF, DMARC or CAA that sends magic-link auth email is
# spoofable and has a dead security@ letterbox; this is the drift check for
# that, and why the intended record set is written down at all.
#
# Exit code = number of failed assertions (consumed like scripts/dev/r1-smoke.sh
# by cron and Healthchecks.io). 99 when no NS resolves.
#
#   bash scripts/ops/dns-perimeter-check.sh
#   DOMAIN=stellarindex.io bash scripts/ops/dns-perimeter-check.sh
#
# Design notes for whoever changes a record:
#   * Records are read tag by tag, never by substring: `p=none; sp=reject`
#     contains "p=reject", a revoked DKIM key is `p=` with nothing after it,
#     and `+all` ahead of `-all` passes everything. A substring match accepts
#     all three.
#   * The apex SPF record must stay singular: a second one is a permerror
#     that silently disables SPF everywhere.
#   * CAA is a SUPERSET check: Cloudflare injects its own CAs (comodoca.com,
#     digicert.com), so equality goes red when it rotates a partner. What
#     matters is letsencrypt.org: r1's Caddy renews through it, and without
#     it api.stellarindex.io goes TLS-dark ~60 days later, silently.
#   * DMARC alignment is deliberately RELAXED: Resend's Return-Path is
#     @send.stellarindex.io while From: is @stellarindex.io, so aspf=s would
#     fail SPF alignment on every legitimate message. DKIM aligns strictly
#     either way.
set -uo pipefail

DOMAIN="${DOMAIN:-stellarindex.io}"
# Land the NS list before slicing it: `dig | sort | head -1` under `pipefail`
# SIGPIPEs sort and fails the script (scripts/ci/lint-shell-sigpipe.sh).
if [ -z "${NS:-}" ]; then
  ns_list="$(dig +short NS "$DOMAIN" | sort)"
  NS="${ns_list%%$'\n'*}"
fi
[ -z "$NS" ] && { echo "FATAL: no NS for $DOMAIN"; exit 99; }

FAILURES=0
pass() { printf '  \033[32mok\033[0m   %s\n' "$1"; }
fail() { printf '  \033[31mFAIL\033[0m %s\n' "$1"; FAILURES=$((FAILURES + 1)); }

q() { dig +short @"$NS" "$1" "$2" 2>/dev/null; }

# tag <record> <name>: the value of one `name=value` tag in a `;`-separated
# record, whitespace removed. Prints nothing when the tag is absent. The awk
# reads to the end: an early exit under pipefail SIGPIPEs the writers.
tag() {
  printf '%s' "$1" | tr ';' '\n' | tr -d ' \t' | awk -F= -v k="$2" '$1 == k && !seen { seen = 1; sub(/^[^=]*=/, ""); print }'
}

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
  # Assert the destination: a bare count passes an MX pointing at a host that
  # accepts nothing.
  mx_one_line="$(printf '%s\n' "$mx" | tr '\n' ' ')"
  case "$(printf '%s' "$mx_one_line" | tr '[:upper:]' '[:lower:]')" in
    *google.com*) pass "apex MX points at Google Workspace" ;;
    *)            fail "apex MX does not point at Google Workspace: $mx_one_line" ;;
  esac
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
  # The first `all` decides every sender no earlier mechanism matched, so it
  # must be the only one, the last term, and a hard fail.
  spf_alls="$(printf '%s\n' "$spf" | tr ' ' '\n' | grep -E '^[-+~?]?all$' | tr '\n' ' ')"
  case "$spf" in
    *" -all") if [ "$spf_alls" = "-all " ]; then
                pass "apex SPF ends in -all (hard fail)"
              else
                fail "apex SPF has an earlier all mechanism (${spf_alls% }) that overrides -all: $spf"
              fi ;;
    *)        fail "apex SPF does not end in -all: $spf" ;;
  esac
  # Resend rides SES; Google Workspace rides its own include.
  for inc in "include:amazonses.com" "include:_spf.google.com"; do
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
  case "$(tag "$dmarc" p)" in
    quarantine|reject) pass "DMARC policy is enforcing" ;;
    none) fail "DMARC p=none — reporting only, nothing is rejected" ;;
    "")   fail "DMARC has no p= tag" ;;
    *)    fail "DMARC p=$(tag "$dmarc" p) is not a policy" ;;
  esac
  # pct below 100 applies the policy to that share of mail only; pct=0 is none.
  case "$(tag "$dmarc" pct)" in
    ""|100) pass "DMARC policy applies to all mail" ;;
    *)      fail "DMARC pct=$(tag "$dmarc" pct) — the policy is applied to only part of the mail" ;;
  esac
  if [ -n "$(tag "$dmarc" rua)" ]; then
    pass "DMARC has an aggregate-report address"
  else
    fail "DMARC has no rua= — policy cannot be tuned from evidence"
  fi
  # See the header note: strict alignment breaks Resend's subdomain Return-Path.
  if [ "$(tag "$dmarc" aspf)" = "s" ]; then
    fail "DMARC aspf=s — breaks SPF alignment for send.$DOMAIN Return-Path"
  else
    pass "DMARC SPF alignment is relaxed (correct for this topology)"
  fi
fi

# --- 4. DKIM -----------------------------------------------------------------
echo "dkim"
# dig splits a long key over several quoted strings; join them before reading
# p=. An empty p= is the revocation form (RFC 6376 3.6.1), and the shortest
# real key (Ed25519) is 44 base64 characters.
for sel in resend google; do
  dkim="$(q TXT "${sel}._domainkey.$DOMAIN" | tr -d '" \n')"
  dkim_key="$(tag "$dkim" p)"
  if [ -z "$dkim" ]; then
    fail "DKIM selector ${sel}: absent"
  elif [ -z "$dkim_key" ]; then
    fail "DKIM selector ${sel}: p= is empty — the key is revoked, every signature fails"
  elif grep -Eq '^[A-Za-z0-9+/]{43,}=*$' <<<"$dkim_key"; then
    pass "DKIM selector ${sel}: published"
  else
    fail "DKIM selector ${sel}: p= is not a public key"
  fi
done

# --- 5. CAA ------------------------------------------------------------------
echo "caa"
caa="$(q CAA "$DOMAIN")"
if [ -z "$caa" ]; then
  fail "CAA: absent — any public CA may issue for $DOMAIN"
else
  pass "CAA: $(printf '%s\n' "$caa" | grep -c .) record(s)"
  # Losing letsencrypt.org takes api.$DOMAIN TLS-dark at the next renewal.
  if grep -q 'letsencrypt.org' <<<"$caa"; then
    pass "CAA allows letsencrypt.org (r1 Caddy renewal)"
  else
    fail "CAA omits letsencrypt.org — r1 Caddy renewal WILL fail"
  fi
  if grep -q 'pki.goog' <<<"$caa"; then
    pass "CAA allows pki.goog (Cloudflare Universal SSL)"
  else
    fail "CAA omits pki.goog — Cloudflare Pages cert renewal may fail"
  fi
fi

# --- 6. DNSSEC (informational: the DS lives at the registrar) ----------------
echo "dnssec"
if grep -q . <<<"$(dig +short DS "$DOMAIN" @1.1.1.1)"; then
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
