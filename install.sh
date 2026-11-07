#!/bin/bash
# Builds the client and copies it to ~/bin.
set -e
cd "$(dirname "$0")"
go build -o actor-codex . && mkdir -p ~/bin && cp actor-codex ~/bin/
