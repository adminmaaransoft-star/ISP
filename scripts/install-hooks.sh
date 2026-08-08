#!/usr/bin/env bash
# Install git hooks from .githooks/ into .git/hooks/
set -euo pipefail
cp .githooks/pre-commit .git/hooks/pre-commit
chmod +x .git/hooks/pre-commit
echo "Git hooks installed from .githooks/"
