#!/bin/sh
# Tests for usr/lib/smoothnas/dkms-autoinstall.sh against a fake
# /lib/modules and a stub dkms. The real-DKMS end-to-end test is in the CI
# package job.
#
# Usage: packaging/tests/dkms-autoinstall-test.sh
set -eu

HERE="$(cd "$(dirname "$0")" && pwd)"
SCRIPT="$HERE/../smoothnas/usr/lib/smoothnas/dkms-autoinstall.sh"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
failures=0

fail() {
    echo "FAIL: $case_name: $*" >&2
    failures=$((failures + 1))
}

setup() {
    case_name="$1"
    T="$WORK/$1"
    MODS="$T/lib/modules"
    BIN="$T/bin"
    mkdir -p "$MODS" "$BIN" "$T/usr/src"
    : > "$T/calls"
    # dkms fails for any kernel listed in $T/fail-kernels.
    cat > "$BIN/dkms" <<STUB
#!/bin/sh
echo "dkms \$*" >> "$T/calls"
[ "\$1" = autoinstall ] && [ "\$2" = -k ] && grep -qx "\$3" "$T/fail-kernels" 2>/dev/null && exit 1
exit 0
STUB
    chmod +x "$BIN/dkms"
}

# kernel <kver> headers|no-headers|dangling
kernel() {
    mkdir -p "$MODS/$1/kernel"
    case "$2" in
        headers)
            mkdir -p "$T/usr/src/linux-headers-$1"
            echo "# kbuild" > "$T/usr/src/linux-headers-$1/Makefile"
            ln -s "$T/usr/src/linux-headers-$1" "$MODS/$1/build"
            ;;
        dangling)
            ln -s "$T/usr/src/linux-headers-$1" "$MODS/$1/build"
            ;;
        no-headers) ;;
    esac
}

run() {
    set +e
    PATH="$BIN:$PATH" SMOOTHNAS_MODULES_DIR="$MODS" sh "$SCRIPT" 2> "$T/stderr"
    status=$?
    set -e
}

calls() { sort "$T/calls" | tr '\n' ';'; }

# 1. The WTR Max after its Nov 1 upgrade: 7.2.14-83's image was configured
# before its headers, so DKMS skipped it. 7.2.10-76 was removed (rc): its
# directory remains without headers.
setup wtr-max-upgrade
kernel 7.2.10-76-smoothkernel no-headers
kernel 7.2.10-80-smoothkernel headers
kernel 7.2.14-83-smoothkernel headers
run
[ "$status" = 0 ] || fail "exit $status: $(cat "$T/stderr")"
want="dkms autoinstall -k 7.2.10-80-smoothkernel;dkms autoinstall -k 7.2.14-83-smoothkernel;"
[ "$(calls)" = "$want" ] || fail "calls: got '$(calls)', want '$want'"

# 2. Kernel image installed, headers not yet: its build link dangles. Skip
# it; the run after its headers are configured builds it.
setup headers-not-yet-installed
kernel 7.2.15-84-smoothkernel dangling
kernel 7.2.14-83-smoothkernel headers
run
[ "$status" = 0 ] || fail "exit $status"
[ "$(calls)" = "dkms autoinstall -k 7.2.14-83-smoothkernel;" ] || fail "calls: '$(calls)'"

# 3. The next dpkg run, once the headers are in: now it builds.
kernel_headers_arrive() {
    mkdir -p "$T/usr/src/linux-headers-$1"
    echo "# kbuild" > "$T/usr/src/linux-headers-$1/Makefile"
}
kernel_headers_arrive 7.2.15-84-smoothkernel
: > "$T/calls"
run
[ "$status" = 0 ] || fail "exit $status"
grep -qx "dkms autoinstall -k 7.2.15-84-smoothkernel" "$T/calls" || fail "did not build once headers arrived: '$(calls)'"

# 4. A failed build is reported with its kernel, the other kernels are still
# built, and the exit status says something failed.
setup one-kernel-fails
kernel 7.2.14-83-smoothkernel headers
kernel 7.2.15-84-smoothkernel headers
echo 7.2.14-83-smoothkernel > "$T/fail-kernels"
run
[ "$status" = 1 ] || fail "exit $status, want 1"
grep -q "7.2.14-83-smoothkernel" "$T/stderr" || fail "failure does not name the kernel: $(cat "$T/stderr")"
grep -qx "dkms autoinstall -k 7.2.15-84-smoothkernel" "$T/calls" || fail "stopped after the failure: '$(calls)'"

# 5. No dkms (smoothnas removed, conffile left): nothing to do, no error.
setup no-dkms
kernel 7.2.14-83-smoothkernel headers
rm "$BIN/dkms"
set +e
PATH="$BIN:/usr/bin:/bin" SMOOTHNAS_MODULES_DIR="$MODS" sh -c 'command -v dkms >/dev/null && exit 99; exec sh "$0"' "$SCRIPT" 2> "$T/stderr"
status=$?
set -e
if [ "$status" = 99 ]; then
    echo "skip: no-dkms (host has a real dkms on PATH)"
else
    [ "$status" = 0 ] || fail "exit $status"
fi

# 6. Empty or missing modules directory.
setup no-kernels
rmdir "$MODS"
run
[ "$status" = 0 ] || fail "exit $status"
[ -z "$(calls)" ] || fail "calls: '$(calls)'"

# 7. The apt hook runs the script, and tolerates it being absent or failing:
# a dpkg run must never fail because of it.
case_name=apt-hook
HOOK="$HERE/../smoothnas/etc/apt/apt.conf.d/53smoothnas-dkms"
grep -q 'DPkg::Post-Invoke' "$HOOK" || fail "53smoothnas-dkms has no DPkg::Post-Invoke"
cmd="$(sed -n 's/.*DPkg::Post-Invoke { "\(.*\)"; };.*/\1/p' "$HOOK" | sed 's/\\"/"/g')"
[ -n "$cmd" ] || fail "could not read the Post-Invoke command"
echo "$cmd" | grep -q '/usr/lib/smoothnas/dkms-autoinstall.sh' || fail "hook does not run dkms-autoinstall.sh: $cmd"
for target in /nonexistent/script /bin/false; do
    probe="$(echo "$cmd" | sed "s|/usr/lib/smoothnas/dkms-autoinstall.sh|$target|g")"
    sh -c "$probe" || fail "hook fails the dpkg run when the script is $target"
done
grep -qx /etc/apt/apt.conf.d/53smoothnas-dkms "$HERE/../smoothnas/DEBIAN/conffiles" || fail "53smoothnas-dkms is not a conffile"

# 8. The postinst runs it and can't fail because of it.
case_name=postinst
grep -qx '/usr/lib/smoothnas/dkms-autoinstall.sh || true' "$HERE/../smoothnas/DEBIAN/postinst" || fail "postinst does not run dkms-autoinstall.sh tolerantly"

if [ "$failures" -ne 0 ]; then
    echo "$failures failure(s)" >&2
    exit 1
fi
echo "dkms-autoinstall: all cases passed"
