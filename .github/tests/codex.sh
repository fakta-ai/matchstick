#!/usr/bin/env bash
# The codex gate (.github/workflows/codex.yml) passes a pull request without a review only when Dependabot opened it
# and flake.lock is the one path it changes. Runs the workflow's own step against a fake `gh` that answers from fixture
# pull requests, and reads back the status it sets. Run it in the dev shell, which has jq and yq.
set -euo pipefail
root=$(cd "$(dirname "$0")/../.." && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

step=$(yq -r '.jobs.status.steps[] | select(.id == "status") | .run' "$root/.github/workflows/codex.yml")
mkdir -p "$work/bin"
cat > "$work/bin/gh" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
[ "$1" = api ] || exit 2
shift
filter=. method=GET fields=()
while [ $# -gt 0 ]; do
  case $1 in
    --paginate|--silent) ;;
    --jq) filter=$2; shift ;;
    -X) method=$2; shift ;;
    -f) fields+=("$2"); shift ;;
    *) path=$1 ;;
  esac
  shift
done
case $method:$path in
  GET:repos/owner/repo/pulls/7) jq -r "$filter" "$FIXTURE/pull.json" ;;
  GET:repos/owner/repo/pulls/7/files) jq -r "$filter" "$FIXTURE/files.json" ;;
  GET:repos/owner/repo/issues/7/comments) jq -r "$filter" "$FIXTURE/comments.json" ;;
  POST:repos/owner/repo/statuses/*) printf '%s %s\n' "${path##*/}" "${fields[*]}" >> "$FIXTURE/statuses" ;;
  *) echo "unexpected: $method $path" >&2; exit 1 ;;
esac
EOF
chmod +x "$work/bin/gh"

head=0123456789abcdef0123456789abcdef01234567
# the status set for a pull request by $1 changing the files $2 (a JSON array of {filename, previous_filename?});
# Codex's comment says it finished reviewing $3, if given
status() {
  fixture=$(mktemp -d "$work/case.XXXXXX")
  jq -n --arg author "$1" --arg head "$head" --argjson files "$2" \
    '{user: {login: $author}, head: {sha: $head}, changed_files: ($files | length)}' > "$fixture/pull.json"
  printf '%s\n' "$2" > "$fixture/files.json"
  if [ -n "${3:-}" ]; then
    jq -n --arg reviewed "$3" '[{user: {login: "chatgpt-codex-connector[bot]"}, body: ("<!-- codex-review-state:v1 " + ({code_review: {commit_sha: $reviewed, status: "completed"}} | tojson) + " -->")}]' \
      > "$fixture/comments.json"
  else
    echo '[]' > "$fixture/comments.json"
  fi
  FIXTURE="$fixture" PATH="$work/bin:$PATH" GH_TOKEN=fixture REPO=owner/repo PR=7 bash -c "$step" > /dev/null
  cat "$fixture/statuses"
}
expect() { # name, wanted state, status line
  case $3 in
    "$head state=$2 context=codex "*) echo "PASS: $1" ;;
    *) echo "FAIL: $1: got '$3'" >&2; exit 1 ;;
  esac
}
files() { jq -cn '$ARGS.positional | map({filename: .})' --args "$@"; }
bot='dependabot[bot]'

expect 'Dependabot changing only flake.lock passes without a review' success "$(status "$bot" "$(files flake.lock)")"
expect 'Dependabot changing flake.lock and another file waits for a review' pending \
  "$(status "$bot" "$(files flake.lock go.sum)")"
expect 'Dependabot changing another file waits for a review' pending "$(status "$bot" "$(files go.sum)")"
expect 'Dependabot moving another file onto flake.lock waits for a review' pending \
  "$(status "$bot" '[{"filename": "flake.lock", "previous_filename": "flake.nix"}]')"
expect 'a person changing only flake.lock waits for a review' pending "$(status someone "$(files flake.lock)")"
expect 'another bot changing only flake.lock waits for a review' pending \
  "$(status 'github-actions[bot]' "$(files flake.lock)")"
expect 'a reviewed pull request passes' success "$(status someone "$(files flake.lock go.sum)" "$head")"
expect 'a pull request reviewed at an older commit waits' pending \
  "$(status someone "$(files flake.lock)" fedcba9876543210fedcba9876543210fedcba98)"
