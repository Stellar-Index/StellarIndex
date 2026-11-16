#!/usr/bin/env bash
# dns-perimeter-check-test.sh — drives dns-perimeter-check.sh against a stub
# `dig` so each record shape it exists to catch is proven to fail it.
#
#   bash scripts/ops/dns-perimeter-check-test.sh
set -uo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

mkdir -p "$work/bin"
# The stub answers `dig +short [@ns] TYPE NAME` from $FIXTURES/TYPE_NAME.
cat > "$work/bin/dig" <<'STUB'
#!/usr/bin/env bash
args=()
for a in "$@"; do
  case "$a" in +*|@*) ;; *) args+=("$a") ;; esac
done
f="$FIXTURES/${args[0]}_${args[1]}"
[ -f "$f" ] && cat "$f"
exit 0
STUB
chmod +x "$work/bin/dig"

D=example.test
# Key-shaped, not a key: a secret scanner must not read this fixture as one.
KEY="$(printf 'AbCd%.0s' 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20)"

# good <dir>: a perimeter the check must pass.
good() {
  mkdir -p "$1"
  printf 'ns1.example.test.\n' > "$1/NS_$D"
  printf '1 aspmx.l.google.com.\n' > "$1/MX_$D"
  printf '"v=spf1 include:amazonses.com include:_spf.google.com -all"\n"unrelated-verification=abc"\n' > "$1/TXT_$D"
  printf '"v=spf1 include:amazonses.com ~all"\n' > "$1/TXT_send.$D"
  printf '"v=DMARC1; p=quarantine; rua=mailto:dmarc@example.test; fo=1; pct=100"\n' > "$1/TXT__dmarc.$D"
  printf '"v=DKIM1; k=rsa; p=%s" "%s"\n' "$KEY" "$KEY" > "$1/TXT_resend._domainkey.$D"
  printf '"v=DKIM1; k=rsa; p=%s"\n' "$KEY" > "$1/TXT_google._domainkey.$D"
  printf '0 issue "letsencrypt.org"\n0 issue "pki.goog"\n' > "$1/CAA_$D"
  printf '12345 13 2 ABCDEF\n' > "$1/DS_$D"
}

failures=0
n=0
# check <name> <want-rc> <want-text> : runs the script over $work/<name>.
check() {
  local name="$1" want_rc="$2" want_text="$3" out rc
  n=$((n + 1))
  out="$(FIXTURES="$work/$name" PATH="$work/bin:$PATH" DOMAIN="$D" NS='' bash "$here/dns-perimeter-check.sh" 2>&1)"
  rc=$?
  if [ "$rc" -ne "$want_rc" ]; then
    echo "FAIL $name: exit $rc, want $want_rc"
    printf '%s\n' "$out" | sed 's/^/    /'
    failures=$((failures + 1))
  elif ! grep -qF -- "$want_text" <<<"$out"; then
    echo "FAIL $name: output lacks: $want_text"
    printf '%s\n' "$out" | sed 's/^/    /'
    failures=$((failures + 1))
  else
    echo "ok   $name"
  fi
}

good "$work/good"
check good 0 "ALL CHECKS PASSED"

good "$work/dmarc_p_none_sp_reject"
printf '"v=DMARC1; p=none; sp=reject; rua=mailto:x@example.test"\n' > "$work/dmarc_p_none_sp_reject/TXT__dmarc.$D"
check dmarc_p_none_sp_reject 1 "DMARC p=none"

good "$work/dmarc_pct_0"
printf '"v=DMARC1; p=reject; pct=0; rua=mailto:x@example.test"\n' > "$work/dmarc_pct_0/TXT__dmarc.$D"
check dmarc_pct_0 1 "DMARC pct=0"

good "$work/dmarc_no_policy"
printf '"v=DMARC1; sp=reject; rua=mailto:x@example.test"\n' > "$work/dmarc_no_policy/TXT__dmarc.$D"
check dmarc_no_policy 1 "DMARC has no p= tag"

good "$work/dmarc_rua_in_other_tag"
printf '"v=DMARC1; p=reject; ruf=mailto:rua=@example.test"\n' > "$work/dmarc_rua_in_other_tag/TXT__dmarc.$D"
check dmarc_rua_in_other_tag 1 "DMARC has no rua="

good "$work/dmarc_strict_spf"
printf '"v=DMARC1; p=reject; aspf=s; rua=mailto:x@example.test"\n' > "$work/dmarc_strict_spf/TXT__dmarc.$D"
check dmarc_strict_spf 1 "DMARC aspf=s"

good "$work/dkim_revoked"
printf '"v=DKIM1; k=rsa; p="\n' > "$work/dkim_revoked/TXT_resend._domainkey.$D"
check dkim_revoked 1 "DKIM selector resend: p= is empty"

good "$work/dkim_absent"
rm "$work/dkim_absent/TXT_google._domainkey.$D"
check dkim_absent 1 "DKIM selector google: absent"

good "$work/dkim_not_a_key"
printf '"v=DKIM1; k=rsa; p=placeholder"\n' > "$work/dkim_not_a_key/TXT_google._domainkey.$D"
check dkim_not_a_key 1 "DKIM selector google: p= is not a public key"

good "$work/spf_plus_all"
printf '"v=spf1 +all include:amazonses.com include:_spf.google.com -all"\n' > "$work/spf_plus_all/TXT_$D"
check spf_plus_all 1 "earlier all mechanism (+all -all)"

good "$work/spf_soft_fail"
printf '"v=spf1 include:amazonses.com include:_spf.google.com ~all"\n' > "$work/spf_soft_fail/TXT_$D"
check spf_soft_fail 1 "apex SPF does not end in -all"

good "$work/spf_two_records"
printf '"v=spf1 include:amazonses.com include:_spf.google.com -all"\n"v=spf1 -all"\n' > "$work/spf_two_records/TXT_$D"
check spf_two_records 1 "apex SPF: 2 records"

echo
if [ "$failures" -ne 0 ]; then
  echo "$failures of $n dns-perimeter-check case(s) failed"
  exit 1
fi
echo "dns-perimeter-check-test: $n case(s) passed"
