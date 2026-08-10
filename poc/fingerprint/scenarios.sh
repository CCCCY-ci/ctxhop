#!/usr/bin/env bash
#
# PoC-2 scenario matrix.
#
# The consistency check only earns its place if it stays quiet when nothing
# relevant changed. A check that cries wolf gets dismissed, and a dismissed
# check protects nobody. These scenarios measure exactly that: which situations
# produce a warning, and whether each warning is deserved.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
WORK="${WORK:-${TMPDIR:-/tmp}/agentsync-poc2}"
# The fingerprint belongs to the sync store, not the project. Keeping it
# inside the repo would let scenarios commit and then check it out from under
# themselves.
FP="${TMPDIR:-/tmp}/agentsync-poc2-fingerprint.json"
TOUCHED="src/a.go,docs/x.md"

# Build once rather than `go run` per invocation: the scenarios cd into a
# scratch repo that is not part of any module, where `go run` cannot resolve
# the package.
BIN="${TMPDIR:-/tmp}/agentsync-poc2-fingerprint.exe"
(cd "$REPO_ROOT" && go build -o "$BIN" ./poc/fingerprint)

pass=0
fail=0

setup() {
  rm -rf "$WORK"
  mkdir -p "$WORK/src" "$WORK/docs"
  cd "$WORK"
  git init -q -b main
  printf 'package main\nfunc A() int { return 1 }\n' > src/a.go
  printf 'package main\nfunc B() int { return 2 }\n' > src/b.go
  printf 'package main\nfunc C() int { return 3 }\n' > src/c.go
  printf '# Doc\noriginal\n' > docs/x.md
  git add -A
  git commit -qm "initial"

  # A second commit, so that scenario 8 can branch from a point that does not
  # contain the session's starting commit. With a single commit, every branch
  # is a descendant and true divergence cannot be expressed.
  printf '# Doc\nsecond\n' > docs/x.md
  git add -A
  git commit -qm "second"

  # The session left an uncommitted edit in a file it touched. This is the
  # normal end state of a working day and the case the whole check exists for.
  printf 'package main\nfunc A() int { return 42 }\n' > src/a.go

  "$BIN" -mode capture -root "$WORK" -touched "$TOUCHED" -fingerprint "$FP" >/dev/null
}

check() {
  local name="$1" expect="$2"
  local out verdict
  out="$("$BIN" -mode compare -root "$WORK" -fingerprint "$FP")"
  verdict="$(echo "$out" | grep '^VERDICT:' | awk '{print $2}')"

  if [ "$verdict" = "$expect" ]; then
    printf '  PASS  %-46s -> %s\n' "$name" "$verdict"
    pass=$((pass + 1))
  else
    printf '  FAIL  %-46s -> %s (expected %s)\n' "$name" "$verdict" "$expect"
    echo "$out" | sed 's/^/        /'
    fail=$((fail + 1))
  fi
}

echo "PoC-2 scenario matrix"
echo

# 1. Nothing changed on the target machine.
setup
check "unchanged workspace" consistent

# 2. The user edited a file the session never touched. This is the single most
#    important case: if it warns here, users learn to ignore the warning.
setup
printf 'package main\nfunc C() int { return 999 }\n' > "$WORK/src/c.go"
check "unrelated file edited" consistent

# 3. A file the session touched was changed underneath it.
setup
printf 'package main\nfunc A() int { return 7 }\n' > "$WORK/src/a.go"
check "touched file changed" inconsistent

# 4. Many unrelated files changed at once, e.g. a dependency update.
setup
for i in 1 2 3 4 5; do printf 'package main\n// noise %s\n' "$i" > "$WORK/src/noise$i.go"; done
check "many unrelated files added" consistent

# 5. The target machine committed the session's own uncommitted work and moved
#    on. Committing does not change file contents, so nothing differs and the
#    ideal answer is silence.
setup
cd "$WORK"
git add -A && git commit -qm "commit the session's work"
printf 'later\n' > docs/later.md
git add -A && git commit -qm "further work"
check "session work committed, then more commits" consistent

# 5b. A later commit changed a file the session had been editing. The change is
#     explainable, but the agent's picture of that file is stale, so it must
#     still be surfaced rather than silently accepted.
setup
cd "$WORK"
git add -A && git commit -qm "commit the session's work"
printf 'package main\nfunc A() int { return 100 }\n' > src/a.go
git add -A && git commit -qm "someone else changed it"
check "later commit changed a touched file" explainable

# 6. A file the session touched was deleted.
setup
rm "$WORK/src/a.go"
check "touched file deleted" inconsistent

# 7. Whitespace-only reformat of a touched file.
setup
printf 'package main\n\nfunc A() int {\n\treturn 42\n}\n' > "$WORK/src/a.go"
check "touched file reformatted" inconsistent

# 8. A branch that genuinely does not contain the session's starting commit.
#    Branching from the current commit would produce a descendant, which is not
#    divergence at all, so the branch has to start from an earlier one.
setup
cd "$WORK"
git checkout -q --force -b sidetrack HEAD~1
printf 'package main\nfunc A() int { return -1 }\n' > src/a.go
git add -A && git commit -qm "divergent work"
check "divergent branch" inconsistent

echo
echo "passed $pass, failed $fail"
[ "$fail" -eq 0 ]
