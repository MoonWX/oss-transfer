#!/bin/bash
set -euo pipefail
shopt -s nullglob
source_root="$(cd "$(dirname "$0")/../.." && pwd)"
test_root="$(mktemp -d "${TMPDIR:-/tmp}/oss-build-test.XXXXXX")"
test_root="$(cd "$test_root" && pwd)"
trap 'rm -rf "$test_root"' EXIT
fixture="$test_root/source with spaces"
mkdir -p "$fixture/frontend" "$fixture/cli-go" "$fixture/deploy/docker" "$fixture/docs" "$test_root/bin"
cp "$source_root/build.sh" "$fixture/build.sh"
mkdir -p "$fixture/.github/ci"
cp "$source_root/.github/ci/server-smoke-tests.txt" "$fixture/.github/ci/"
while IFS= read -r test_selection || [ -n "$test_selection" ]; do
    case "$test_selection" in ''|'#'*) continue ;; esac
    test_class="${test_selection%%#*}"
    test_source="$fixture/src/test/java/${test_class//.//}.java"
    mkdir -p "$(dirname "$test_source")"
    cp "$source_root/src/test/java/${test_class//.//}.java" "$test_source"
done < "$fixture/.github/ci/server-smoke-tests.txt"
printf '    <version>4.0.0</version>\n' > "$fixture/pom.xml"
for file in deploy.sh oss-transfer-cli.completion; do
    printf 'fixture\n' > "$fixture/deploy/$file"
done
for file in preflight-node.sh wait-node-ready.sh install.sh install-master.sh install-agent.sh runtime.sh; do
    cp "$source_root/deploy/$file" "$fixture/deploy/$file"
done
cp "$source_root/Dockerfile" "$source_root/.dockerignore" \
    "$source_root/docker-compose.yml" "$source_root/.env.example" "$fixture/"
cp -R "$source_root/deploy/docker/." "$fixture/deploy/docker/"
printf 'guide\n' > "$fixture/docs/deployment-and-user-guide.md"
printf 'readme\n' > "$fixture/README.md"
printf 'readme\n' > "$fixture/README.zh-CN.md"
cat > "$test_root/bin/npm" <<'TOOL'
#!/bin/bash
[ -z "${MOCK_BUILD_LOG:-}" ] || printf 'npm %s\n' "$*" >> "$MOCK_BUILD_LOG"
[ "${MOCK_BUILD_FAIL:-}" != "npm-$1" ] || exit 6
exit 0
TOOL
cat > "$test_root/bin/mvn" <<'TOOL'
#!/bin/bash
[ -z "${MOCK_BUILD_LOG:-}" ] || printf 'mvn %s\n' "$*" >> "$MOCK_BUILD_LOG"
[ "${MOCK_BUILD_FAIL:-}" != mvn ] || exit 7
mkdir -p target
if [[ " $* " == *' verify '* ]]; then
    printf 'tested jar\n' > target/oss-transfer.jar
else
    printf 'new jar\n' > target/oss-transfer.jar
fi
TOOL
cat > "$test_root/bin/go" <<'TOOL'
#!/bin/bash
[ -z "${MOCK_BUILD_LOG:-}" ] || printf 'go[%s/%s] %s\n' "${GOOS:-host}" "${GOARCH:-host}" "$*" >> "$MOCK_BUILD_LOG"
if [ "$1" = test ]; then
    [ "${MOCK_BUILD_FAIL:-}" != go-test ] || exit 8
    [ "$*" = 'test ./...' ] || exit 9
    exit 0
fi
while [ "$#" -gt 0 ]; do
    if [ "$1" = -o ]; then printf 'new cli\n' > "$2"; exit 0; fi
    shift
done
exit 1
TOOL
cat > "$test_root/bin/git" <<'TOOL'
#!/bin/bash
if [ "$1" = rev-parse ] && [ "$2" = --short ]; then printf 'abcdef1\n'
elif [ "$1" = rev-parse ]; then printf 'transfer-wx\n'
else printf 'fixture\n'; fi
TOOL
real_mv="$(command -v mv)"
export REAL_MV="$real_mv"
cat > "$test_root/bin/mv" <<'TOOL'
#!/bin/bash
set -euo pipefail
if [ -z "${MOCK_MV_STATE:-}" ]; then exec "$REAL_MV" "$@"; fi
call=0
[ ! -f "$MOCK_MV_STATE" ] || read -r call < "$MOCK_MV_STATE"
call=$((call+1))
printf '%s\n' "$call" > "$MOCK_MV_STATE"
if [ "$call" = "${MOCK_MV_FAIL_CALL:-}" ]; then exit 71; fi
# build.sh always uses mv -- source destination.
if [ -n "${MOCK_MV_RESTORE_FAIL_NAME:-}" ] && [[ "$2" = */package-backups/*/"$MOCK_MV_RESTORE_FAIL_NAME" ]]; then exit 72; fi
"$REAL_MV" "$@"
if [ "$call" = "${MOCK_MV_PAUSE_CALL:-}" ]; then
    touch "$MOCK_MV_STATE.ready"
    for ((attempt=0; attempt<500; attempt++)); do
        [ ! -f "$MOCK_MV_STATE.continue" ] || exit 0
        sleep 0.02
    done
    exit 73
fi
TOOL
chmod +x "$test_root/bin/"*
export PATH="$test_root/bin:$PATH"

run_build() {
    OSS_TRANSFER_OUTPUT_DIR="$output" bash "$fixture/build.sh" "$@"
}
expect_failure() {
    local expected_status="$1" actual_status
    shift
    if "$@" > "$test_root/failure.log" 2>&1; then
        cat "$test_root/failure.log" >&2
        echo 'Expected build failure' >&2; exit 1
    else
        actual_status=$?
    fi
    if [ "$actual_status" -ne "$expected_status" ]; then
        cat "$test_root/failure.log" >&2
        echo "Expected exit $expected_status, got $actual_status" >&2; exit 1
    fi
}
seed_release() {
    mkdir -p "$output/oss-transfer" "$output/qa-evidence"
    printf 'old package\n' > "$output/oss-transfer.tar.gz"
    printf 'old hash\n' > "$output/oss-transfer.tar.gz.sha256"
    printf 'old jar\n' > "$output/oss-transfer/oss-transfer.jar"
    printf 'local config\n' > "$output/oss-transfer/application.yml"
    printf 'evidence\n' > "$output/qa-evidence/keep.txt"
}
assert_original_release() {
    [ "$(cat "$output/oss-transfer.tar.gz")" = 'old package' ]
    [ "$(cat "$output/oss-transfer.tar.gz.sha256")" = 'old hash' ]
    [ "$(cat "$output/oss-transfer/oss-transfer.jar")" = 'old jar' ]
    [ "$(cat "$output/oss-transfer/application.yml")" = 'local config' ]
    [ "$(cat "$output/qa-evidence/keep.txt")" = evidence ]
}
assert_no_staging() {
    local staging
    for staging in "$output"/.build.*; do [ ! -e "$staging" ]; done
}
assert_clean() {
    assert_no_staging
    [ ! -e "$output/.publish.lock" ]
}
assert_package_version() {
    local expected_version="$1"
    [ "$(sed -n 's/^Version: //p' "$output/oss-transfer/VERSION")" = "$expected_version" ]
    cmp "$output/oss-transfer/VERSION" "$output/oss-transfer/CLI-VERSION"
    cmp "$output/oss-transfer/VERSION" "$output/oss-transfer/APPLICATION-VERSION"
    grep -Fxq '    image: ${OSS_TRANSFER_IMAGE:-moonwx/osstransfer:latest}' \
        "$output/oss-transfer/docker-compose.yml"
    grep -Fxq 'OSS_TRANSFER_IMAGE=moonwx/osstransfer:latest' \
        "$output/oss-transfer/.env.example"
    tar -xOzf "$output/oss-transfer.tar.gz" oss-transfer/docker-compose.yml | \
        grep -Fx '    image: ${OSS_TRANSFER_IMAGE:-moonwx/osstransfer:latest}' > /dev/null
    tar -xOzf "$output/oss-transfer.tar.gz" oss-transfer/.env.example | \
        grep -Fx 'OSS_TRANSFER_IMAGE=moonwx/osstransfer:latest' > /dev/null
    tar -xOzf "$output/oss-transfer/agent-bundle.tar.gz" ./VERSION > "$test_root/bundle-version"
    cmp "$output/oss-transfer/VERSION" "$test_root/bundle-version"
    # Compose stays on latest in source and every package, independently of
    # the application version recorded in the package metadata.
    grep -Fxq '    image: ${OSS_TRANSFER_IMAGE:-moonwx/osstransfer:latest}' "$fixture/docker-compose.yml"
    grep -Fxq 'OSS_TRANSFER_IMAGE=moonwx/osstransfer:latest' "$fixture/.env.example"
    cmp "$source_root/docker-compose.yml" "$fixture/docker-compose.yml"
    cmp "$source_root/.env.example" "$fixture/.env.example"
    cmp "$fixture/docker-compose.yml" "$output/oss-transfer/docker-compose.yml"
    cmp "$fixture/.env.example" "$output/oss-transfer/.env.example"
    for old_file in compose.yml compose.env.example; do
        [ ! -e "$source_root/deploy/docker/$old_file" ]
        [ ! -e "$fixture/deploy/docker/$old_file" ]
        [ ! -e "$output/oss-transfer/deploy/docker/$old_file" ]
    done
    tar -tzf "$output/oss-transfer.tar.gz" > "$test_root/version-archive-files"
    if grep -Eq '^oss-transfer/deploy/docker/compose(\.yml|\.env\.example)$' "$test_root/version-archive-files"; then
        echo 'Package contains superseded Compose files' >&2; exit 1
    fi
}

output="$test_root/release output"
seed_release
MOCK_BUILD_FAIL=mvn expect_failure 7 run_build
assert_original_release
assert_clean
run_build > "$test_root/build.log" 2>&1
[ "$(cat "$output/oss-transfer/oss-transfer.jar")" = 'new jar' ]
[ "$(cat "$output/qa-evidence/keep.txt")" = evidence ]
cmp "$source_root/Dockerfile" "$output/oss-transfer/Dockerfile"
cmp "$source_root/.dockerignore" "$output/oss-transfer/.dockerignore"
for file in preflight-node.sh wait-node-ready.sh; do
    cmp "$source_root/deploy/$file" "$output/oss-transfer/deploy/$file"
    [ -x "$output/oss-transfer/deploy/$file" ]
done
for file in docker-defaults.yml entrypoint.sh healthcheck.sh cli.sh; do
    cmp "$source_root/deploy/docker/$file" "$output/oss-transfer/deploy/docker/$file"
done
assert_package_version 4.0.0
[ -x "$output/oss-transfer/oss-transfer-cli-arm64" ]
[ -x "$output/oss-transfer/oss-transfer-cli-amd64" ]
[ "$(sed -n 's/^Commit: //p' "$output/oss-transfer/VERSION")" = abcdef1 ]
tar -tzf "$output/oss-transfer.tar.gz" > "$test_root/archive-files"
grep -q 'oss-transfer/deploy/docker/entrypoint.sh' "$test_root/archive-files"
grep -q 'oss-transfer/deploy/preflight-node.sh' "$test_root/archive-files"
grep -q 'oss-transfer/deploy/wait-node-ready.sh' "$test_root/archive-files"
grep -q 'oss-transfer/agent-bundle.tar.gz' "$test_root/archive-files"
tar -tzf "$output/oss-transfer/agent-bundle.tar.gz" > "$test_root/bundle-files"
grep -q './deploy/install-agent.sh' "$test_root/bundle-files"
grep -q './deploy/wait-node-ready.sh' "$test_root/bundle-files"
if grep -Eq 'application\.yml|secrets|frontend|README|test-results' "$test_root/bundle-files"; then
    echo 'Agent bundle contains configuration or unrelated files' >&2; exit 1
fi
(cd "$output/oss-transfer" && shasum -a 256 -c agent-bundle.tar.gz.sha256)
(cd "$output" && shasum -a 256 -c oss-transfer.tar.gz.sha256)
backup_count=0
for backup in "$output/package-backups/"*; do
    [ "$(cat "$backup/oss-transfer.tar.gz")" = 'old package' ]
    [ "$(cat "$backup/oss-transfer/oss-transfer.jar")" = 'old jar' ]
    [ "$(cat "$backup/oss-transfer/application.yml")" = 'local config' ]
    [ "$(cat "$backup/oss-transfer.tar.gz.sha256")" = 'old hash' ]
    backup_count=$((backup_count+1))
done
[ "$backup_count" -eq 1 ]
assert_clean

# CI tests and packages in one invocation. The JAR produced by verify is the
# published JAR; a second build must not replace it with an untested artifact.
output="$test_root/tested release"
MOCK_BUILD_LOG="$test_root/tested-commands" run_build --test > "$test_root/tested-build.log" 2>&1
[ "$(cat "$output/oss-transfer/oss-transfer.jar")" = 'tested jar' ]
tar -xOzf "$output/oss-transfer.tar.gz" oss-transfer/oss-transfer.jar > "$test_root/tested-jar"
[ "$(cat "$test_root/tested-jar")" = 'tested jar' ]
[ "$(grep -c '^npm ' "$test_root/tested-commands")" -eq 2 ]
[ "$(grep -c '^mvn ' "$test_root/tested-commands")" -eq 1 ]
grep -Fxq 'mvn clean verify -q' "$test_root/tested-commands"
grep -Fxq 'npm ci' "$test_root/tested-commands"
grep -Fxq 'npm run build' "$test_root/tested-commands"
[ "$(grep -c '] test ' "$test_root/tested-commands")" -eq 1 ]
[ "$(grep -c '] build ' "$test_root/tested-commands")" -eq 2 ]
grep -Fq 'go[linux/amd64] build ' "$test_root/tested-commands"
grep -Fq 'go[linux/arm64] build ' "$test_root/tested-commands"
if grep -Eq 'skipTests|maven.test.skip|clean package' "$test_root/tested-commands"; then
    echo 'Tested build skipped tests or rebuilt the verified package' >&2; exit 1
fi
assert_clean

# The daily core suite uses an explicit list, keeps CLI coverage, and packages
# the same tested JAR without a second production compilation.
output="$test_root/smoke release"
MOCK_BUILD_LOG="$test_root/smoke-commands" run_build --test=smoke > "$test_root/smoke-build.log" 2>&1
[ "$(cat "$output/oss-transfer/oss-transfer.jar")" = 'tested jar' ]
[ "$(grep -c '^npm ' "$test_root/smoke-commands")" -eq 2 ]
[ "$(grep -c '^mvn ' "$test_root/smoke-commands")" -eq 1 ]
smoke_classes="$(sed '/^#/d; /^$/d' "$fixture/.github/ci/server-smoke-tests.txt" | paste -sd, -)"
grep -Fxq "mvn clean verify -q -Dtest=$smoke_classes -Dsurefire.failIfNoSpecifiedTests=true" "$test_root/smoke-commands"
[ "$(grep -c '] test ' "$test_root/smoke-commands")" -eq 1 ]
[ "$(grep -c '] build ' "$test_root/smoke-commands")" -eq 2 ]
assert_clean

# Change-based CI may omit unaffected component tests, but both production
# components must still be compiled and included in every release package.
for mode in --test --test=smoke; do
    for components in server cli none; do
        output="$test_root/component $mode $components"
        command_log="$test_root/components-$mode-$components"
        BUILD_TEST_COMPONENTS="$components" MOCK_BUILD_LOG="$command_log" run_build "$mode" > "$test_root/component-build.log" 2>&1
        [ "$(grep -c '^mvn ' "$command_log")" -eq 1 ]
        [ "$(grep -c '] build ' "$command_log")" -eq 2 ]
        if [ "$components" = server ]; then
            grep -q '^mvn clean verify -q' "$command_log"
        else
            grep -Fxq 'mvn clean package -q -DskipTests' "$command_log"
        fi
        if [ "$components" = cli ]; then
            [ "$(grep -c '] test ' "$command_log")" -eq 1 ]
        elif grep -q '] test ' "$command_log"; then
            echo 'Ran tests for an unselected component' >&2; exit 1
        fi
        assert_clean
    done
done

# Every prerequisite failure must stop before publishing, even with a JAR
# left in target by an earlier successful build.
for mode in --test --test=smoke; do
    for phase in npm-ci npm-run mvn go-test; do
        output="$test_root/tested failure $mode $phase"
        seed_release
        case "$phase" in npm-*) expected_exit=6 ;; mvn) expected_exit=7 ;; go-test) expected_exit=8 ;; esac
        MOCK_BUILD_FAIL="$phase" expect_failure "$expected_exit" run_build "$mode"
        assert_original_release
        assert_clean
    done
done

# A missing, empty, or stale suite must fail before compiling, not fall back
# to all tests (or silently produce an untested package).
output="$test_root/invalid suite"
seed_release
cp "$fixture/.github/ci/server-smoke-tests.txt" "$test_root/original-smoke-list"
for invalid_list in missing empty unknown duplicate wildcard whole-class unknown-method wildcard-method; do
    case "$invalid_list" in
        missing) rm "$fixture/.github/ci/server-smoke-tests.txt" ;;
        empty) printf '# Empty suite\n' > "$fixture/.github/ci/server-smoke-tests.txt" ;;
        unknown) printf 'com.transfer.tools.DoesNotExistTest#missingMethod\n' > "$fixture/.github/ci/server-smoke-tests.txt" ;;
        duplicate) cat "$test_root/original-smoke-list" "$test_root/original-smoke-list" > "$fixture/.github/ci/server-smoke-tests.txt" ;;
        wildcard) printf '*Test\n' > "$fixture/.github/ci/server-smoke-tests.txt" ;;
        whole-class) printf 'com.transfer.tools.service.TaskStopResumeTest\n' > "$fixture/.github/ci/server-smoke-tests.txt" ;;
        unknown-method) printf 'com.transfer.tools.service.TaskStopResumeTest#doesNotExist\n' > "$fixture/.github/ci/server-smoke-tests.txt" ;;
        wildcard-method) printf 'com.transfer.tools.service.TaskStopResumeTest#stop*\n' > "$fixture/.github/ci/server-smoke-tests.txt" ;;
    esac
    MOCK_BUILD_LOG="$test_root/invalid-suite-commands" expect_failure 2 run_build --test=smoke
    [ ! -e "$test_root/invalid-suite-commands" ]
    assert_original_release
    assert_clean
done
cp "$test_root/original-smoke-list" "$fixture/.github/ci/server-smoke-tests.txt"
output="$test_root/invalid options"
seed_release
expect_failure 2 run_build --skip-build
expect_failure 2 run_build --test extra
expect_failure 2 run_build --test=smoke extra
expect_failure 2 run_build --test=unknown
for components in '' frontend 'server,' 'cli,server' 'none,server'; do
    BUILD_TEST_COMPONENTS="$components" expect_failure 2 run_build --test=smoke
done
assert_original_release
assert_clean

# Fail each old-file backup and each replacement move independently.
for call in 1 2 3 4 5 6; do
    output="$test_root/rollback $call"
    seed_release
    MOCK_MV_STATE="$test_root/moves-$call" MOCK_MV_FAIL_CALL="$call" expect_failure 71 run_build
    assert_original_release
    assert_clean
done

# A first release must remove all new entries after a partial publication.
for call in 1 2 3; do
    output="$test_root/fresh rollback $call"
    MOCK_MV_STATE="$test_root/fresh-moves-$call" MOCK_MV_FAIL_CALL="$call" expect_failure 71 run_build
    [ ! -e "$output/oss-transfer" ]
    [ ! -e "$output/oss-transfer.tar.gz" ]
    [ ! -e "$output/oss-transfer.tar.gz.sha256" ]
    assert_clean
done

# Existing partial releases and symlinks are restored without following them.
output="$test_root/partial release"
seed_release
rm "$output/oss-transfer.tar.gz"
MOCK_MV_STATE="$test_root/partial-moves" MOCK_MV_FAIL_CALL=5 expect_failure 71 run_build
[ ! -e "$output/oss-transfer.tar.gz" ]
[ "$(cat "$output/oss-transfer/oss-transfer.jar")" = 'old jar' ]
[ "$(cat "$output/oss-transfer.tar.gz.sha256")" = 'old hash' ]
[ "$(cat "$output/qa-evidence/keep.txt")" = evidence ]
assert_clean
output="$test_root/symlink release"
mkdir -p "$output" "$test_root/external package"
printf 'external data\n' > "$test_root/external package/keep.txt"
ln -s "$test_root/external package" "$output/oss-transfer"
ln -s "$test_root/missing archive" "$output/oss-transfer.tar.gz"
MOCK_MV_STATE="$test_root/symlink-moves" MOCK_MV_FAIL_CALL=4 expect_failure 71 run_build
[ "$(readlink "$output/oss-transfer")" = "$test_root/external package" ]
[ "$(readlink "$output/oss-transfer.tar.gz")" = "$test_root/missing archive" ]
[ "$(cat "$test_root/external package/keep.txt")" = 'external data' ]
assert_clean

# A restoration failure preserves its backup, restores other files, and blocks
# a new publication until the incomplete rollback is explicitly recovered.
output="$test_root/restore failure"
seed_release
MOCK_MV_STATE="$test_root/restore-moves" MOCK_MV_FAIL_CALL=4 MOCK_MV_RESTORE_FAIL_NAME=oss-transfer expect_failure 71 run_build
[ ! -e "$output/oss-transfer" ]
[ "$(cat "$output/oss-transfer.tar.gz")" = 'old package' ]
[ "$(cat "$output/oss-transfer.tar.gz.sha256")" = 'old hash' ]
[ "$(cat "$output/qa-evidence/keep.txt")" = evidence ]
for backup in "$output/package-backups/"*; do
    [ "$(cat "$backup/oss-transfer/oss-transfer.jar")" = 'old jar' ]
done
grep -Fq "$output/package-backups/" "$test_root/failure.log"
[ -d "$output/.publish.lock" ]
assert_no_staging
expect_failure 1 run_build
[ -d "$output/.publish.lock" ]
assert_no_staging

# Hold the first publisher after moving its old directory. Another completed
# build must neither replace its files nor release its lock.
output="$test_root/concurrent release"
seed_release
MOCK_MV_STATE="$test_root/paused-moves" MOCK_MV_PAUSE_CALL=1 run_build > "$test_root/concurrent.log" 2>&1 &
publisher_pid=$!
for ((attempt=0; attempt<500; attempt++)); do
    [ ! -f "$test_root/paused-moves.ready" ] || break
    sleep 0.02
done
[ -f "$test_root/paused-moves.ready" ]
expect_failure 1 run_build
[ -d "$output/.publish.lock" ]
[ ! -e "$output/oss-transfer" ]
[ "$(cat "$output/oss-transfer.tar.gz")" = 'old package' ]
touch "$test_root/paused-moves.continue"
wait "$publisher_pid"
[ "$(cat "$output/oss-transfer/oss-transfer.jar")" = 'new jar' ]
[ "$(cat "$output/qa-evidence/keep.txt")" = evidence ]
(cd "$output" && shasum -a 256 -c oss-transfer.tar.gz.sha256)
assert_clean
# Changing only the POM changes package metadata; Compose stays on latest
# and installer scripts are copied without a manual version edit.
for fixture_version in 4.0.0 4.7.2; do
    printf '    <version>%s</version>\n' "$fixture_version" > "$fixture/pom.xml"
    output="$test_root/project version $fixture_version"
    run_build > "$test_root/version-$fixture_version.log" 2>&1
    assert_package_version "$fixture_version"
    cmp "$fixture/deploy/install.sh" "$output/oss-transfer/deploy/install.sh"
    (cd "$output" && shasum -a 256 -c oss-transfer.tar.gz.sha256)
    assert_clean
done
printf 'Build packaging checks passed (full/core test selection, component selection, tested build reuse, test failure isolation, POM-derived versions, root Compose defaults to latest, Docker materials, checksum, backups, QA evidence, rollback at every move, partial releases, symlinks, restore failure, concurrent publishing)\n'
