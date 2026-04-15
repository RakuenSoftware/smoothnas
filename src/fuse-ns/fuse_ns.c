/*
 * tierd-fuse-ns: FUSE daemon for SmoothNAS ZFS managed tiering adapter.
 *
 * Usage: tierd-fuse-ns <socket_path> <mount_path>
 *
 * Connects to tierd's Unix socket, mounts a FUSE filesystem, and on each
 * open() queries tierd for the backing fd (delivered via SCM_RIGHTS).
 * If the kernel supports FUSE_PASSTHROUGH (Linux 6.x), reads/writes bypass
 * userspace via the passthrough mechanism; otherwise falls back to manual
 * read/write handlers.
 *
 * Binary framing protocol (8-byte header):
 *   Bytes 0-3: message type (uint32 little-endian)
 *   Bytes 4-7: payload length (uint32 little-endian)
 *   Then: payload_length bytes of payload
 *
 * Message types:
 *   OPEN_REQUEST  (1) daemon→tierd: 4-byte flags LE + null-terminated UTF-8 object key
 *   OPEN_RESPONSE (2) tierd→daemon: 4-byte request_id LE + 1-byte result + 8-byte inode LE; fd via SCM_RIGHTS
 *   RELEASE_NOTIFY(3) daemon→tierd: 8-byte inode LE
 *   HEALTH_PING   (4) tierd→daemon: 0-byte payload
 *   HEALTH_PONG   (5) daemon→tierd: 0-byte payload
 *   ERROR         (6) either direction: ASCII error string
 *   DIR_UPDATE    (7) tierd→daemon: directory tree snapshot
 */

#define FUSE_USE_VERSION 317
#include <fuse3/fuse_lowlevel.h>

/*
 * FUSE passthrough (Linux 6.9+, libfuse 3.17+): after open/create, the
 * kernel routes read/write directly to the backing fd — zero userspace
 * data copies.  The API is:
 *   backing_id = fuse_passthrough_open(req, fd);
 *   fi->backing_id = backing_id;
 *   fuse_reply_open(req, fi);        // or fuse_reply_create
 */
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>
#include <sys/socket.h>
#include <sys/un.h>
#include <sys/stat.h>
#include <fcntl.h>
#include <errno.h>
#include <limits.h>
#include <stdint.h>
#include <pthread.h>
#include <sys/uio.h>

/* -----------------------------------------------------------------------
 * Protocol constants
 * --------------------------------------------------------------------- */

#define MSG_OPEN_REQUEST    1
#define MSG_OPEN_RESPONSE   2
#define MSG_RELEASE_NOTIFY  3
#define MSG_HEALTH_PING     4
#define MSG_HEALTH_PONG     5
#define MSG_ERROR           6
#define MSG_DIR_UPDATE      7
/* MSG 8/9 = QUIESCE/QUIESCE_ACK (tierd→daemon); no handler needed */
/* MSG 10 = RELEASE (tierd→daemon); no handler needed */
#define MSG_FS_OP           11  /* daemon→tierd: mkdir/unlink/rmdir/rename */
#define MSG_FS_OP_RESPONSE  12  /* tierd→daemon: result */

#define MAX_KEY_LEN         4096
#define MAX_PAYLOAD         (64 * 1024 * 1024)

/* OPEN_RESPONSE result codes → POSIX errors */
#define RESP_SUCCESS        0
#define RESP_ENOENT         1
#define RESP_EIO            2
#define RESP_EAGAIN         3
#define RESP_EACCES         5

/* FS_OP op types */
#define FSOP_MKDIR          1
#define FSOP_UNLINK         2
#define FSOP_RMDIR          3
#define FSOP_RENAME         4

/* -----------------------------------------------------------------------
 * Global state
 * --------------------------------------------------------------------- */

static int          g_sock_fd      = -1;
static char         g_mount_path[PATH_MAX];
#ifdef FUSE_CAP_PASSTHROUGH
static int          g_passthrough  = 0;
#endif

/* Serialize all socket sends. */
static pthread_mutex_t g_sock_send_mu = PTHREAD_MUTEX_INITIALIZER;

/* -----------------------------------------------------------------------
 * In-flight OPEN_REQUEST state — supports concurrent requests via request IDs
 * --------------------------------------------------------------------- */

#define MAX_INFLIGHT_OPENS 64

typedef struct {
    uint32_t request_id;   /* unique ID for matching response to request */
    int      result_code;  /* RESP_* */
    uint64_t inode;        /* backing inode on success */
    int      backing_fd;   /* received fd on success, -1 otherwise */
    int      active;       /* 1 when slot is in use */
    int      ready;        /* set to 1 when response has been deposited */
    pthread_cond_t cond;   /* per-request condition variable */
} OpenSlot;

static pthread_mutex_t g_open_mu = PTHREAD_MUTEX_INITIALIZER;
static OpenSlot        g_open_slots[MAX_INFLIGHT_OPENS];
static uint32_t        g_next_request_id = 1;

/* -----------------------------------------------------------------------
 * In-flight FS_OP state — for concurrent mkdir/unlink/rmdir/rename requests.
 * --------------------------------------------------------------------- */

#define MAX_INFLIGHT_FSOPS 32

typedef struct {
    uint32_t request_id;
    int      active;
    int      ready;
    int      errnum;       /* 0 on success, positive errno on failure */
    uint64_t inode;        /* for mkdir: new directory inode */
    int64_t  mtime_sec;    /* for mkdir */
    uint32_t mtime_nsec;   /* for mkdir */
    pthread_cond_t cond;
} FsOpSlot;

static pthread_mutex_t g_fsop_mu = PTHREAD_MUTEX_INITIALIZER;
static FsOpSlot        g_fsop_slots[MAX_INFLIGHT_FSOPS];
static uint32_t        g_next_fsop_id = 0x80000000U; /* distinct from open IDs */

/* -----------------------------------------------------------------------
 * Directory cache: populated via DIR_UPDATE messages from tierd.
 * Protected by g_dir_rwlock.
 * --------------------------------------------------------------------- */

typedef struct {
    uint64_t inode;
    uint8_t  type;       /* 0=file, 1=dir */
    char    *path;       /* heap-allocated */
    uint32_t mode;
    uint32_t uid;
    uint32_t gid;
    uint64_t size;
    int64_t  mtime_sec;
    uint32_t mtime_nsec;
} DirCacheEntry;

typedef struct {
    DirCacheEntry *entries;
    size_t         count;
} DirCache;

static DirCache         g_dir_cache;
static pthread_rwlock_t g_dir_rwlock = PTHREAD_RWLOCK_INITIALIZER;

/* free_dir_cache frees all entries in cache (does NOT free cache itself). */
static void free_dir_cache(DirCache *c)
{
    for (size_t i = 0; i < c->count; i++)
        free(c->entries[i].path);
    free(c->entries);
    c->entries = NULL;
    c->count   = 0;
}

/* dir_cache_find_by_inode: returns pointer to entry or NULL. Caller holds rdlock. */
static const DirCacheEntry *dir_cache_find_ino(uint64_t ino)
{
    for (size_t i = 0; i < g_dir_cache.count; i++)
        if (g_dir_cache.entries[i].inode == ino)
            return &g_dir_cache.entries[i];
    return NULL;
}

/* dir_cache_find_path: returns pointer to entry or NULL. Caller holds rdlock. */
static const DirCacheEntry *dir_cache_find_path(const char *path)
{
    for (size_t i = 0; i < g_dir_cache.count; i++)
        if (strcmp(g_dir_cache.entries[i].path, path) == 0)
            return &g_dir_cache.entries[i];
    return NULL;
}

/* fill_stat_from_entry: fill struct stat from a DirCacheEntry. */
static void fill_stat_from_entry(const DirCacheEntry *e, struct stat *st)
{
    memset(st, 0, sizeof(*st));
    st->st_ino   = (ino_t)e->inode;
    st->st_mode  = (mode_t)e->mode;
    st->st_uid   = (uid_t)e->uid;
    st->st_gid   = (gid_t)e->gid;
    st->st_size  = (off_t)e->size;
    st->st_nlink = (e->type == 1) ? 2 : 1;
    st->st_mtim.tv_sec  = (time_t)e->mtime_sec;
    st->st_mtim.tv_nsec = (long)e->mtime_nsec;
}

/* -----------------------------------------------------------------------
 * Binary framing socket helpers
 * --------------------------------------------------------------------- */

/*
 * read_full: read exactly 'len' bytes from fd into buf.
 * Returns 0 on success, -1 on EOF/error.
 */
static int read_full(int fd, void *buf, size_t len)
{
    size_t done = 0;
    char *p = (char *)buf;
    while (done < len) {
        ssize_t n = read(fd, p + done, len - done);
        if (n <= 0)
            return -1;
        done += (size_t)n;
    }
    return 0;
}

/*
 * write_full: write exactly 'len' bytes from buf to fd.
 * Returns 0 on success, -1 on error.
 */
static int write_full(int fd, const void *buf, size_t len)
{
    size_t done = 0;
    const char *p = (const char *)buf;
    while (done < len) {
        ssize_t n = write(fd, p + done, len - done);
        if (n <= 0)
            return -1;
        done += (size_t)n;
    }
    return 0;
}

/* encode uint32 little-endian into buf */
static void put_le32(uint8_t *buf, uint32_t v)
{
    buf[0] = (uint8_t)(v);
    buf[1] = (uint8_t)(v >> 8);
    buf[2] = (uint8_t)(v >> 16);
    buf[3] = (uint8_t)(v >> 24);
}

/* encode uint64 little-endian into buf */
static void __attribute__((unused)) put_le64(uint8_t *buf, uint64_t v)
{
    buf[0] = (uint8_t)(v);
    buf[1] = (uint8_t)(v >> 8);
    buf[2] = (uint8_t)(v >> 16);
    buf[3] = (uint8_t)(v >> 24);
    buf[4] = (uint8_t)(v >> 32);
    buf[5] = (uint8_t)(v >> 40);
    buf[6] = (uint8_t)(v >> 48);
    buf[7] = (uint8_t)(v >> 56);
}

/* decode uint32 little-endian from buf */
static uint32_t get_le32(const uint8_t *buf)
{
    return (uint32_t)buf[0]
         | ((uint32_t)buf[1] << 8)
         | ((uint32_t)buf[2] << 16)
         | ((uint32_t)buf[3] << 24);
}

/* decode uint64 little-endian from buf */
static uint64_t get_le64(const uint8_t *buf)
{
    return (uint64_t)buf[0]
         | ((uint64_t)buf[1] << 8)
         | ((uint64_t)buf[2] << 16)
         | ((uint64_t)buf[3] << 24)
         | ((uint64_t)buf[4] << 32)
         | ((uint64_t)buf[5] << 40)
         | ((uint64_t)buf[6] << 48)
         | ((uint64_t)buf[7] << 56);
}

/*
 * send_msg: send a binary-framed message (header + payload).
 * Caller must hold g_sock_send_mu.
 * Returns 0 on success, -1 on error.
 */
static int send_msg_locked(uint32_t msg_type, const void *payload, uint32_t payload_len)
{
    uint8_t hdr[8];
    put_le32(hdr,     msg_type);
    put_le32(hdr + 4, payload_len);

    if (write_full(g_sock_fd, hdr, 8) < 0)
        return -1;
    if (payload_len > 0 && write_full(g_sock_fd, payload, payload_len) < 0)
        return -1;
    return 0;
}

/*
 * send_msg: thread-safe wrapper that acquires the send mutex.
 */
static int send_msg(uint32_t msg_type, const void *payload, uint32_t payload_len)
{
    pthread_mutex_lock(&g_sock_send_mu);
    int r = send_msg_locked(msg_type, payload, payload_len);
    pthread_mutex_unlock(&g_sock_send_mu);
    return r;
}

/*
 * send_error: send an ERROR(6) message with a short string.
 */
static void send_error(const char *msg)
{
    send_msg(MSG_ERROR, msg, (uint32_t)strlen(msg));
}

/*
 * tierd_open_request: send OPEN_REQUEST and wait for OPEN_RESPONSE.
 * Supports concurrent in-flight requests via per-request slots and IDs.
 * Returns backing_fd (>= 0) on success, -errno on failure.
 * On success, *inode_out is set to the backing inode.
 * open_flags are the POSIX O_RDONLY/O_WRONLY/O_RDWR flags to open with.
 */
static int tierd_open_request(const char *key, uint32_t open_flags, uint64_t *inode_out)
{
    size_t key_len = strlen(key) + 1; /* include null terminator */
    if (key_len > MAX_KEY_LEN) {
        send_error("key too long");
        exit(1);
    }

    /* Allocate a slot for this request */
    pthread_mutex_lock(&g_open_mu);
    int slot_idx = -1;
    for (int i = 0; i < MAX_INFLIGHT_OPENS; i++) {
        if (!g_open_slots[i].active) {
            slot_idx = i;
            break;
        }
    }
    if (slot_idx < 0) {
        /* All slots full — wait for one to free up */
        while (slot_idx < 0) {
            /* Use slot 0's cond as a general "slot freed" signal */
            pthread_cond_wait(&g_open_slots[0].cond, &g_open_mu);
            for (int i = 0; i < MAX_INFLIGHT_OPENS; i++) {
                if (!g_open_slots[i].active) {
                    slot_idx = i;
                    break;
                }
            }
        }
    }

    OpenSlot *slot = &g_open_slots[slot_idx];
    slot->request_id  = g_next_request_id++;
    slot->result_code = RESP_EIO;
    slot->inode       = 0;
    slot->backing_fd  = -1;
    slot->active      = 1;
    slot->ready       = 0;

    uint32_t req_id = slot->request_id;
    pthread_mutex_unlock(&g_open_mu);

    /* Build payload: [4-byte request_id LE] [4-byte flags LE] [null-terminated key] */
    uint32_t payload_len = 4 + 4 + (uint32_t)key_len;
    uint8_t *payload = malloc(payload_len);
    if (!payload) {
        pthread_mutex_lock(&g_open_mu);
        slot->active = 0;
        pthread_cond_broadcast(&g_open_slots[0].cond);
        pthread_mutex_unlock(&g_open_mu);
        return -ENOMEM;
    }
    put_le32(payload, req_id);
    put_le32(payload + 4, open_flags);
    memcpy(payload + 8, key, key_len);

    /* Send OPEN_REQUEST */
    pthread_mutex_lock(&g_sock_send_mu);
    int send_r = send_msg_locked(MSG_OPEN_REQUEST, payload, payload_len);
    pthread_mutex_unlock(&g_sock_send_mu);
    free(payload);

    if (send_r < 0) {
        pthread_mutex_lock(&g_open_mu);
        slot->active = 0;
        pthread_cond_broadcast(&g_open_slots[0].cond);
        pthread_mutex_unlock(&g_open_mu);
        return -EIO;
    }

    /* Wait for reader thread to deposit OPEN_RESPONSE for our request ID */
    pthread_mutex_lock(&g_open_mu);
    while (!slot->ready)
        pthread_cond_wait(&slot->cond, &g_open_mu);

    int result_code = slot->result_code;
    uint64_t inode  = slot->inode;
    int backing_fd  = slot->backing_fd;
    slot->active    = 0;
    pthread_cond_broadcast(&g_open_slots[0].cond); /* wake anyone waiting for a slot */
    pthread_mutex_unlock(&g_open_mu);

    if (result_code != RESP_SUCCESS) {
        int err;
        switch (result_code) {
        case RESP_ENOENT: err = ENOENT; break;
        case RESP_EAGAIN: err = EAGAIN; break;
        case RESP_EACCES: err = EACCES; break;
        default:          err = EIO;    break;
        }
        return -err;
    }

    /* Validate: fstat the received fd to confirm it is usable.
     * We do NOT compare st_ino against inode here because tierd uses
     * path-derived virtual inodes (FNV-64a hash) in OPEN_RESPONSE and
     * DIR_UPDATE to avoid collisions between backing tiers that share
     * the same physical inode namespace independently. */
    struct stat st;
    if (fstat(backing_fd, &st) < 0) {
        close(backing_fd);
        send_error("fstat failed on received fd");
        return -EIO;
    }

    *inode_out = inode;
    return backing_fd;
}

/*
 * tierd_release_notify: send RELEASE_NOTIFY with backing inode.
 */
static void tierd_release_notify(uint64_t inode)
{
    uint8_t payload[8];
    payload[0] = (uint8_t)(inode);
    payload[1] = (uint8_t)(inode >> 8);
    payload[2] = (uint8_t)(inode >> 16);
    payload[3] = (uint8_t)(inode >> 24);
    payload[4] = (uint8_t)(inode >> 32);
    payload[5] = (uint8_t)(inode >> 40);
    payload[6] = (uint8_t)(inode >> 48);
    payload[7] = (uint8_t)(inode >> 56);
    send_msg(MSG_RELEASE_NOTIFY, payload, 8);
}

/*
 * send_fsop: send MSG_FS_OP to tierd and block until MSG_FS_OP_RESPONSE arrives.
 *
 * Payload: [4-byte req_id] [1-byte op] [4-byte mode] [path1\0] [path2\0]
 *
 * On success returns 0 and (for FSOP_MKDIR) populates *inode_out,
 * *mtime_sec_out, *mtime_nsec_out. On failure returns a positive errno value.
 */
static int send_fsop(int op, uint32_t mode,
                     const char *path1, const char *path2,
                     uint64_t *inode_out, int64_t *mtime_sec_out, uint32_t *mtime_nsec_out)
{
    size_t p1len = strlen(path1) + 1; /* include null terminator */
    size_t p2len = path2 ? strlen(path2) + 1 : 1; /* at least one null byte */

    /* Payload: req_id(4) + op(1) + mode(4) + path1 + path2 */
    uint32_t payload_len = 4 + 1 + 4 + (uint32_t)p1len + (uint32_t)p2len;
    uint8_t *payload = malloc(payload_len);
    if (!payload)
        return ENOMEM;

    /* Allocate a FsOpSlot */
    pthread_mutex_lock(&g_fsop_mu);
    int slot_idx = -1;
    for (int i = 0; i < MAX_INFLIGHT_FSOPS; i++) {
        if (!g_fsop_slots[i].active) {
            slot_idx = i;
            break;
        }
    }
    while (slot_idx < 0) {
        pthread_cond_wait(&g_fsop_slots[0].cond, &g_fsop_mu);
        for (int i = 0; i < MAX_INFLIGHT_FSOPS; i++) {
            if (!g_fsop_slots[i].active) {
                slot_idx = i;
                break;
            }
        }
    }

    FsOpSlot *slot = &g_fsop_slots[slot_idx];
    slot->request_id = g_next_fsop_id++;
    slot->active     = 1;
    slot->ready      = 0;
    slot->errnum     = EIO;
    slot->inode      = 0;
    slot->mtime_sec  = 0;
    slot->mtime_nsec = 0;
    uint32_t req_id = slot->request_id;
    pthread_mutex_unlock(&g_fsop_mu);

    /* Build payload */
    uint32_t off = 0;
    put_le32(payload + off, req_id);       off += 4;
    payload[off] = (uint8_t)op;            off += 1;
    put_le32(payload + off, mode);         off += 4;
    memcpy(payload + off, path1, p1len);   off += (uint32_t)p1len;
    if (path2)
        memcpy(payload + off, path2, p2len);
    else
        payload[off] = 0;

    /* Send MSG_FS_OP */
    pthread_mutex_lock(&g_sock_send_mu);
    int send_r = send_msg_locked(MSG_FS_OP, payload, payload_len);
    pthread_mutex_unlock(&g_sock_send_mu);
    free(payload);

    if (send_r < 0) {
        pthread_mutex_lock(&g_fsop_mu);
        slot->active = 0;
        pthread_cond_broadcast(&g_fsop_slots[0].cond);
        pthread_mutex_unlock(&g_fsop_mu);
        return EIO;
    }

    /* Wait for response */
    pthread_mutex_lock(&g_fsop_mu);
    while (!slot->ready)
        pthread_cond_wait(&slot->cond, &g_fsop_mu);

    int errnum         = slot->errnum;
    uint64_t inode     = slot->inode;
    int64_t mtime_sec  = slot->mtime_sec;
    uint32_t mtime_nsec = slot->mtime_nsec;
    slot->active = 0;
    pthread_cond_broadcast(&g_fsop_slots[0].cond);
    pthread_mutex_unlock(&g_fsop_mu);

    if (errnum == 0) {
        if (inode_out)     *inode_out     = inode;
        if (mtime_sec_out) *mtime_sec_out = mtime_sec;
        if (mtime_nsec_out)*mtime_nsec_out = mtime_nsec;
    }
    return errnum;
}

/* -----------------------------------------------------------------------
 * Local directory cache mutation helpers
 * --------------------------------------------------------------------- */

/*
 * dir_cache_add: add or update an entry in the local dir cache.
 * Takes wrlock internally.  The entry's path is strdup'd.
 */
static void dir_cache_add(const DirCacheEntry *e_new)
{
    pthread_rwlock_wrlock(&g_dir_rwlock);

    /* Update if path already exists */
    for (size_t i = 0; i < g_dir_cache.count; i++) {
        if (strcmp(g_dir_cache.entries[i].path, e_new->path) == 0) {
            free(g_dir_cache.entries[i].path);
            g_dir_cache.entries[i] = *e_new;
            g_dir_cache.entries[i].path = strdup(e_new->path);
            pthread_rwlock_unlock(&g_dir_rwlock);
            return;
        }
    }

    /* Grow array and append */
    DirCacheEntry *new_arr = realloc(g_dir_cache.entries,
                                     (g_dir_cache.count + 1) * sizeof(DirCacheEntry));
    if (!new_arr) {
        pthread_rwlock_unlock(&g_dir_rwlock);
        return;
    }
    g_dir_cache.entries = new_arr;
    g_dir_cache.entries[g_dir_cache.count] = *e_new;
    g_dir_cache.entries[g_dir_cache.count].path = strdup(e_new->path);
    g_dir_cache.count++;

    pthread_rwlock_unlock(&g_dir_rwlock);
}

/*
 * dir_cache_remove: remove the entry with the given path from the dir cache.
 * Takes wrlock internally.
 */
static void dir_cache_remove(const char *path)
{
    pthread_rwlock_wrlock(&g_dir_rwlock);
    for (size_t i = 0; i < g_dir_cache.count; i++) {
        if (strcmp(g_dir_cache.entries[i].path, path) == 0) {
            free(g_dir_cache.entries[i].path);
            /* Replace with last entry and shrink */
            g_dir_cache.entries[i] = g_dir_cache.entries[g_dir_cache.count - 1];
            g_dir_cache.count--;
            break;
        }
    }
    pthread_rwlock_unlock(&g_dir_rwlock);
}

/* -----------------------------------------------------------------------
 * DIR_UPDATE parsing
 * --------------------------------------------------------------------- */

/*
 * apply_dir_update: parse a DIR_UPDATE payload and atomically replace the
 * directory cache.
 */
static void apply_dir_update(const uint8_t *payload, uint32_t payload_len)
{
    /* Count entries to pre-allocate */
    size_t count = 0;
    uint32_t off = 0;
    while (off + 11 <= payload_len) {
        uint16_t path_len = (uint16_t)payload[off + 9]
                          | ((uint16_t)payload[off + 10] << 8);
        uint32_t entry_size = 8 + 1 + 2 + (uint32_t)path_len + 1 + 4 + 4 + 4 + 8 + 8 + 4;
        if (off + entry_size > payload_len)
            break;
        count++;
        off += entry_size;
    }

    if (count == 0) {
        /* Empty update: clear the cache */
        pthread_rwlock_wrlock(&g_dir_rwlock);
        free_dir_cache(&g_dir_cache);
        pthread_rwlock_unlock(&g_dir_rwlock);
        return;
    }

    DirCacheEntry *entries = calloc(count, sizeof(DirCacheEntry));
    if (!entries) {
        fprintf(stderr, "tierd-fuse-ns: apply_dir_update: OOM\n");
        return;
    }

    off = 0;
    size_t idx = 0;
    while (idx < count && off + 11 <= payload_len) {
        uint64_t inode    = get_le64(payload + off); off += 8;
        uint8_t  type     = payload[off++];
        uint16_t path_len = (uint16_t)payload[off] | ((uint16_t)payload[off + 1] << 8);
        off += 2;

        if (off + path_len + 1 > payload_len)
            break;

        char *path = malloc(path_len + 1);
        if (!path) {
            fprintf(stderr, "tierd-fuse-ns: apply_dir_update: OOM for path\n");
            break;
        }
        memcpy(path, payload + off, path_len);
        path[path_len] = '\0';
        off += path_len + 1; /* skip null terminator in payload */

        if (off + 4 + 4 + 4 + 8 + 8 + 4 > payload_len) {
            free(path);
            break;
        }

        uint32_t mode        = get_le32(payload + off); off += 4;
        uint32_t uid         = get_le32(payload + off); off += 4;
        uint32_t gid         = get_le32(payload + off); off += 4;
        uint64_t size        = get_le64(payload + off); off += 8;
        int64_t  mtime_sec   = (int64_t)get_le64(payload + off); off += 8;
        uint32_t mtime_nsec  = get_le32(payload + off); off += 4;

        entries[idx].inode      = inode;
        entries[idx].type       = type;
        entries[idx].path       = path;
        entries[idx].mode       = mode;
        entries[idx].uid        = uid;
        entries[idx].gid        = gid;
        entries[idx].size       = size;
        entries[idx].mtime_sec  = mtime_sec;
        entries[idx].mtime_nsec = mtime_nsec;
        idx++;
    }

    /* Atomically swap under write lock */
    DirCache new_cache = { .entries = entries, .count = idx };

    pthread_rwlock_wrlock(&g_dir_rwlock);
    DirCache old_cache = g_dir_cache;
    g_dir_cache = new_cache;
    pthread_rwlock_unlock(&g_dir_rwlock);

    free_dir_cache(&old_cache);
}

/* -----------------------------------------------------------------------
 * Reader thread: handles tierd→daemon messages (HEALTH_PING, DIR_UPDATE,
 * OPEN_RESPONSE).
 * --------------------------------------------------------------------- */

/* -----------------------------------------------------------------------
 * Reader thread: handles tierd→daemon messages with SCM_RIGHTS for OPEN_RESPONSE
 * --------------------------------------------------------------------- */

static void *reader_thread_main(void *arg)
{
    (void)arg;

    for (;;) {
        /* Read 8-byte header */
        uint8_t hdr[8];
        if (read_full(g_sock_fd, hdr, 8) < 0) {
            fprintf(stderr, "tierd-fuse-ns: reader: connection closed\n");
            exit(1);
        }

        uint32_t msg_type   = get_le32(hdr);
        uint32_t payload_len = get_le32(hdr + 4);

        if (payload_len > MAX_PAYLOAD) {
            send_error("payload too large");
            exit(1);
        }

        if (msg_type == MSG_OPEN_RESPONSE) {
            /* Must use recvmsg to receive SCM_RIGHTS along with payload */
            /* New format: 4-byte request_id LE + 1-byte result + 8-byte inode LE = 13 bytes */
            uint8_t pbuf[16];
            uint8_t ctrl_buf[CMSG_SPACE(sizeof(int))];

            struct iovec iov = {
                .iov_base = pbuf,
                .iov_len  = (payload_len < sizeof(pbuf)) ? payload_len : sizeof(pbuf) - 1,
            };
            struct msghdr msg = {
                .msg_iov        = &iov,
                .msg_iovlen     = 1,
                .msg_control    = ctrl_buf,
                .msg_controllen = sizeof(ctrl_buf),
            };

            ssize_t n = recvmsg(g_sock_fd, &msg, 0);
            if (n < 0) {
                fprintf(stderr, "tierd-fuse-ns: recvmsg OPEN_RESPONSE: %s\n",
                        strerror(errno));
                exit(1);
            }

            /* Parse request_id (first 4 bytes) */
            uint32_t resp_req_id = (n >= 4) ? get_le32(pbuf) : 0;
            uint8_t result_code  = (n >= 5) ? pbuf[4] : RESP_EIO;
            uint64_t inode       = 0;
            int backing_fd       = -1;

            if (result_code == RESP_SUCCESS && n >= 13) {
                inode = get_le64(pbuf + 5);

                /* Extract fd from SCM_RIGHTS */
                struct cmsghdr *cmsg = CMSG_FIRSTHDR(&msg);
                if (cmsg && cmsg->cmsg_level == SOL_SOCKET &&
                    cmsg->cmsg_type == SCM_RIGHTS) {
                    memcpy(&backing_fd, CMSG_DATA(cmsg), sizeof(int));
                } else {
                    fprintf(stderr, "tierd-fuse-ns: no SCM_RIGHTS in OPEN_RESPONSE\n");
                    result_code = RESP_EIO;
                }
            }

            /* If payload was larger than our buffer, drain the rest */
            if ((uint32_t)n < payload_len) {
                size_t remaining = payload_len - (size_t)n;
                uint8_t drain[256];
                while (remaining > 0) {
                    size_t chunk = (remaining < sizeof(drain)) ? remaining : sizeof(drain);
                    if (read_full(g_sock_fd, drain, chunk) < 0)
                        break;
                    remaining -= chunk;
                }
            }

            /* Dispatch to the correct slot by request_id */
            pthread_mutex_lock(&g_open_mu);
            int found = 0;
            for (int i = 0; i < MAX_INFLIGHT_OPENS; i++) {
                if (g_open_slots[i].active && g_open_slots[i].request_id == resp_req_id) {
                    g_open_slots[i].result_code = result_code;
                    g_open_slots[i].inode       = inode;
                    g_open_slots[i].backing_fd  = backing_fd;
                    g_open_slots[i].ready       = 1;
                    pthread_cond_signal(&g_open_slots[i].cond);
                    found = 1;
                    break;
                }
            }
            pthread_mutex_unlock(&g_open_mu);

            if (!found) {
                fprintf(stderr, "tierd-fuse-ns: OPEN_RESPONSE for unknown request_id %u\n",
                        resp_req_id);
                if (backing_fd >= 0)
                    close(backing_fd);
            }
            continue;
        }

        /* For all other messages, read payload normally */
        uint8_t *payload = NULL;
        if (payload_len > 0) {
            payload = malloc(payload_len);
            if (!payload) {
                fprintf(stderr, "tierd-fuse-ns: reader: OOM\n");
                exit(1);
            }
            if (read_full(g_sock_fd, payload, payload_len) < 0) {
                free(payload);
                fprintf(stderr, "tierd-fuse-ns: reader: read payload failed\n");
                exit(1);
            }
        }

        switch (msg_type) {
        case MSG_HEALTH_PING:
            send_msg(MSG_HEALTH_PONG, NULL, 0);
            break;

        case MSG_DIR_UPDATE:
            apply_dir_update(payload, payload_len);
            break;

        case MSG_FS_OP_RESPONSE: {
            /*
             * Payload: [4-byte req_id] [4-byte errno] [8-byte inode]
             *          [8-byte mtime_sec] [4-byte mtime_nsec]  = 28 bytes
             */
            if (payload_len < 8) {
                fprintf(stderr, "tierd-fuse-ns: FS_OP_RESPONSE too short (%u)\n", payload_len);
                break;
            }
            uint32_t resp_req_id = get_le32(payload);
            uint32_t resp_errno  = get_le32(payload + 4);
            uint64_t resp_inode  = (payload_len >= 16) ? get_le64(payload + 8)  : 0;
            int64_t  resp_mtime_sec  = (payload_len >= 24) ? (int64_t)get_le64(payload + 16) : 0;
            uint32_t resp_mtime_nsec = (payload_len >= 28) ? get_le32(payload + 24) : 0;

            pthread_mutex_lock(&g_fsop_mu);
            int found = 0;
            for (int i = 0; i < MAX_INFLIGHT_FSOPS; i++) {
                if (g_fsop_slots[i].active && g_fsop_slots[i].request_id == resp_req_id) {
                    g_fsop_slots[i].errnum     = (int)resp_errno;
                    g_fsop_slots[i].inode      = resp_inode;
                    g_fsop_slots[i].mtime_sec  = resp_mtime_sec;
                    g_fsop_slots[i].mtime_nsec = resp_mtime_nsec;
                    g_fsop_slots[i].ready      = 1;
                    pthread_cond_signal(&g_fsop_slots[i].cond);
                    found = 1;
                    break;
                }
            }
            pthread_mutex_unlock(&g_fsop_mu);

            if (!found)
                fprintf(stderr, "tierd-fuse-ns: FS_OP_RESPONSE for unknown req_id %u\n",
                        resp_req_id);
            break;
        }

        case MSG_ERROR:
            fprintf(stderr, "tierd-fuse-ns: ERROR from tierd: %.*s\n",
                    (int)payload_len, payload ? (char *)payload : "");
            break;

        default:
            /* MSG 8 (QUIESCE), 10 (RELEASE) — silently ignore */
            if (msg_type != 8 && msg_type != 10) {
                fprintf(stderr, "tierd-fuse-ns: unknown message type %u\n", msg_type);
                send_error("unknown message type");
            }
            break;
        }

        free(payload);
    }

    return NULL;
}

/* -----------------------------------------------------------------------
 * Synthetic stat helpers
 * --------------------------------------------------------------------- */

static void fill_root_stat(struct stat *st)
{
    memset(st, 0, sizeof(*st));
    st->st_ino   = 1;
    st->st_mode  = S_IFDIR | 0755;
    st->st_nlink = 2;
}

static void fill_synth_stat(fuse_ino_t ino, struct stat *st)
{
    memset(st, 0, sizeof(*st));
    st->st_ino   = ino;
    st->st_mode  = S_IFREG | 0644;
    st->st_nlink = 1;
    st->st_size  = 0;
}

/* -----------------------------------------------------------------------
 * Opendir snapshot: each opendir stores a copy of the current cache so
 * in-flight readdir is not disrupted by a concurrent DIR_UPDATE swap.
 * --------------------------------------------------------------------- */

typedef struct {
    DirCacheEntry *entries;
    size_t         count;
    fuse_ino_t     dir_ino;  /* inode of the opened directory */
} DirSnapshot;

static DirSnapshot *snapshot_create(fuse_ino_t dir_ino)
{
    DirSnapshot *snap = malloc(sizeof(DirSnapshot));
    if (!snap)
        return NULL;

    pthread_rwlock_rdlock(&g_dir_rwlock);
    size_t count = g_dir_cache.count;
    DirCacheEntry *entries = NULL;
    if (count > 0) {
        entries = calloc(count, sizeof(DirCacheEntry));
        if (!entries) {
            pthread_rwlock_unlock(&g_dir_rwlock);
            free(snap);
            return NULL;
        }
        for (size_t i = 0; i < count; i++) {
            entries[i] = g_dir_cache.entries[i];
            entries[i].path = strdup(g_dir_cache.entries[i].path);
        }
    }
    pthread_rwlock_unlock(&g_dir_rwlock);

    snap->entries = entries;
    snap->count   = count;
    snap->dir_ino = dir_ino;
    return snap;
}

static void snapshot_free(DirSnapshot *snap)
{
    if (!snap)
        return;
    for (size_t i = 0; i < snap->count; i++)
        free(snap->entries[i].path);
    free(snap->entries);
    free(snap);
}

/* -----------------------------------------------------------------------
 * FUSE low-level operations
 * --------------------------------------------------------------------- */

static void fuse_ns_init(void *userdata, struct fuse_conn_info *conn)
{
    (void)userdata;

#ifdef FUSE_CAP_PASSTHROUGH
    if (conn->capable & FUSE_CAP_PASSTHROUGH) {
        conn->want |= FUSE_CAP_PASSTHROUGH;
        g_passthrough = 1;
        fprintf(stderr, "tierd-fuse-ns: FUSE passthrough enabled\n");
    }
#endif

    /* Writeback cache and passthrough are mutually exclusive in the Linux
     * kernel (process_init_reply skips passthrough setup if writeback_cache
     * is set).  Only enable writeback_cache as a fallback. */
#ifdef FUSE_CAP_WRITEBACK_CACHE
    if (!g_passthrough && (conn->capable & FUSE_CAP_WRITEBACK_CACHE)) {
        conn->want |= FUSE_CAP_WRITEBACK_CACHE;
        fprintf(stderr, "tierd-fuse-ns: writeback cache enabled (passthrough unavailable)\n");
    }
#endif

#ifdef FUSE_CAP_SPLICE_WRITE
    if (conn->capable & FUSE_CAP_SPLICE_WRITE)
        conn->want |= FUSE_CAP_SPLICE_WRITE;
#endif
#ifdef FUSE_CAP_SPLICE_READ
    if (conn->capable & FUSE_CAP_SPLICE_READ)
        conn->want |= FUSE_CAP_SPLICE_READ;
#endif

    /* Maximize per-request write size — reduces syscall overhead for
     * large sequential writes when passthrough is not active. */
    if (conn->max_write < 1048576)
        conn->max_write = 1048576;
}

static void fuse_ns_destroy(void *userdata)
{
    (void)userdata;
    /* nothing to clean up beyond normal process teardown */
}

static void fuse_ns_lookup(fuse_req_t req, fuse_ino_t parent, const char *name)
{
    /* Serve lookup from directory cache */
    pthread_rwlock_rdlock(&g_dir_rwlock);

    /* Find the parent entry to construct the full path */
    const DirCacheEntry *parent_e = (parent == 1) ? NULL : dir_cache_find_ino(parent);

    char path[PATH_MAX];
    if (parent == 1) {
        snprintf(path, sizeof(path), "%s", name);
    } else if (parent_e) {
        snprintf(path, sizeof(path), "%s/%s", parent_e->path, name);
    } else {
        pthread_rwlock_unlock(&g_dir_rwlock);
        fuse_reply_err(req, ENOENT);
        return;
    }

    const DirCacheEntry *e = dir_cache_find_path(path);
    if (!e) {
        pthread_rwlock_unlock(&g_dir_rwlock);
        fuse_reply_err(req, ENOENT);
        return;
    }

    struct fuse_entry_param ep;
    memset(&ep, 0, sizeof(ep));
    ep.ino           = (fuse_ino_t)e->inode;
    ep.attr_timeout  = 1.0;
    ep.entry_timeout = 1.0;
    fill_stat_from_entry(e, &ep.attr);
    pthread_rwlock_unlock(&g_dir_rwlock);

    fuse_reply_entry(req, &ep);
}

static void fuse_ns_getattr(fuse_req_t req, fuse_ino_t ino,
                            struct fuse_file_info *fi)
{
    (void)fi;

    if (ino == 1) {
        struct stat st;
        fill_root_stat(&st);
        fuse_reply_attr(req, &st, 1.0);
        return;
    }

    pthread_rwlock_rdlock(&g_dir_rwlock);
    const DirCacheEntry *e = dir_cache_find_ino(ino);
    if (!e) {
        pthread_rwlock_unlock(&g_dir_rwlock);
        /* Return synthetic attrs so already-open files remain accessible */
        struct stat st;
        fill_synth_stat(ino, &st);
        fuse_reply_attr(req, &st, 1.0);
        return;
    }
    struct stat st;
    fill_stat_from_entry(e, &st);
    pthread_rwlock_unlock(&g_dir_rwlock);

    fuse_reply_attr(req, &st, 1.0);
}

static void fuse_ns_setattr(fuse_req_t req, fuse_ino_t ino, struct stat *attr,
                            int to_set, struct fuse_file_info *fi)
{
    (void)to_set;
    (void)fi;
    (void)attr;
    /* TODO(proposal-05): forward setattr to tierd */

    struct stat st;
    if (ino == 1) {
        fill_root_stat(&st);
        fuse_reply_attr(req, &st, 1.0);
        return;
    }

    pthread_rwlock_rdlock(&g_dir_rwlock);
    const DirCacheEntry *e = dir_cache_find_ino(ino);
    if (!e) {
        pthread_rwlock_unlock(&g_dir_rwlock);
        /* Return synthetic attrs so setattr on freshly-created files works */
        fill_synth_stat(ino, &st);
        fuse_reply_attr(req, &st, 1.0);
        return;
    }
    fill_stat_from_entry(e, &st);
    pthread_rwlock_unlock(&g_dir_rwlock);

    fuse_reply_attr(req, &st, 1.0);
}

static void fuse_ns_open(fuse_req_t req, fuse_ino_t ino,
                         struct fuse_file_info *fi)
{
    /* Resolve key from directory cache */
    pthread_rwlock_rdlock(&g_dir_rwlock);
    const DirCacheEntry *e = dir_cache_find_ino(ino);
    char key[PATH_MAX];
    if (e) {
        strncpy(key, e->path, PATH_MAX - 1);
        key[PATH_MAX - 1] = '\0';
    }
    int found = (e != NULL);
    pthread_rwlock_unlock(&g_dir_rwlock);

    if (!found) {
        fuse_reply_err(req, ENOENT);
        return;
    }

    /* Map FUSE open flags to POSIX flags for the backing file */
    uint32_t open_flags = (uint32_t)(fi->flags & O_ACCMODE);

    uint64_t inode = 0;
    int backing_fd = tierd_open_request(key, open_flags, &inode);
    if (backing_fd < 0) {
        fuse_reply_err(req, -backing_fd);
        return;
    }

    fi->fh = (uint64_t)(uintptr_t)backing_fd;

#ifdef FUSE_CAP_PASSTHROUGH
    if (g_passthrough) {
        /* Re-open via /proc/self/fd/N to get a fresh struct file owned
         * by this process rather than the SCM_RIGHTS-inherited one. */
        char proc_path[64];
        snprintf(proc_path, sizeof(proc_path), "/proc/self/fd/%d", backing_fd);
        int local_fd = open(proc_path, (fi->flags & O_ACCMODE) | O_CLOEXEC);
        if (local_fd >= 0) {
            int bid = fuse_passthrough_open(req, local_fd);
            if (bid > 0)
                fi->backing_id = bid;
            close(local_fd);
        }
    }
#endif

    fuse_reply_open(req, fi);
}

static void fuse_ns_create(fuse_req_t req, fuse_ino_t parent,
                           const char *name, mode_t mode,
                           struct fuse_file_info *fi)
{
    (void)mode;

    /* Build the full object key: parentPath/name (or just name at root). */
    char key[PATH_MAX];
    if (parent == 1) {
        snprintf(key, sizeof(key), "%s", name);
    } else {
        pthread_rwlock_rdlock(&g_dir_rwlock);
        const DirCacheEntry *parent_e = dir_cache_find_ino(parent);
        if (!parent_e) {
            pthread_rwlock_unlock(&g_dir_rwlock);
            fuse_reply_err(req, ENOENT);
            return;
        }
        snprintf(key, sizeof(key), "%s/%s", parent_e->path, name);
        pthread_rwlock_unlock(&g_dir_rwlock);
    }

    /* create() always needs write access and implies O_CREAT so the
     * tierd socket handler knows to auto-register the file. */
    uint32_t open_flags = O_RDWR | O_CREAT;

    uint64_t inode = 0;
    int backing_fd = tierd_open_request(key, open_flags, &inode);
    if (backing_fd < 0) {
        fuse_reply_err(req, -backing_fd);
        return;
    }

    /* Stat the backing fd to get real attributes */
    struct stat backing_st;
    memset(&backing_st, 0, sizeof(backing_st));
    fstat(backing_fd, &backing_st);

    /* Add the new file to the dir cache so subsequent open() calls work */
    DirCacheEntry file_e;
    memset(&file_e, 0, sizeof(file_e));
    file_e.inode      = inode;
    file_e.type       = 0; /* file */
    file_e.path       = key; /* dir_cache_add will strdup */
    file_e.mode       = backing_st.st_mode ? (uint32_t)backing_st.st_mode : (S_IFREG | 0644);
    file_e.uid        = fuse_req_ctx(req)->uid;
    file_e.gid        = fuse_req_ctx(req)->gid;
    file_e.size       = (uint64_t)backing_st.st_size;
    file_e.mtime_sec  = (int64_t)backing_st.st_mtim.tv_sec;
    file_e.mtime_nsec = (uint32_t)backing_st.st_mtim.tv_nsec;
    dir_cache_add(&file_e);

    struct fuse_entry_param e;
    memset(&e, 0, sizeof(e));
    e.ino           = (fuse_ino_t)inode;
    e.attr_timeout  = 1.0;
    e.entry_timeout = 1.0;
    fill_stat_from_entry(&file_e, &e.attr);

    fi->fh = (uint64_t)(uintptr_t)backing_fd;

#ifdef FUSE_CAP_PASSTHROUGH
    if (g_passthrough) {
        char proc_path[64];
        snprintf(proc_path, sizeof(proc_path), "/proc/self/fd/%d", backing_fd);
        int local_fd = open(proc_path, O_RDWR | O_CLOEXEC);
        if (local_fd >= 0) {
            int bid = fuse_passthrough_open(req, local_fd);
            if (bid > 0)
                fi->backing_id = bid;
            close(local_fd);
        }
    }
#endif

    fuse_reply_create(req, &e, fi);
}

static void fuse_ns_read(fuse_req_t req, fuse_ino_t ino, size_t size,
                         off_t off, struct fuse_file_info *fi)
{
    (void)ino;

    int fd = (int)(uintptr_t)fi->fh;
    if (fd < 0) {
        fuse_reply_err(req, EBADF);
        return;
    }

    char  stack_buf[65536];
    char *buf = (size <= sizeof(stack_buf)) ? stack_buf : malloc(size);
    if (!buf) {
        fuse_reply_err(req, ENOMEM);
        return;
    }

    ssize_t n = pread(fd, buf, size, off);
    if (n < 0) {
        int err = errno;
        if (buf != stack_buf) free(buf);
        fuse_reply_err(req, err);
        return;
    }

    fuse_reply_buf(req, buf, (size_t)n);
    if (buf != stack_buf) free(buf);
}

static void fuse_ns_write(fuse_req_t req, fuse_ino_t ino, const char *buf,
                          size_t size, off_t off, struct fuse_file_info *fi)
{
    (void)ino;

    int fd = (int)(uintptr_t)fi->fh;
    if (fd < 0) {
        fuse_reply_err(req, EBADF);
        return;
    }

    ssize_t n = pwrite(fd, buf, size, off);
    if (n < 0) {
        fuse_reply_err(req, errno);
        return;
    }

    fuse_reply_write(req, (size_t)n);
}

static void fuse_ns_release(fuse_req_t req, fuse_ino_t ino,
                            struct fuse_file_info *fi)
{
    int fd = (int)(uintptr_t)fi->fh;

    /* Get backing inode from fstat before closing */
    uint64_t backing_ino = (uint64_t)ino; /* fallback: use FUSE ino */
    if (fd >= 0) {
        struct stat st;
        if (fstat(fd, &st) == 0)
            backing_ino = (uint64_t)st.st_ino;
        close(fd);
    }

    tierd_release_notify(backing_ino);
    fuse_reply_err(req, 0);
}

static void fuse_ns_unlink(fuse_req_t req, fuse_ino_t parent, const char *name)
{
    /* Build full path from parent dir cache entry + name */
    char path[PATH_MAX];
    if (parent == 1) {
        snprintf(path, sizeof(path), "%s", name);
    } else {
        pthread_rwlock_rdlock(&g_dir_rwlock);
        const DirCacheEntry *parent_e = dir_cache_find_ino(parent);
        if (!parent_e) {
            pthread_rwlock_unlock(&g_dir_rwlock);
            fuse_reply_err(req, ENOENT);
            return;
        }
        snprintf(path, sizeof(path), "%s/%s", parent_e->path, name);
        pthread_rwlock_unlock(&g_dir_rwlock);
    }

    int errnum = send_fsop(FSOP_UNLINK, 0, path, NULL, NULL, NULL, NULL);
    if (errnum == 0) {
        dir_cache_remove(path);
        fuse_reply_err(req, 0);
    } else {
        fuse_reply_err(req, errnum);
    }
}

static void fuse_ns_mkdir(fuse_req_t req, fuse_ino_t parent, const char *name,
                          mode_t mode)
{
    /* Build full path from parent dir cache entry + name */
    char path[PATH_MAX];
    if (parent == 1) {
        snprintf(path, sizeof(path), "%s", name);
    } else {
        pthread_rwlock_rdlock(&g_dir_rwlock);
        const DirCacheEntry *parent_e = dir_cache_find_ino(parent);
        if (!parent_e) {
            pthread_rwlock_unlock(&g_dir_rwlock);
            fuse_reply_err(req, ENOENT);
            return;
        }
        snprintf(path, sizeof(path), "%s/%s", parent_e->path, name);
        pthread_rwlock_unlock(&g_dir_rwlock);
    }

    uint64_t inode = 0;
    int64_t  mtime_sec = 0;
    uint32_t mtime_nsec = 0;

    int errnum = send_fsop(FSOP_MKDIR, (uint32_t)mode, path, NULL,
                           &inode, &mtime_sec, &mtime_nsec);
    if (errnum != 0) {
        fuse_reply_err(req, errnum);
        return;
    }

    /* Add to local dir cache immediately so subsequent lookups find it */
    DirCacheEntry new_e;
    memset(&new_e, 0, sizeof(new_e));
    new_e.inode      = inode;
    new_e.type       = 1; /* directory */
    new_e.path       = path; /* dir_cache_add will strdup */
    new_e.mode       = (uint32_t)(S_IFDIR | (mode & 0777));
    new_e.uid        = fuse_req_ctx(req)->uid;
    new_e.gid        = fuse_req_ctx(req)->gid;
    new_e.size       = 0;
    new_e.mtime_sec  = mtime_sec;
    new_e.mtime_nsec = mtime_nsec;
    dir_cache_add(&new_e);

    struct fuse_entry_param ep;
    memset(&ep, 0, sizeof(ep));
    ep.ino           = (fuse_ino_t)inode;
    ep.attr_timeout  = 1.0;
    ep.entry_timeout = 1.0;
    fill_stat_from_entry(&new_e, &ep.attr);

    fuse_reply_entry(req, &ep);
}

static void fuse_ns_rmdir(fuse_req_t req, fuse_ino_t parent, const char *name)
{
    char path[PATH_MAX];
    if (parent == 1) {
        snprintf(path, sizeof(path), "%s", name);
    } else {
        pthread_rwlock_rdlock(&g_dir_rwlock);
        const DirCacheEntry *parent_e = dir_cache_find_ino(parent);
        if (!parent_e) {
            pthread_rwlock_unlock(&g_dir_rwlock);
            fuse_reply_err(req, ENOENT);
            return;
        }
        snprintf(path, sizeof(path), "%s/%s", parent_e->path, name);
        pthread_rwlock_unlock(&g_dir_rwlock);
    }

    int errnum = send_fsop(FSOP_RMDIR, 0, path, NULL, NULL, NULL, NULL);
    if (errnum == 0) {
        dir_cache_remove(path);
        fuse_reply_err(req, 0);
    } else {
        fuse_reply_err(req, errnum);
    }
}

static void fuse_ns_rename(fuse_req_t req, fuse_ino_t parent, const char *name,
                           fuse_ino_t newparent, const char *newname,
                           unsigned int flags)
{
    (void)flags;

    char old_path[PATH_MAX];
    char new_path[PATH_MAX];

    /* Build old path */
    if (parent == 1) {
        snprintf(old_path, sizeof(old_path), "%s", name);
    } else {
        pthread_rwlock_rdlock(&g_dir_rwlock);
        const DirCacheEntry *pe = dir_cache_find_ino(parent);
        if (!pe) {
            pthread_rwlock_unlock(&g_dir_rwlock);
            fuse_reply_err(req, ENOENT);
            return;
        }
        snprintf(old_path, sizeof(old_path), "%s/%s", pe->path, name);
        pthread_rwlock_unlock(&g_dir_rwlock);
    }

    /* Build new path */
    if (newparent == 1) {
        snprintf(new_path, sizeof(new_path), "%s", newname);
    } else {
        pthread_rwlock_rdlock(&g_dir_rwlock);
        const DirCacheEntry *npe = dir_cache_find_ino(newparent);
        if (!npe) {
            pthread_rwlock_unlock(&g_dir_rwlock);
            fuse_reply_err(req, ENOENT);
            return;
        }
        snprintf(new_path, sizeof(new_path), "%s/%s", npe->path, newname);
        pthread_rwlock_unlock(&g_dir_rwlock);
    }

    int errnum = send_fsop(FSOP_RENAME, 0, old_path, new_path, NULL, NULL, NULL);
    if (errnum == 0) {
        /* Update local dir cache: rename old_path entry to new_path */
        pthread_rwlock_wrlock(&g_dir_rwlock);
        for (size_t i = 0; i < g_dir_cache.count; i++) {
            if (strcmp(g_dir_cache.entries[i].path, old_path) == 0) {
                free(g_dir_cache.entries[i].path);
                g_dir_cache.entries[i].path = strdup(new_path);
                break;
            }
        }
        pthread_rwlock_unlock(&g_dir_rwlock);
        fuse_reply_err(req, 0);
    } else {
        fuse_reply_err(req, errnum);
    }
}

static void fuse_ns_fsync(fuse_req_t req, fuse_ino_t ino, int datasync,
                          struct fuse_file_info *fi)
{
    (void)ino;
    int fd = (int)(uintptr_t)fi->fh;
    if (fd >= 0) {
        int r = datasync ? fdatasync(fd) : fsync(fd);
        if (r < 0) {
            fuse_reply_err(req, errno);
            return;
        }
    }
    fuse_reply_err(req, 0);
}

static void fuse_ns_opendir(fuse_req_t req, fuse_ino_t ino,
                            struct fuse_file_info *fi)
{
    DirSnapshot *snap = snapshot_create(ino);
    if (!snap) {
        fuse_reply_err(req, ENOMEM);
        return;
    }
    fi->fh = (uint64_t)(uintptr_t)snap;
    fuse_reply_open(req, fi);
}

static void fuse_ns_readdir(fuse_req_t req, fuse_ino_t ino, size_t size,
                            off_t off, struct fuse_file_info *fi)
{
    (void)ino;

    DirSnapshot *snap = (DirSnapshot *)(uintptr_t)fi->fh;
    if (!snap) {
        fuse_reply_err(req, EBADF);
        return;
    }

    char *buf = calloc(1, size);
    if (!buf) {
        fuse_reply_err(req, ENOMEM);
        return;
    }

    size_t buf_off = 0;
    off_t entry_idx = off;

    /* Emit "." and ".." first */
    if (entry_idx == 0) {
        struct stat st;
        fill_root_stat(&st);
        size_t needed = fuse_add_direntry(req, buf + buf_off, size - buf_off,
                                          ".", &st, 1);
        if (needed > size - buf_off)
            goto done;
        buf_off += needed;
        entry_idx = 1;
    }
    if (entry_idx == 1) {
        struct stat st;
        fill_root_stat(&st);
        size_t needed = fuse_add_direntry(req, buf + buf_off, size - buf_off,
                                          "..", &st, 2);
        if (needed > size - buf_off)
            goto done;
        buf_off += needed;
        entry_idx = 2;
    }

    /* Emit entries from the snapshot that are direct children of dir_ino */
    for (size_t i = 0; i < snap->count; i++) {
        DirCacheEntry *e = &snap->entries[i];
        if ((off_t)(i + 2) < entry_idx)
            continue;

        /* Only emit entries whose parent is the opened directory */
        /* For root dir (inode 1), emit top-level entries (no '/' in path) */
        int is_child = 0;
        if (snap->dir_ino == 1) {
            is_child = (strchr(e->path, '/') == NULL);
        } else {
            /* Find the parent entry and check if path starts with parent path + '/' */
            pthread_rwlock_rdlock(&g_dir_rwlock);
            const DirCacheEntry *parent_e = dir_cache_find_ino(snap->dir_ino);
            if (parent_e) {
                size_t plen = strlen(parent_e->path);
                is_child = (strncmp(e->path, parent_e->path, plen) == 0
                            && e->path[plen] == '/'
                            && strchr(e->path + plen + 1, '/') == NULL);
            }
            pthread_rwlock_unlock(&g_dir_rwlock);
        }

        if (!is_child)
            continue;

        /* Use the basename of the path as the dirent name */
        const char *dirent_name = strrchr(e->path, '/');
        dirent_name = dirent_name ? dirent_name + 1 : e->path;

        struct stat st;
        fill_stat_from_entry(e, &st);
        off_t next_off = (off_t)(i + 3);
        size_t needed = fuse_add_direntry(req, buf + buf_off, size - buf_off,
                                          dirent_name, &st, next_off);
        if (needed > size - buf_off)
            break;
        buf_off += needed;
    }

done:
    fuse_reply_buf(req, buf, buf_off);
    free(buf);
}

static void fuse_ns_releasedir(fuse_req_t req, fuse_ino_t ino,
                               struct fuse_file_info *fi)
{
    (void)ino;
    DirSnapshot *snap = (DirSnapshot *)(uintptr_t)fi->fh;
    snapshot_free(snap);
    fuse_reply_err(req, 0);
}

/* -----------------------------------------------------------------------
 * Operation table
 * --------------------------------------------------------------------- */

static const struct fuse_lowlevel_ops fuse_ns_ops = {
    .init       = fuse_ns_init,
    .destroy    = fuse_ns_destroy,
    .lookup     = fuse_ns_lookup,
    .getattr    = fuse_ns_getattr,
    .setattr    = fuse_ns_setattr,
    .open       = fuse_ns_open,
    .create     = fuse_ns_create,
    .read       = fuse_ns_read,
    .write      = fuse_ns_write,
    .release    = fuse_ns_release,
    .unlink     = fuse_ns_unlink,
    .mkdir      = fuse_ns_mkdir,
    .rmdir      = fuse_ns_rmdir,
    .rename     = fuse_ns_rename,
    .fsync      = fuse_ns_fsync,
    .opendir    = fuse_ns_opendir,
    .readdir    = fuse_ns_readdir,
    .releasedir = fuse_ns_releasedir,
};

/* -----------------------------------------------------------------------
 * Unix socket connection
 * --------------------------------------------------------------------- */

static int connect_to_tierd(const char *socket_path)
{
    int fd = socket(AF_UNIX, SOCK_STREAM, 0);
    if (fd < 0) {
        fprintf(stderr, "tierd-fuse-ns: socket(): %s\n", strerror(errno));
        return -1;
    }

    struct sockaddr_un addr;
    memset(&addr, 0, sizeof(addr));
    addr.sun_family = AF_UNIX;
    strncpy(addr.sun_path, socket_path, sizeof(addr.sun_path) - 1);

    if (connect(fd, (struct sockaddr *)&addr, sizeof(addr)) < 0) {
        fprintf(stderr, "tierd-fuse-ns: connect(%s): %s\n",
                socket_path, strerror(errno));
        close(fd);
        return -1;
    }

    return fd;
}

/* -----------------------------------------------------------------------
 * Write PID file
 * --------------------------------------------------------------------- */

static void write_pid_file(const char *run_dir, const char *namespace_id)
{
    char pid_path[PATH_MAX];
    snprintf(pid_path, sizeof(pid_path), "%s/fuse-%s.pid", run_dir, namespace_id);

    FILE *f = fopen(pid_path, "w");
    if (!f) {
        fprintf(stderr, "tierd-fuse-ns: failed to write PID file %s: %s\n",
                pid_path, strerror(errno));
        return;
    }
    fprintf(f, "%d\n", (int)getpid());
    fclose(f);
}

/* -----------------------------------------------------------------------
 * main
 * --------------------------------------------------------------------- */

int main(int argc, char *argv[])
{
    if (argc != 3) {
        fprintf(stderr, "Usage: tierd-fuse-ns <socket_path> <mount_path>\n");
        return 1;
    }

    const char *socket_path = argv[1];
    const char *mount_path  = argv[2];

    /* Initialize per-slot condition variables for concurrent open requests */
    for (int i = 0; i < MAX_INFLIGHT_OPENS; i++)
        pthread_cond_init(&g_open_slots[i].cond, NULL);

    /* Initialize per-slot condition variables for concurrent FS ops */
    for (int i = 0; i < MAX_INFLIGHT_FSOPS; i++)
        pthread_cond_init(&g_fsop_slots[i].cond, NULL);

    /* Stash mount path */
    strncpy(g_mount_path, mount_path, PATH_MAX - 1);
    g_mount_path[PATH_MAX - 1] = '\0';

    /* Connect to tierd */
    g_sock_fd = connect_to_tierd(socket_path);
    if (g_sock_fd < 0)
        return 1;

    /* Derive run_dir from socket_path (directory containing the socket file) */
    char socket_dir[PATH_MAX];
    strncpy(socket_dir, socket_path, PATH_MAX - 1);
    socket_dir[PATH_MAX - 1] = '\0';
    /* Trim filename from socket_dir */
    char *last_slash = strrchr(socket_dir, '/');
    if (last_slash)
        *last_slash = '\0';
    else
        strncpy(socket_dir, ".", PATH_MAX - 1);

    /* Derive namespace_id from socket_path filename: "fuse-<ns>.sock" → <ns> */
    char ns_id[PATH_MAX];
    const char *sock_base = strrchr(socket_path, '/');
    sock_base = sock_base ? sock_base + 1 : socket_path;
    strncpy(ns_id, sock_base, PATH_MAX - 1);
    ns_id[PATH_MAX - 1] = '\0';
    /* Strip "fuse-" prefix and ".sock" suffix */
    if (strncmp(ns_id, "fuse-", 5) == 0)
        memmove(ns_id, ns_id + 5, strlen(ns_id) - 4); /* -4 = -5 prefix + 1 null shift */
    char *dot = strrchr(ns_id, '.');
    if (dot && strcmp(dot, ".sock") == 0)
        *dot = '\0';

    /* Write PID file */
    write_pid_file(socket_dir, ns_id);

    /* Start reader thread */
    pthread_t reader_tid;
    if (pthread_create(&reader_tid, NULL, reader_thread_main, NULL) != 0) {
        fprintf(stderr, "tierd-fuse-ns: pthread_create reader: %s\n", strerror(errno));
        close(g_sock_fd);
        return 1;
    }
    pthread_detach(reader_tid);

    /* Build FUSE args (program name only — no -f; fuse_session_new does not
     * accept foreground flags and the binary is already managed in-process
     * by tierd so daemonisation is not needed). */
    struct fuse_args args = FUSE_ARGS_INIT(0, NULL);
    if (fuse_opt_add_arg(&args, argv[0]) != 0 ||
        fuse_opt_add_arg(&args, "-o") != 0 ||
        fuse_opt_add_arg(&args, "allow_other") != 0) {
        fprintf(stderr, "tierd-fuse-ns: fuse_opt_add_arg failed\n");
        return 1;
    }

    /* Create FUSE session */
    struct fuse_session *se = fuse_session_new(&args, &fuse_ns_ops,
                                               sizeof(fuse_ns_ops), NULL);
    if (!se) {
        fprintf(stderr, "tierd-fuse-ns: fuse_session_new failed\n");
        fuse_opt_free_args(&args);
        return 1;
    }

    /* Set up signal handlers */
    if (fuse_set_signal_handlers(se) != 0) {
        fprintf(stderr, "tierd-fuse-ns: fuse_set_signal_handlers failed\n");
        fuse_session_destroy(se);
        fuse_opt_free_args(&args);
        return 1;
    }

    /* Mount */
    if (fuse_session_mount(se, mount_path) != 0) {
        fprintf(stderr, "tierd-fuse-ns: fuse_session_mount(%s) failed\n",
                mount_path);
        fuse_remove_signal_handlers(se);
        fuse_session_destroy(se);
        fuse_opt_free_args(&args);
        return 1;
    }

    /* Multi-threaded event loop — allows concurrent FUSE ops.  With
     * passthrough active most I/O bypasses userspace entirely, but
     * concurrent open/create/lookup still benefit from threading. */
    struct fuse_loop_config *loop_cfg = fuse_loop_cfg_create();
    fuse_loop_cfg_set_clone_fd(loop_cfg, 1);
    int ret = fuse_session_loop_mt(se, loop_cfg);
    fuse_loop_cfg_destroy(loop_cfg);

    /* Cleanup */
    fuse_session_unmount(se);
    fuse_remove_signal_handlers(se);
    fuse_session_destroy(se);
    fuse_opt_free_args(&args);
    close(g_sock_fd);

    return ret < 0 ? 1 : 0;
}
