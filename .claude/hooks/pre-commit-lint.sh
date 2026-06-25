#!/usr/bin/env bash
set -euo pipefail

# Pre-commit lint hook for Claude Code
# Runs golangci-lint --fix on staged Go files before git commit.
# Auto-stages files that were auto-fixed by the linter.

# Read tool input from stdin
INPUT=$(cat)

# Only run for git commit commands
COMMAND=$(echo "$INPUT" | jq -r '.tool_input.command // empty' 2>/dev/null || true)
if [[ -z "$COMMAND" ]] || ! echo "$COMMAND" | grep -qE '^git commit'; then
    exit 0
fi

# Get staged .go files (only in root module — exclude sub-modules like cdk/)
STAGED_FILES=$(git diff --cached --name-only --diff-filter=ACM -- '*.go' 2>/dev/null || true)
if [[ -z "$STAGED_FILES" ]]; then
    exit 0
fi

# Filter out files in sub-modules (directories with their own go.mod)
ROOT_STAGED_FILES=""
while IFS= read -r file; do
    dir=$(dirname "$file")
    in_submodule=false
    while [[ "$dir" != "." ]]; do
        if [[ -f "$dir/go.mod" ]]; then
            in_submodule=true
            break
        fi
        dir=$(dirname "$dir")
    done
    if [[ "$in_submodule" == "false" ]]; then
        ROOT_STAGED_FILES="${ROOT_STAGED_FILES:+$ROOT_STAGED_FILES
}$file"
    fi
done <<< "$STAGED_FILES"

if [[ -z "$ROOT_STAGED_FILES" ]]; then
    exit 0
fi
STAGED_FILES="$ROOT_STAGED_FILES"

# Find unique package directories
PACKAGES=$(echo "$STAGED_FILES" | xargs -I{} dirname {} | sort -u | sed 's|$|/...|')

# Snapshot checksums before lint
declare -A CHECKSUMS
while IFS= read -r file; do
    if [[ -f "$file" ]]; then
        CHECKSUMS["$file"]=$(md5sum "$file" | awk '{print $1}')
    fi
done <<< "$STAGED_FILES"

# Run linter with auto-fix
LINT_EXIT=0
go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2 run --fix $PACKAGES 2>&1 || LINT_EXIT=$?

# Re-stage any auto-fixed files
RESTAGED=0
while IFS= read -r file; do
    if [[ -f "$file" ]]; then
        NEW_CHECKSUM=$(md5sum "$file" | awk '{print $1}')
        if [[ "${CHECKSUMS[$file]:-}" != "$NEW_CHECKSUM" ]]; then
            git add "$file"
            RESTAGED=$((RESTAGED + 1))
        fi
    fi
done <<< "$STAGED_FILES"

if [[ $RESTAGED -gt 0 ]]; then
    echo "Auto-fixed and re-staged $RESTAGED file(s)"
fi

if [[ $LINT_EXIT -ne 0 ]]; then
    echo "Lint errors found. Fix them before committing."
    exit 2
fi

exit 0
