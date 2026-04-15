/*
 * fuse_passthrough_fixup.c — LD_PRELOAD shim for stock Debian libfuse3 3.17
 *
 * Debian's libfuse 3.17 doesn't set the flags2 field in the FUSE INIT reply,
 * so the kernel never enables passthrough (FUSE_PASSTHROUGH = 1ULL << 37).
 * This shim intercepts the writev() that sends the INIT reply and patches in
 * FUSE_INIT_EXT + FUSE_PASSTHROUGH in flags2 + max_stack_depth=1.
 *
 * Build:  cc -shared -fPIC -O2 -o fuse_passthrough_fixup.so fuse_passthrough_fixup.c -ldl
 * Usage:  LD_PRELOAD=/path/to/fuse_passthrough_fixup.so tierd-fuse-ns ...
 */
#define _GNU_SOURCE
#include <dlfcn.h>
#include <sys/uio.h>
#include <string.h>
#include <stdint.h>

static ssize_t (*real_writev)(int, const struct iovec *, int);
static int g_done;

#include <stdio.h>

/* Kernel FUSE protocol constants */
#define FUSE_INIT_EXT        (1U << 30)
#define FUSE_PT_FLAGS2_BIT   (1U << 5)   /* FUSE_PASSTHROUGH = 1ULL << 37 → flags2 bit 5 */
#define OUT_HDR              16           /* sizeof(struct fuse_out_header) */
#define INITOUT_FLAGS        12           /* offset of flags   in fuse_init_out */
#define INITOUT_FLAGS2       32           /* offset of flags2  in fuse_init_out */
#define INITOUT_STACKDEPTH   36           /* offset of max_stack_depth */

ssize_t writev(int fd, const struct iovec *iov, int iovcnt)
{
    if (!real_writev)
        real_writev = dlsym(RTLD_NEXT, "writev");

    if (g_done || iovcnt < 1)
        return real_writev(fd, iov, iovcnt);

    /* Compute total length */
    size_t total = 0;
    for (int i = 0; i < iovcnt; i++)
        total += iov[i].iov_len;

    /* INIT reply: 16-byte header + ≥40 bytes of fuse_init_out (to cover flags2) */
    if (total < OUT_HDR + INITOUT_STACKDEPTH + 4)
        return real_writev(fd, iov, iovcnt);

    /* Flatten the iovecs so we can inspect and patch */
    uint8_t buf[256];
    if (total > sizeof(buf))
        return real_writev(fd, iov, iovcnt);

    size_t off = 0;
    for (int i = 0; i < iovcnt; i++) {
        memcpy(buf + off, iov[i].iov_base, iov[i].iov_len);
        off += iov[i].iov_len;
    }

    /* out_header: { uint32_t len, int32_t error, uint64_t unique } */
    int32_t  error;  memcpy(&error, buf + 4, 4);
    /* fuse_init_out starts at buf[16] */
    uint32_t major;  memcpy(&major, buf + OUT_HDR + 0, 4);
    uint32_t minor;  memcpy(&minor, buf + OUT_HDR + 4, 4);

    if (error != 0 || major != 7 || minor < 36)
        return real_writev(fd, iov, iovcnt);

    /* This is the FUSE INIT reply — patch it */
    fprintf(stderr, "fuse_passthrough_fixup: patching INIT reply "
            "(major=%u minor=%u total=%zu)\n", major, minor, total);
    g_done = 1;

    /* Set FUSE_INIT_EXT so the kernel reads flags2 */
    uint32_t flags;
    memcpy(&flags, buf + OUT_HDR + INITOUT_FLAGS, 4);
    flags |= FUSE_INIT_EXT;
    memcpy(buf + OUT_HDR + INITOUT_FLAGS, &flags, 4);

    /* Set FUSE_PASSTHROUGH in flags2 */
    uint32_t flags2;
    memcpy(&flags2, buf + OUT_HDR + INITOUT_FLAGS2, 4);
    flags2 |= FUSE_PT_FLAGS2_BIT;
    memcpy(buf + OUT_HDR + INITOUT_FLAGS2, &flags2, 4);

    /* Ensure max_stack_depth ≥ 1 (required for passthrough) */
    uint32_t depth;
    memcpy(&depth, buf + OUT_HDR + INITOUT_STACKDEPTH, 4);
    if (depth == 0) {
        depth = 1;
        memcpy(buf + OUT_HDR + INITOUT_STACKDEPTH, &depth, 4);
    }

    fprintf(stderr, "fuse_passthrough_fixup: flags=0x%08x flags2=0x%08x "
            "max_stack_depth=%u\n", flags, flags2, depth);

    struct iovec patched = { .iov_base = buf, .iov_len = total };
    return real_writev(fd, &patched, 1);
}
