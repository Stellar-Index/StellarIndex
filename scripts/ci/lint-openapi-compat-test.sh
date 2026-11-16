#!/usr/bin/env bash
# lint-openapi-compat-test.sh — prove lint-openapi-compat.py goes red on each breaking change
# it names and stays green on additive ones. File mode only: no git fixture.
set -uo pipefail
cd "$(dirname "$0")/../.." || exit 1

GATE="scripts/ci/lint-openapi-compat.py"
PASS=0; FAIL=0
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

cat >"$TMP/base.yaml" <<'YAML'
paths:
  /price:
    get:
      parameters:
        - { name: asset, in: query, required: true, schema: { type: string } }
        - { name: mode, in: query, schema: { type: string, enum: [vwap, twap] } }
      responses:
        "200":
          content:
            application/json:
              schema: { $ref: "#/components/schemas/Price" }
  /keys:
    post:
      requestBody:
        content:
          application/json:
            schema: { $ref: "#/components/schemas/KeyRequest" }
      responses: { "201": { description: made } }
    delete:
      responses: { "204": { description: gone } }
components:
  schemas:
    Price:
      type: object
      required: [price]
      properties:
        price: { type: string }
        basis: { type: string, enum: [market, peg] }
    KeyRequest:
      type: object
      required: [name]
      properties:
        name: { type: string }
        scope: { $ref: "#/components/schemas/Scope" }
    Scope:
      type: object
      properties:
        level: { type: string }
YAML

check() { # <name> <ok|red> <needle> <python edit of the parsed spec `d`> [<commit messages>]
  local name="$1" want="$2" needle="$3" edit="$4" rc out
  printf '%s\n' "${5:-}" >"$TMP/messages"
  python3 - "$TMP/base.yaml" "$TMP/head.yaml" "$edit" <<'PY' || { printf '  FAIL %s (fixture edit)\n' "$name"; FAIL=$((FAIL+1)); return; }
import sys, yaml
d = yaml.safe_load(open(sys.argv[1]))
S = d["components"]["schemas"]
exec(sys.argv[3])
yaml.safe_dump(d, open(sys.argv[2], "w"))
PY
  out="$(python3 "$GATE" --base-file "$TMP/base.yaml" --head-file "$TMP/head.yaml" --messages-file "$TMP/messages" 2>&1)"; rc=$?
  if [ "$want" = ok ] && [ "$rc" -eq 0 ]; then
    printf '  ok   %s\n' "$name"; PASS=$((PASS+1))
  elif [ "$want" = red ] && [ "$rc" -ne 0 ] && grep -qF -- "$needle" <<<"$out"; then
    printf '  ok   %s\n' "$name"; PASS=$((PASS+1))
  else
    printf '  FAIL %s (rc=%s, wanted %s)\n%s\n' "$name" "$rc" "$want" "$out"; FAIL=$((FAIL+1))
  fi
}

# In an edit, S is the spec's components.schemas. The needles quote backticks literally.
# shellcheck disable=SC2016
{
check "an unchanged spec passes" ok "" 'pass'
check "a new path, optional parameter, property and enum value pass" ok "" \
  "d['paths']['/new'] = {'get': {}}; d['paths']['/price']['get']['parameters'].append({'name': 'x', 'in': 'query'}); S['Price']['properties']['extra'] = {'type': 'string'}; S['Price']['properties']['basis']['enum'].append('oracle')"
check "reordered parameters pass" ok "" "d['paths']['/price']['get']['parameters'].reverse()"
check "a newly required RESPONSE field passes" ok "" "S['Price']['required'].append('basis')"

check "a removed path is rejected" red "removed-operation: GET /price" "del d['paths']['/price']"
check "a removed method is rejected" red "removed-operation: DELETE /keys" "del d['paths']['/keys']['delete']"
check "a removed schema is rejected" red "removed-schema: components/schemas/Scope" \
  "del S['Scope']; del S['KeyRequest']['properties']['scope']"
check "a removed property is rejected" red 'removed-property: components/schemas/Price lost `price`' \
  "del S['Price']['properties']['price']"
check "a removed schema enum value is rejected" red 'lost `peg`' "S['Price']['properties']['basis']['enum'].remove('peg')"
check "a removed parameter enum value is rejected" red 'parameters/mode:query/schema lost `twap`' \
  "d['paths']['/price']['get']['parameters'][1]['schema']['enum'].remove('twap')"
check "a new required parameter is rejected" red "now requires parameter quote (in query)" \
  "d['paths']['/price']['get']['parameters'].append({'name': 'quote', 'in': 'query', 'required': True})"
check "an optional parameter made required is rejected" red "now requires parameter mode (in query)" \
  "d['paths']['/price']['get']['parameters'][1]['required'] = True"
check "a new required request field is rejected" red 'now requires `scope`' "S['KeyRequest']['required'].append('scope')"
check "a new required field in a nested request schema is rejected" red 'now requires `level`' "S['Scope']['required'] = ['level']"
check "a request body made required is rejected" red "now requires a request body" \
  "d['paths']['/keys']['post']['requestBody']['required'] = True"

check "a break declared by a trailer passes" ok "" "del d['paths']['/price']" \
  "$(printf 'api: drop /price\n\nOpenAPI-Break: replaced by /v2/price\n')"
check "a trailer with no reason declares nothing" red "removed-operation: GET /price" "del d['paths']['/price']" \
  "$(printf 'api: drop /price\n\nOpenAPI-Break:\n')"
check "a trailer mentioned mid-line declares nothing" red "removed-operation: GET /price" "del d['paths']['/price']" \
  "see the OpenAPI-Break: rule"
}

printf 'lint-openapi-compat-test: %d passed, %d failed\n' "$PASS" "$FAIL"
[ "$FAIL" -eq 0 ]
