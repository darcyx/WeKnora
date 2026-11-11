#!/usr/bin/env bash
# Compare with the last successful publication, so failed/cancelled pushes are retried.
set -euo pipefail

base=${1:-}
head=${2:-HEAD}
ui=false app=false docreader=false sandbox=false
changes=$(mktemp)
trap 'rm -f "$changes"' EXIT

# Include reverted changes too: an intervening failed run may have pushed them.
if [ -z "$base" ] || ! git merge-base --is-ancestor "$base" "$head" || \
    ! git log --format= --name-only --no-renames -m -z "$base..$head" > "$changes"; then
    ui=true app=true docreader=true sandbox=true
else
    while IFS= read -r -d '' file; do
        case "$file" in
            .dockerignore|VERSION|.github/workflows/docker-image.yml|scripts/docker-build-changes.sh|scripts/test-docker-build-changes.sh|scripts/get_version.sh)
                ui=true app=true docreader=true sandbox=true ;;
            frontend/*)
                ui=true ;;
            docreader/*|packages/*)
                app=true docreader=true ;;
            docker/Dockerfile.docreader)
                docreader=true ;;
            docker/Dockerfile.sandbox|docker/sandbox-*|docker/desktop/*|docker/scripts/*)
                sandbox=true ;;
            .github/*)
                ;;
            *)
                # The app's COPY . . consumes the root context; keep unknown inputs safe.
                app=true ;;
        esac
    done < "$changes"
fi

printf 'ui=%s\napp=%s\ndocreader=%s\nsandbox=%s\n' "$ui" "$app" "$docreader" "$sandbox"
