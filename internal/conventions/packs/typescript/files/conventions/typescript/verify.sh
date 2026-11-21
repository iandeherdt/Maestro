#!/bin/sh
# Every check that decides whether a change is acceptable. Needs no network
# after prepare.sh. The Symphony service runs this itself before it pushes
# anything, and CI runs the same script.
set -eu

# The share of lines the tests have to cover, in percent.
COVERAGE_MIN=80

cd "$(dirname "$0")/../.."
. conventions/typescript/env.sh

echo "== format =="
run prettier --check .

echo "== lint =="
run eslint . --max-warnings 0

echo "== types =="
if [ ! -f tsconfig.json ]; then
    echo "conventions: no tsconfig.json in the root of the repository." >&2
    exit 1
fi
run tsc --noEmit --strict --noUncheckedIndexedAccess

echo "== tests, coverage of at least ${COVERAGE_MIN}% =="
if ! uses vitest; then
    echo "conventions: this pack runs the tests with vitest. Add vitest and @vitest/coverage-v8 to devDependencies." >&2
    exit 1
fi
# Every source file counts, also one that no test imports.
run vitest run --passWithNoTests=false --coverage.enabled --coverage.reporter=text-summary \
    --coverage.reportsDirectory="$(mktemp -d)" --coverage.thresholds.lines="$COVERAGE_MIN" \
    --coverage.include='**/*.{ts,tsx}' --coverage.exclude='**/*.config.*' --coverage.exclude='**/*.d.ts' \
    --coverage.exclude='**/*.gen.ts' --coverage.exclude='**/*.{test,spec}.{ts,tsx}' --coverage.exclude='**/__tests__/**'
