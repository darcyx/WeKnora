#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
repo=$(mktemp -d)
trap 'rm -rf "$repo"' EXIT
cd "$repo"
git init -q
git config user.email ci@example.com
git config user.name CI
git commit -qm initial --allow-empty
base=$(git rev-parse HEAD)

check() {
    local expected=$1
    shift
    git read-tree --empty
    for file in "$@"; do
        mkdir -p "$(dirname "$file")"
        printf 'test\n' > "$file"
        git add -f -- "$file"
    done
    local head actual
    head=$(git commit-tree "$(git write-tree)" -p "$base" -m fixture)
    actual=$(bash "$script_dir/docker-build-changes.sh" "$base" "$head")
    if [ "$actual" != "$expected" ]; then
        printf 'Unexpected selection for %s:\n%s\n' "$*" "$actual" >&2
        exit 1
    fi
}

none=$'ui=false\napp=false\ndocreader=false\nsandbox=false'
all=$'ui=true\napp=true\ndocreader=true\nsandbox=true'
check "$none"
check $'ui=true\napp=false\ndocreader=false\nsandbox=false' 'frontend/src/file with spaces.vue'
check $'ui=false\napp=true\ndocreader=false\nsandbox=false' internal/server.go
check $'ui=true\napp=true\ndocreader=false\nsandbox=false' frontend/src/App.vue internal/server.go
check $'ui=false\napp=true\ndocreader=true\nsandbox=false' docreader/main.py
check $'ui=false\napp=false\ndocreader=true\nsandbox=false' docker/Dockerfile.docreader
check $'ui=false\napp=false\ndocreader=false\nsandbox=true' docker/scripts/start-desktop.sh
check "$all" .dockerignore
check "$all" .github/workflows/docker-image.yml
check "$none" .github/workflows/cli.yml

# Missing/unavailable publication baselines (including tags) must build everything.
test "$(bash "$script_dir/docker-build-changes.sh")" = "$all"
test "$(bash "$script_dir/docker-build-changes.sh" missing HEAD 2>/dev/null)" = "$all"

# Deletions and renames must include both the old and new build inputs.
mkdir -p frontend
printf 'old\n' > frontend/old.vue
git add frontend/old.vue
git commit -qm old
old=$(git rev-parse HEAD)
git rm -q frontend/old.vue
git commit -qm deleted
test "$(bash "$script_dir/docker-build-changes.sh" "$old" HEAD)" = $'ui=true\napp=false\ndocreader=false\nsandbox=false'

printf 'old\n' > frontend/old.vue
git add frontend/old.vue
git commit -qm restored
old=$(git rev-parse HEAD)
mkdir -p internal
git mv frontend/old.vue internal/renamed.go
git commit -qm renamed
test "$(bash "$script_dir/docker-build-changes.sh" "$old" HEAD)" = $'ui=true\napp=true\ndocreader=false\nsandbox=false'

# A failed run may publish the UI before failing the app. Even if a later
# commit reverts the UI, it must publish again to restore the successful state.
old=$(git rev-parse HEAD)
printf 'changed\n' > frontend/changed.vue
git add frontend/changed.vue
git commit -qm partial-publication
git rm -q frontend/changed.vue
printf 'fixed\n' > internal/renamed.go
git add internal/renamed.go
git commit -qm revert-and-fix
test "$(bash "$script_dir/docker-build-changes.sh" "$old" HEAD)" = $'ui=true\napp=true\ndocreader=false\nsandbox=false'

# A force push outside the successful history needs a full publication.
unrelated=$(git commit-tree "$(git write-tree)" -m unrelated)
test "$(bash "$script_dir/docker-build-changes.sh" "$unrelated" HEAD)" = "$all"

echo 'Docker build selection checks passed'
