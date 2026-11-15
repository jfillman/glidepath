#!/usr/bin/env bash
# deliver-onboarding-files, the gitops half: whether a resync has anything to deliver is decided from the tree's own state,
# never from how a pipe ended.
#
# 2026-11-15, found live on the lab: the owner's cicd.yaml push ran onboarding-resync twice for gate-api, each time logging
# "gitops repo: .tekton/ already up to date, nothing to deliver" while eleven .tekton files differed from the platform's
# templates (so no release PR check could start). The decision was `git status --porcelain | grep -q .` under
# `set -o pipefail`: grep -q quits at the first line, git is still writing, git gets SIGPIPE (141), the pipeline is
# non-zero, and the `if` takes the "nothing changed" branch. Reproduced in the task's own toolbox image: 1 run in ~300 on an
# idle machine, 1 in 5 with the CPU shared.
#
# The Task step is rendered from the chart and run end to end against a local origin, with a `git` in front whose `status`
# prints a line at a time (what a busy node does to git), a `git status` that fails, and an origin that is current:
#   1. differing files          -> a PR is opened (this is the case that fails on `| grep -q`)
#   2. a current tree           -> "nothing to deliver", said with the count of files compared, no push
#   3. `git status` failing     -> the step FAILS; it never reports "nothing to deliver"
#   4. neither half of the Task decides from `git status | grep`
#
# Usage: charts/glidepath-catalog/tests/onboarding_resync_decision_test.sh      (needs helm, yq, jq, git)
set -u
cd "$(dirname "$0")/../../.."
fail=0
d="$(mktemp -d)"
trap 'rm -rf "$d"' EXIT
REAL_GIT="$(command -v git)"

helm template t charts/glidepath-catalog -f charts/glidepath-catalog/tests/values.example.yaml \
  --show-only templates/tasks/deliver-onboarding-files.yaml > "$d/task.yaml" || { echo "FAIL render"; exit 1; }
yq '.spec.steps[] | select(.name == "deliver-gitops-repo-files") | .script' "$d/task.yaml" > "$d/step.sh"
yq '.spec.steps[] | select(.name == "deliver-app-repo-files") | .script' "$d/task.yaml" > "$d/app-step.sh"
[[ -s "$d/step.sh" && -s "$d/app-step.sh" ]] || { echo "FAIL the steps are not in the rendered Task"; exit 1; }

# The templates mount is a path in the image; the test points the script at its own directory.
mkdir -p "$d/tpl" "$d/lib" "$d/bin"
sed "s#/opt/platform/onboarding-templates/gitops-repo#${d}/tpl#g" "$d/step.sh" > "$d/step-local.sh"
for n in clearance-gate policy-validation values qa sast; do
  printf '# template %s\nname: <APP_NAME>\nnamespace: <APP_NAMESPACE>\n' "$n" > "$d/tpl/pull-request-$n.yaml"
done
cp catalog/lib/retry.sh "$d/lib/retry.sh"
printf 'github_app_installation_token() { echo test-token; }\n' > "$d/lib/github-app.sh"

# git: clone comes from the local origin; status prints a line at a time (or fails); push and curl are recorded, not made.
cat > "$d/bin/git" <<EOF
#!/usr/bin/env bash
case "\$1" in
  clone) exec "${REAL_GIT}" clone --quiet "file://\${ORIGIN}" "\${@: -1}" ;;
  status)
    if [[ -n "\${FAKE_STATUS_FAIL:-}" ]]; then echo "fatal: simulated status failure" >&2; exit 128; fi
    out="\$("${REAL_GIT}" "\$@")"
    [[ -z "\${out}" ]] && exit 0
    while IFS= read -r l; do printf '%s\n' "\${l}"; sleep 0.05; done <<<"\${out}" ;;
  push) echo "push \$*" >> "\${CALLS}"; exit 0 ;;
  *) exec "${REAL_GIT}" "\$@" ;;
esac
EOF
cat > "$d/bin/curl" <<'EOF'
#!/usr/bin/env bash
echo "curl $*" >> "${CALLS}"
echo '[]'
EOF
chmod +x "$d/bin/git" "$d/bin/curl"

# An origin whose .tekton holds `$1` (stale | current); the rendered templates are what "current" means.
make_origin() {
  local kind="$1" o="$d/origin-$1" t b
  rm -rf "$o"; mkdir -p "$o/.tekton"
  for t in "$d"/tpl/*.yaml; do
    b="$(basename "$t")"
    if [[ "$kind" == current ]]; then
      sed -e 's|<APP_NAMESPACE>|app-shop-cicd|g' -e 's|<APP_NAME>|shop|g' "$t" > "$o/.tekton/$b"
    else
      printf '# older platform\nname: shop\n' > "$o/.tekton/$b"
    fi
  done
  "${REAL_GIT}" -C "$o" init -q -b main
  "${REAL_GIT}" -C "$o" add .tekton
  "${REAL_GIT}" -C "$o" -c user.name=t -c user.email=t@t.invalid commit -q -m base
  echo "$o"
}

run_step() { # <origin> [extra env...]; sets out and rc
  local origin="$1"; shift
  : > "$d/calls"
  out="$(env -i PATH="$d/bin:$PATH" HOME="$d" TMPDIR="$d" PLATFORM_LIB="$d/lib" CALLS="$d/calls" ORIGIN="$origin" \
    GITOPS_REPO_URL=https://github.com/o/gitops-shop CLEARANCE_CHECKS=false APP_NAMESPACE=app-shop-cicd APP_NAME=shop \
    "$@" bash "$d/step-local.sh" 2>&1)"
  rc=$?
}

# 1. differing files, repeated: a decision from a pipe's status loses a run now and then; this one never may.
stale="$(make_origin stale)"
for i in 1 2 3 4 5; do
  run_step "$stale"
  if (( rc != 0 )) || [[ "$out" != *"opened onboarding-resync PR"* ]] || ! grep -q '^push ' "$d/calls"; then
    echo "FAIL differing files (run $i): rc=${rc}, no PR opened. Output: $(tail -3 <<<"$out")"; fail=1; break
  fi
  if [[ "$out" != *"5 of 5 template files differ"* ]]; then
    echo "FAIL differing files: the log does not say how many differ. Output: $(tail -3 <<<"$out")"; fail=1; break
  fi
done

# 2. current files: said as a fact, with the count compared, and nothing is pushed.
current="$(make_origin current)"
run_step "$current"
if (( rc != 0 )) || [[ "$out" != *"nothing to deliver"* ]] || [[ "$out" != *"5 template files compared, 0 differ"* ]] || grep -q '^push ' "$d/calls"; then
  echo "FAIL current files: rc=${rc}. Output: $(tail -3 <<<"$out")"; fail=1
fi

# 3. git status failing must fail the step: no "nothing to deliver" from an error.
run_step "$stale" FAKE_STATUS_FAIL=1
if (( rc == 0 )) || [[ "$out" == *"nothing to deliver"* ]] || grep -q '^push ' "$d/calls"; then
  echo "FAIL a failing git status: rc=${rc}, output: $(tail -3 <<<"$out")"; fail=1
fi

# 4. neither half decides on a pipe's status.
for f in step.sh app-step.sh; do
  if grep -vE '^[[:space:]]*#' "$d/$f" | grep -E 'git (status|diff)[^|]*\|[[:space:]]*grep' >/dev/null; then
    echo "FAIL ${f}: decides from 'git status | grep' (an early grep exit turns into 'nothing changed' under pipefail)"; fail=1
  fi
done

(( fail == 0 )) && echo "ok: onboarding resync decides from captured git status (differing, current, failing)"
exit "$fail"
