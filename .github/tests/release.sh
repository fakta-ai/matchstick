#!/usr/bin/env bash
# The release workflow's decisions (.github/workflows/release.yml), against a fake `gh` that answers from fixtures:
# which version CHANGELOG.md asks to release and whether it is released already (job version), taking the tag (job
# tag), the image the chart runs (job chart) and waiting for the end-to-end status and its acknowledgement (job e2e).
# Runs each step as the workflow has it. Needs bash, jq, yq (mikefarah's) and helm, as GitHub's Ubuntu runners (with
# setup-helm) and the dev shell have.
set -euo pipefail
root=$(cd "$(dirname "$0")/../.." && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
workflow=$root/.github/workflows/release.yml
version_step=$(yq -r '.jobs.version.steps[] | select(.id == "version") | .run' "$workflow")
tag_step=$(yq -r '.jobs.tag.steps[] | select(.id == "tag") | .run' "$workflow")
pin_step=$(yq -r '.jobs.chart.steps[] | select(.id == "pin") | .run' "$workflow")
# without the pauses, and in seconds rather than minutes
wait_step=$(yq -r '.jobs.e2e.steps[] | select(.id == "wait") | .run' "$workflow" | sed 's/sleep 30/sleep 0.1/; s/TIMEOUT \* 60/TIMEOUT * 1/g')

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

# chart: the packaged chart runs the staged image's digest under the image's public name
image=sha256:$(printf 'a%.0s' $(seq 64))
d=$(fixture); mkdir -p "$d/deploy/chart"; cp -R "$root/deploy/chart/matchstick" "$d/deploy/chart/"
expect 'the chart pins the staged image' '' "$(run "$d" "$pin_step" DIGEST="$image" GITHUB_STEP_SUMMARY="$d/summary")"
expect '... and runs it by digest under its public name' "ghcr.io/fakta-ai/matchstick@$image" \
  "$(helm template matchstick "$d/deploy/chart/matchstick" |
    yq 'select(.kind == "Deployment") | .spec.template.spec.containers[0].image')"

d=$(fixture); mkdir -p "$d/deploy/chart"; cp -R "$root/deploy/chart/matchstick" "$d/deploy/chart/"
expect 'no staged image digest: fails' failed "$(run "$d" "$pin_step" DIGEST= GITHUB_STEP_SUMMARY="$d/summary")"

# e2e: a status counts only when its description names the staged chart's digest
chart=sha256:$(printf 'c%.0s' $(seq 64))
earlier=sha256:$(printf 'e%.0s' $(seq 64))
status() { # fixture, n, state or "" for none, [the digest its description names; this run's chart by default]
  if [ -n "$3" ]; then
    jq -n --arg state "$3" --arg description "a job ran; chart ${4:-$chart}" \
      '{statuses: [{context: "other", state: "failure"}, {context: "e2e", state: $state, description: $description}]}'
  else
    echo '{"statuses": [{"context": "other", "state": "success"}]}'
  fi > "$1/status.$2"
}
wait_for() { # fixture, env...: the workflow sets RELEASE_E2E_ACK_TIMEOUT's variable, empty when unset
  local dir=$1
  shift
  run "$dir" "$wait_step" DIGEST="$chart" ACK_TIMEOUT= "$@"
}
d=$(fixture)
expect 'no context set: no wait' '' "$(wait_for "$d" CONTEXT= TIMEOUT=30)"

d=$(fixture); status "$d" 0 success
expect 'the status succeeded: go on' '' "$(wait_for "$d" CONTEXT=e2e TIMEOUT=30)"

d=$(fixture); status "$d" 0 ''; status "$d" 1 pending; status "$d" 2 success
expect 'none, then pending, then success: go on' '' "$(wait_for "$d" CONTEXT=e2e TIMEOUT=30)"

d=$(fixture); status "$d" 0 pending; status "$d" 1 failure
expect 'pending, then failure: fails' failed "$(wait_for "$d" CONTEXT=e2e TIMEOUT=30)"

d=$(fixture); status "$d" 0 error
expect 'error: fails' failed "$(wait_for "$d" CONTEXT=e2e TIMEOUT=30)"

d=$(fixture); status "$d" 0 pending
expect 'pending until the timeout: fails' failed "$(wait_for "$d" CONTEXT=e2e TIMEOUT=1)"

d=$(fixture); status "$d" 0 success
expect 'a timeout that is not minutes: fails' failed "$(wait_for "$d" CONTEXT=e2e TIMEOUT=10m)"

d=$(fixture); status "$d" 0 success "$earlier"
expect "an earlier build's success only: fails at the timeout" failed "$(wait_for "$d" CONTEXT=e2e TIMEOUT=1)"
expect "... saying it is another build's" true \
  "$(grep -q 'success for another build' "$d/log" && echo true || echo false)"

d=$(fixture); status "$d" 0 failure "$earlier"; status "$d" 1 pending; status "$d" 2 success
expect "an earlier build's failure, then this one's success: go on" '' "$(wait_for "$d" CONTEXT=e2e TIMEOUT=30)"

d=$(fixture); status "$d" 0 success "$earlier"; status "$d" 1 failure
expect "an earlier build's success, then this one's failure: fails" failed "$(wait_for "$d" CONTEXT=e2e TIMEOUT=30)"

d=$(fixture); status "$d" 0 success
expect 'no staged chart digest: fails' failed "$(run "$d" "$wait_step" DIGEST= CONTEXT=e2e TIMEOUT=30 ACK_TIMEOUT=)"

# e2e with an acknowledgement timeout: until a status names this run's chart, the wait ends at RELEASE_E2E_ACK_TIMEOUT
pending() { # fixture, reads: this build's pending for that many reads; prints the next read's number
  local n
  for n in $(seq 0 $(($2 - 1))); do status "$1" "$n" pending; done
  echo "$2"
}
said() { # fixture, text: whether the run's log says it
  grep -qF "$2" "$1/log" && echo true || echo false
}
unacknowledged='no end-to-end system acknowledged this build within 1 minutes'
# 30 reads, at least 3 seconds: past the acknowledgement timeout of 1 (a second here)
d=$(fixture); status "$d" "$(pending "$d" 30)" success
expect 'acknowledged, pending past the acknowledgement timeout, then success: go on' '' \
  "$(wait_for "$d" CONTEXT=e2e TIMEOUT=30 ACK_TIMEOUT=1)"

d=$(fixture); status "$d" "$(pending "$d" 30)" failure
expect 'acknowledged, pending past the acknowledgement timeout, then failure: fails' failed \
  "$(wait_for "$d" CONTEXT=e2e TIMEOUT=30 ACK_TIMEOUT=1)"
expect '... with the status' true "$(said "$d" 'e2e: failure: a job ran')"

d=$(fixture); status "$d" 0 pending
expect 'acknowledged, then nothing more: fails at the timeout' failed \
  "$(wait_for "$d" CONTEXT=e2e TIMEOUT=3 ACK_TIMEOUT=1)"
expect '... not at the acknowledgement timeout' true \
  "$(said "$d" 'did not succeed for this build within 3 minutes (last: pending)')"

d=$(fixture); status "$d" 0 ''
expect 'never acknowledged: fails at the acknowledgement timeout' failed \
  "$(wait_for "$d" CONTEXT=e2e TIMEOUT=30 ACK_TIMEOUT=1)"
expect '... saying none acknowledged it' true "$(said "$d" "$unacknowledged (last: none yet)")"

d=$(fixture); status "$d" 0 pending "$earlier"
expect "another build's pending only: fails at the acknowledgement timeout" failed \
  "$(wait_for "$d" CONTEXT=e2e TIMEOUT=30 ACK_TIMEOUT=1)"
expect "... saying it is another build's" true "$(said "$d" "$unacknowledged (last: pending for another build)")"

d=$(fixture); status "$d" 0 ''; status "$d" 1 failure
expect 'a failure before any pending: fails with it, at once' failed \
  "$(wait_for "$d" CONTEXT=e2e TIMEOUT=30 ACK_TIMEOUT=1)"
expect '... with the status' true "$(said "$d" 'e2e: failure: a job ran')"

d=$(fixture); status "$d" 0 ''; status "$d" 1 success
expect 'a success before any pending: go on' '' "$(wait_for "$d" CONTEXT=e2e TIMEOUT=30 ACK_TIMEOUT=1)"

d=$(fixture); status "$d" 0 ''
expect 'no acknowledgement timeout, no status: fails at the timeout, as without one' failed \
  "$(wait_for "$d" CONTEXT=e2e TIMEOUT=2)"
expect '... not for want of an acknowledgement' true \
  "$(said "$d" 'did not succeed for this build within 2 minutes (last: none yet)')"

d=$(fixture); status "$d" 0 success
expect 'an acknowledgement timeout that is not minutes: fails' failed \
  "$(wait_for "$d" CONTEXT=e2e TIMEOUT=30 ACK_TIMEOUT=5m)"

[ "$failures" -eq 0 ]
