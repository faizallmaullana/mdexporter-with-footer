#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"

install -D -m 0755 "$script_dir/build/mdexport" /usr/bin/mdexport
install -D -m 0644 "$script_dir/public/footer.png" /usr/src/footer
