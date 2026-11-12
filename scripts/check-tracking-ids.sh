#!/usr/bin/env bash
# Fail on planning IDs in code, tests and scripts: acceptance IDs, phase
# numbers, design row or section numbers, issue and PR numbers. They mean
# nothing to a later reader; say the reason instead.
set -euo pipefail

if git grep -nE 'AC-[0-9]+-[0-9]+|func TestAC|[Pp]hase [0-9]|row [0-9]{2,}|§ ?[0-9]|#[0-9]{2,}' -- \
	'*.go' '*.sh' 'Makefile' ':!scripts/check-tracking-ids.sh'; then
	echo "^ tracking ID in code: explain the reason instead (see CLAUDE.md)" >&2
	exit 1
fi
