#!/usr/bin/env bash
# Exercise archive packaging with a real ELF object, without downloading Rust crates.
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
fixture=$(mktemp -d)
trap 'rm -rf "$fixture"' EXIT

case "$(go env GOOS)/$(go env GOARCH)" in
  linux/arm64) target=aarch64-unknown-linux-gnu; lib_dir=linux_arm64_gnu ;;
  linux/amd64) target=x86_64-unknown-linux-gnu; lib_dir=linux_amd64_gnu ;;
  *) echo 'Run this test on Linux amd64 or arm64.' >&2; exit 1 ;;
esac

crate_dir="$fixture/third_party/anydoc-go"
mkdir -p "$fixture/scripts" "$fixture/bin" "$fixture/native" "$crate_dir/patched-anydoc" \
  "$crate_dir/target/$target/release"
cp "$repo_root/scripts/build-anydoc-lib.sh" "$fixture/scripts/"
printf 'anydoc = "=0.0.1"\n' > "$crate_dir/Cargo.toml"
printf 'const Version = "0.0.1"\n' > "$crate_dir/version.go"
printf '0.0.1-zip9-v1\n' > "$crate_dir/patched-anydoc/.weknora-patched"
touch "$crate_dir/patched-anydoc/Cargo.toml"
printf '#!/bin/sh\nexit 0\n' > "$fixture/bin/cargo"
chmod +x "$fixture/bin/cargo"

printf 'int anydoc_fixture(void) { return 42; }\n' > "$fixture/native/native.c"
cc -c "$fixture/native/native.c" -o "$fixture/native/native.o"
printf 'embedded LLVM bitcode\n' > "$fixture/bitcode"
objcopy --add-section ".llvmbc=$fixture/bitcode" \
  --add-section ".llvmcmd=$fixture/bitcode" "$fixture/native/native.o"
ar rcs "$crate_dir/target/$target/release/libanydoc_go.a" "$fixture/native/native.o"

printf 'module example.test/anydoc-archive\n\ngo 1.26.0\n' > "$fixture/go.mod"
cat > "$fixture/main.go" <<EOF
package main
/*
#cgo LDFLAGS: \${SRCDIR}/third_party/anydoc-go/lib/$lib_dir/libanydoc_go.a
int anydoc_fixture(void);
*/
import "C"
func main() {
    if C.anydoc_fixture() != 42 { panic("native code changed during packaging") }
}
EOF

mkdir -p "$crate_dir/lib/$lib_dir"
cp "$crate_dir/target/$target/release/libanydoc_go.a" "$crate_dir/lib/$lib_dir/"
# Older Go patch releases accept this archive; newer ones reject its LTO section.
if ! (cd "$fixture" && GOWORK=off CGO_ENABLED=1 go build -o raw-client .) > "$fixture/raw.log" 2>&1; then
  grep -q 'disallowed LTO section' "$fixture/raw.log"
  echo 'Go rejected the unprocessed archive with embedded LLVM bitcode'
fi

PATH="$fixture/bin:$PATH" TARGET="$target" bash "$fixture/scripts/build-anydoc-lib.sh"
archive="$crate_dir/lib/$lib_dir/libanydoc_go.a"
objdump -h "$archive" > "$fixture/sections"
if grep -Eq '\.llvmbc|\.llvmcmd' "$fixture/sections"; then
  echo 'Packaged archive still contains embedded LLVM bitcode.' >&2
  exit 1
fi
(cd "$fixture" && GOWORK=off CGO_ENABLED=1 go run .)
echo "Anydoc archive packaging test passed for $target"
