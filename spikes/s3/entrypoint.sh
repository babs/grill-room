#!/bin/sh
# Seeds generated files into a fresh HOME; never overwrites what a previous run left.
set -eu
if [ -d /seed ]; then
    mkdir -p "$HOME/.claude"
    [ -f /seed/.credentials.json ] && [ ! -f "$HOME/.claude/.credentials.json" ] && cp /seed/.credentials.json "$HOME/.claude/.credentials.json"
    [ -f /seed/.claude.json ] && [ ! -f "$HOME/.claude.json" ] && cp /seed/.claude.json "$HOME/.claude.json"
fi
exec "$@"
