#!/bin/sh
# Regenerates THIRD_PARTY_NOTICES.md from the licences of everything linked
# into a released resticscope binary: the Go standard library plus every
# module contributing a package to ./cmd/resticscope on a released platform.
#
# Run it from anywhere after a dependency change, then commit the result:
#
#     ./scripts/third-party-notices.sh
#
# Test-only and Windows-only dependencies are excluded because they are not
# part of what gets distributed.
set -eu

# Byte collation, so the generated file is identical regardless of the
# maintainer's locale and a root LICENSE sorts ahead of nested ones.
export LC_ALL=C

cd "$(dirname "$0")/.."

module=$(go list -m)
goroot=$(go env GOROOT)
out=THIRD_PARTY_NOTICES.md
tmp=$(mktemp)
trap 'rm -f "$tmp"' EXIT

# One line of "path version dir" per module, unioned over the released
# platforms. GOARCH does not change the package set, so amd64 stands in for
# both architectures.
modules=$(
	for os in linux darwin; do
		GOOS="$os" GOARCH=amd64 go list -deps \
			-f '{{with .Module}}{{.Path}} {{.Version}} {{.Dir}}{{end}}' \
			./cmd/resticscope
	done | grep -v '^[[:space:]]*$' | grep -v "^$module " | sort -u
)

emit() { # name, path to licence file
	printf '## %s\n\n```text\n' "$1"
	# Trailing blank lines would push the fence away from the text.
	sed -e 's/\r$//' "$2" | sed -e :a -e '/^\n*$/{$d;N;ba' -e '}'
	printf '\n```\n\n'
}

{
	cat <<-EOF
		# Third-party notices

		resticscope is licensed under the GNU General Public License v3.0; see
		LICENSE. Released binaries are statically linked and therefore also
		contain the software listed below. These notices reproduce the copyright
		and permission notices that those licences require when the binary is
		redistributed.

		Module versions correspond to \`go.mod\` and \`go.sum\`. Regenerate this
		file with \`./scripts/third-party-notices.sh\` after changing
		dependencies.

	EOF

	emit "The Go standard library — $(go env GOVERSION)" "$goroot/LICENSE"

	echo "$modules" | while read -r path version dir; do
		[ -n "$path" ] || continue
		# A module may carry the same licence text in several places, e.g.
		# BurntSushi/toml repeats COPYING under each cmd. Emit each distinct
		# text once, but keep genuinely different nested licences.
		seen=""
		find "$dir" -type f \
			\( -iname 'LICENSE*' -o -iname 'LICENCE*' \
			-o -iname 'COPYING*' -o -iname 'NOTICE*' \) |
			sort | while read -r file; do
			case "$file" in
			*.go) continue ;;
			esac
			sum=$(cksum <"$file")
			case " $seen " in
			*" $sum "*) continue ;;
			esac
			seen="$seen $sum"
			emit "$path $version — ${file#"$dir"/}" "$file"
		done
	done
} >"$tmp"

# Collapse the trailing blank line left by the last entry.
sed -e :a -e '/^\n*$/{$d;N;ba' -e '}' "$tmp" >"$out"
printf 'wrote %s (%s modules)\n' "$out" "$(echo "$modules" | grep -c .)"
