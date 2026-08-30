#!/bin/bash
set -e
out=THIRD-PARTY-NOTICES.md
go list -deps -f '{{if not .Standard}}{{.Module.Path}}|{{.Module.Version}}|{{.Module.Dir}}{{end}}' ./... 2>/dev/null \
  | sort -u | grep -v '^github.com/Privasys/immutable-ledger|' > /tmp/deps_gen.txt

lic_kind() {
  if grep -qi "Apache License" "$1"; then echo "Apache-2.0"
  elif grep -qi "Mozilla Public License" "$1"; then echo "MPL-2.0"
  elif grep -qi "Permission is hereby granted, free of charge" "$1"; then echo "MIT"
  elif grep -qi "Redistribution and use in source and binary forms" "$1"; then echo "BSD"
  else echo "see text"; fi
}

{
  echo "# Third-party notices"
  echo
  echo "immutable-ledger's own code is licensed under the AGPL-3.0 (see"
  echo "[LICENSE](LICENSE)). Builds of this module link the third-party Go"
  echo "modules below, fetched from their upstream repositories; this"
  echo "repository redistributes none of their source. This file reproduces"
  echo "their licence and notice texts so that anyone distributing a"
  echo "compiled artefact can meet the retention requirements (BSD clause 2,"
  echo "Apache-2.0 section 4, MIT, MPL-2.0) by shipping this file with it."
  echo
  echo "Regenerate after dependency changes with"
  echo '`bash scripts/gen-third-party-notices.sh`.'
  echo
  echo "| Module | Version | Licence |"
  echo "| --- | --- | --- |"
  while IFS='|' read -r path ver dir; do
    f=$(ls "$dir"/LICENSE* "$dir"/COPYING* "$dir"/License 2>/dev/null | head -1)
    echo "| $path | $ver | $(lic_kind "$f") |"
  done < /tmp/deps_gen.txt
  echo
  while IFS='|' read -r path ver dir; do
    echo "---"
    echo
    echo "## $path $ver"
    echo
    for f in "$dir"/LICENSE* "$dir"/COPYING* "$dir"/License "$dir"/NOTICE* "$dir"/Notice; do
      [ -f "$f" ] || continue
      echo '```'
      tr -d '\r' < "$f"
      echo '```'
      echo
    done
  done < /tmp/deps_gen.txt
} > "$out"
echo "wrote $out: $(wc -l < "$out") lines"
