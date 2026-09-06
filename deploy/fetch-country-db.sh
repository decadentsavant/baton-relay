#!/usr/bin/env bash
# Fetch the public-domain IP-to-country range lists the relay can load with
# -country-db. Source: the "user-country" files from
# https://github.com/sapics/ip-location-db (PDDL, updated daily). Each file is
# start,end,country per line; the relay keeps them in memory and binary-searches
# them. Country is the only thing that ever leaves that lookup.
#
#   fetch-country-db.sh [directory]      default: current directory
#
# Run it again whenever you like (a monthly cron is plenty); files are swapped
# atomically, and the relay picks them up on its next restart.
set -euo pipefail

dir="${1:-.}"
base="https://github.com/sapics/ip-location-db/releases/download/latest"
mkdir -p "$dir"
for family in ipv4 ipv6; do
  tmp="$dir/country-$family.csv.tmp"
  curl -fsSL --retry 3 -o "$tmp" "$base/user-country-$family.csv"
  # A sane file has hundreds of thousands of rows; refuse anything that looks
  # like an error page.
  if [[ $(wc -l <"$tmp") -lt 1000 ]]; then
    echo "fetch-country-db: $family list looks wrong, keeping the old one" >&2
    rm -f "$tmp"
    exit 1
  fi
  mv "$tmp" "$dir/country-$family.csv"
done
echo "country lists written to $dir (restart the relay to load them)"
