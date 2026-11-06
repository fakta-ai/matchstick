#!/usr/bin/env bash
# The release workflow's decisions (.github/workflows/release.yml), against a fake `gh` that answers from fixtures:
# which version CHANGELOG.md asks to release and whether it is released already (job version), taking the tag (job
# tag), and waiting for the end-to-end status (job e2e). Runs each step as the workflow has it. Needs bash, jq and yq
# (mikefarah's), as GitHub's Ubuntu runners and the dev shell have.
set -euo pipefail
root=$(cd "$(dirname "$0")/../.." && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
workflow=$root/.github/workflows/release.yml
version_step=$(yq -r '.jobs.version.steps[] | select(.id == "version") | .run' "$workflow")
tag_step=$(yq -r '.jobs.tag.steps[] | select(.id == "tag") | .run' "$workflow")
# without the pauses, and in seconds rather than minutes
wait_step=$(yq -r '.jobs.e2e.steps[] | select(.id == "wait") | .run' "$workflow" | sed 's/sleep 30/sleep 0.1/; s/TIMEOUT \* 60/TIMEOUT * 1/')

mkdir -p "$work/bin"
cat > "$work/bin/gh" <<'EOF'
#!/usr/bin/env bash
# answers from $FIXTURE: refs.json ([{ref, object: {type, sha}}]), tags.json ({tag object sha: commit}),
# released (tag names, one per line), status.<n> (the nth combined status read), release-error (a releases API failure)
set -euo pipefail
[ "$1" = api ] || exit 2
shift
filter=. method=GET fields=()
while [ $# -gt 0 ]; do
  case $1 in
    --paginate | --silent) ;;
    --jq) filter=$2; shift ;;
    -X) method=$2; shift ;;
    -f) fields+=("$2"); shift ;;
    *) path=$1 ;;
  esac
  shift
done
notfound() { echo 'gh: Not Found (HTTP 404)' >&2; exit 1; }
case $method:$path in
  GET:repos/owner/repo/git/matching-refs/tags/v)
    jq -r "$filter" "$FIXTURE/refs.json" ;;
  GET:repos/owner/repo/git/ref/tags/*)
    ref=$(jq --arg ref "refs/tags/${path##*/}" '.[] | select(.ref == $ref)' "$FIXTURE/refs.json")
    [ -n "$ref" ] || notfound
    jq -r "$filter" <<< "$ref" ;;
  GET:repos/owner/repo/git/tags/*)
    jq --arg sha "${path##*/}" '{object: {sha: .[$sha]}}' "$FIXTURE/tags.json" | jq -r "$filter" ;;
  GET:repos/owner/repo/releases/tags/*)
    if [ -e "$FIXTURE/release-error" ]; then echo 'gh: Server Error (HTTP 500)' >&2; exit 1; fi
    grep -qxF "${path##*/}" "$FIXTURE/released" 2> /dev/null || notfound
    jq -nr --arg tag "${path##*/}" '{html_url: ("https://example.com/" + $tag)}' | jq -r "$filter" ;;
  POST:repos/owner/repo/git/refs)
    ref=${fields[0]#ref=} sha=${fields[1]#sha=}
    if [ -e "$FIXTURE/refuse" ]; then echo 'gh: Resource not accessible by integration (HTTP 403)' >&2; exit 1; fi
    if jq -e --arg ref "$ref" 'any(.[]; .ref == $ref)' "$FIXTURE/refs.json" > /dev/null; then
      echo 'gh: Reference already exists (HTTP 422)' >&2; exit 1
    fi
    jq --arg ref "$ref" --arg sha "$sha" '. + [{ref: $ref, object: {type: "commit", sha: $sha}}]' "$FIXTURE/refs.json" \
      > "$FIXTURE/refs.new" && mv "$FIXTURE/refs.new" "$FIXTURE/refs.json"
    echo "$ref $sha" >> "$FIXTURE/created" ;;
  GET:repos/owner/repo/commits/*/status*)
    n=$(cat "$FIXTURE/reads" 2> /dev/null || echo 0)
    echo $((n + 1)) > "$FIXTURE/reads"
    file=$FIXTURE/status.$n
    [ -e "$file" ] || file=$(ls "$FIXTURE"/status.* | sort -t. -k2 -n | tail -n 1)
    jq -r "$filter" "$file" ;;
  *) echo "unexpected: $method $path" >&2; exit 1 ;;
esac
EOF
chmod +x "$work/bin/gh"

this=1111111111111111111111111111111111111111
other=2222222222222222222222222222222222222222
fixture() { # a fresh fixture with no tags and no releases; prints its directory
  local dir
  dir=$(mktemp -d "$work/case.XXXXXX")
  echo '[]' > "$dir/refs.json"
  echo '{}' > "$dir/tags.json"
  echo "$dir"
}
tag() { # fixture, tag, commit, [annotated tag object]
  local object=${4:-}
  if [ -n "$object" ]; then
    jq --arg ref "refs/tags/$2" --arg sha "$object" '. + [{ref: $ref, object: {type: "tag", sha: $sha}}]' "$1/refs.json" \
      > "$1/x" && mv "$1/x" "$1/refs.json"
    jq --arg object "$object" --arg sha "$3" '. + {($object): $sha}' "$1/tags.json" > "$1/x" && mv "$1/x" "$1/tags.json"
  else
    jq --arg ref "refs/tags/$2" --arg sha "$3" '. + [{ref: $ref, object: {type: "commit", sha: $sha}}]' "$1/refs.json" \
      > "$1/x" && mv "$1/x" "$1/refs.json"
  fi
}
changelog() { # fixture, CHANGELOG.md's text
  printf '%s\n' "$2" > "$1/CHANGELOG.md"
}
run() { # fixture, step, env...: runs the step in the fixture; prints its outputs on one line, or "failed"
  local dir=$1 step=$2
  shift 2
  : > "$dir/output"
  if (cd "$dir" && env FIXTURE="$dir" PATH="$work/bin:$PATH" GH_TOKEN=fixture REPO=owner/repo SHA=$this \
    GITHUB_OUTPUT="$dir/output" "$@" bash -c "$step" > "$dir/log" 2>&1); then
    grep -E '^(release|version|ok)=' "$dir/output" | tr '\n' ' ' | sed 's/ $//'
  else
    echo failed
  fi
}
failures=0
expect() { # name, wanted, got
  if [ "$2" = "$3" ]; then
    echo "PASS: $1"
  else
    echo "FAIL: $1: got '$3', want '$2'" >&2
    failures=$((failures + 1))
  fi
}

released='# Changelog

## [Unreleased]

## [0.3.1] - 2026-11-06

### Fixed

- a fix

## [0.3.0] - 2026-11-06

- older'
new='# Changelog

## [Unreleased]

- next

## [0.4.0] - 2026-11-07

### Added

- a feature

## [0.3.1] - 2026-11-06

- a fix'

# version
d=$(fixture); changelog "$d" '# Changelog

## [Unreleased]

- next'
expect 'no version heading: nothing to release' 'release=false' "$(run "$d" "$version_step")"

d=$(fixture); changelog "$d" "$new"; tag "$d" v0.3.1 "$other"
expect 'a new version above the latest tag is released' 'release=true version=0.4.0' "$(run "$d" "$version_step")"
expect "its notes are the version's section" true \
  "$(grep -q '^- a feature$' "$d/output" && ! grep -q 'a fix' "$d/output" && echo true)"

d=$(fixture); changelog "$d" "${new/'## [0.4.0] - 2026-11-07'/'## [0.4.0]'}"; tag "$d" v0.3.1 "$other"
expect 'a heading without a date is released' 'release=true version=0.4.0' "$(run "$d" "$version_step")"

d=$(fixture); changelog "$d" "$new"
expect 'the first version, with no tags yet, is released' 'release=true version=0.4.0' "$(run "$d" "$version_step")"

d=$(fixture); changelog "$d" "${new/'## [0.4.0]'/'## [0.4]'}"; tag "$d" v0.3.1 "$other"
expect 'a malformed version heading fails' failed "$(run "$d" "$version_step")"

d=$(fixture); changelog "$d" "${new/'## [0.4.0]'/'## [v0.4.0]'}"; tag "$d" v0.3.1 "$other"
expect "a heading with a 'v' fails" failed "$(run "$d" "$version_step")"

d=$(fixture); changelog "$d" "${new/'## [0.4.0]'/'## [0.2.5]'}"; tag "$d" v0.3.1 "$other"
expect 'a version below the latest tag fails' failed "$(run "$d" "$version_step")"

d=$(fixture); changelog "$d" "$released"; tag "$d" v0.3.0 "$other" 3333333333333333333333333333333333333333
tag "$d" v0.3.1 "$other" 4444444444444444444444444444444444444444
expect 'the newest version tagged on an earlier commit (annotated): nothing to release' 'release=false version=0.3.1' \
  "$(run "$d" "$version_step")"

d=$(fixture); changelog "$d" "$new"; tag "$d" v0.3.1 "$other"; tag "$d" v0.4.0 "$other"
expect 'the newest version tagged on an earlier commit: nothing to release' 'release=false version=0.4.0' \
  "$(run "$d" "$version_step")"

d=$(fixture); changelog "$d" "$new"; tag "$d" v0.4.0 "$this"; echo v0.4.0 > "$d/released"
expect 'tagged on this commit and released: nothing to do' 'release=false version=0.4.0' "$(run "$d" "$version_step")"

d=$(fixture); changelog "$d" "$new"; tag "$d" v0.4.0 "$this"
expect 'tagged on this commit, not released (a re-run): released' 'release=true version=0.4.0' \
  "$(run "$d" "$version_step")"

d=$(fixture); changelog "$d" "$new"; tag "$d" v0.4.0 "$this"; touch "$d/release-error"
expect "tagged on this commit, the releases API failing: fails" failed "$(run "$d" "$version_step")"

d=$(fixture); changelog "$d" '# Changelog

## [Unreleased]

## [0.4.0] - 2026-11-07

## [0.3.1] - 2026-11-06

- a fix'
tag "$d" v0.3.1 "$other"
expect 'an empty section fails' failed "$(run "$d" "$version_step")"

d=$(fixture); changelog "$d" '# Changelog

## [0.4.0] - 2026-11-07

- a feature'
expect 'without an Unreleased section, the first version heading counts' 'release=true version=0.4.0' \
  "$(run "$d" "$version_step")"

# tag
d=$(fixture)
expect 'an untagged version is tagged on this commit' ok=true "$(run "$d" "$tag_step" VERSION=0.4.0)"
expect '... with the tag pointing at this commit' "refs/tags/v0.4.0 $this" "$(cat "$d/created")"

d=$(fixture); tag "$d" v0.4.0 "$this"
expect 'a version already tagged on this commit (a re-run) goes on' ok=true "$(run "$d" "$tag_step" VERSION=0.4.0)"

d=$(fixture); tag "$d" v0.4.0 "$other"
expect 'a version another run tagged meanwhile stops quietly' ok=false "$(run "$d" "$tag_step" VERSION=0.4.0)"

d=$(fixture); touch "$d/refuse"
expect 'a refused tag fails' failed "$(run "$d" "$tag_step" VERSION=0.4.0)"

# e2e
status() { # fixture, n, state or "" for none
  if [ -n "$3" ]; then
    jq -n --arg state "$3" '{statuses: [{context: "other", state: "failure"}, {context: "e2e", state: $state}]}'
  else
    echo '{"statuses": [{"context": "other", "state": "success"}]}'
  fi > "$1/status.$2"
}
d=$(fixture)
expect 'no context set: no wait' '' "$(run "$d" "$wait_step" CONTEXT= TIMEOUT=30)"

d=$(fixture); status "$d" 0 success
expect 'the status succeeded: go on' '' "$(run "$d" "$wait_step" CONTEXT=e2e TIMEOUT=30)"

d=$(fixture); status "$d" 0 ''; status "$d" 1 pending; status "$d" 2 success
expect 'none, then pending, then success: go on' '' "$(run "$d" "$wait_step" CONTEXT=e2e TIMEOUT=30)"

d=$(fixture); status "$d" 0 pending; status "$d" 1 failure
expect 'pending, then failure: fails' failed "$(run "$d" "$wait_step" CONTEXT=e2e TIMEOUT=30)"

d=$(fixture); status "$d" 0 error
expect 'error: fails' failed "$(run "$d" "$wait_step" CONTEXT=e2e TIMEOUT=30)"

d=$(fixture); status "$d" 0 pending
expect 'pending until the timeout: fails' failed "$(run "$d" "$wait_step" CONTEXT=e2e TIMEOUT=1)"

d=$(fixture); status "$d" 0 success
expect 'a timeout that is not minutes: fails' failed "$(run "$d" "$wait_step" CONTEXT=e2e TIMEOUT=10m)"

[ "$failures" -eq 0 ]
