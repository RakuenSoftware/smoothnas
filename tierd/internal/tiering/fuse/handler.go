// Package fuse provides shared infrastructure for FUSE-based tiering
// adapters. It contains the SocketServer (Unix-domain socket protocol for
// communicating with the tierd-fuse-ns daemon), the DaemonSupervisor
// (process lifecycle management), and the OpenHandler interface that each
// backend adapter implements to route file opens to the correct tier.
//
// The tierd-fuse-ns daemon is filesystem-agnostic — it only deals in fds
// passed via SCM_RIGHTS. Any adapter (mdadm/LVM, ZFS, btrfs, etc.) can
// use this infrastructure by implementing OpenHandler.
package fuse

// OpenHandler is the callback interface used by SocketServer to dispatch
// FUSE daemon events to the backend adapter. Each tiering adapter (mdadm,
// zfs-managed, etc.) implements this interface.
type OpenHandler interface {
	// HandleOpen is called when the FUSE daemon receives an open() or
	// create() syscall. The adapter resolves the object to its current
	// tier target, opens the file on the backing mount, and returns the
	// fd. For O_CREAT with an unknown key, the adapter should auto-register
	// the object on the fastest tier.
	HandleOpen(namespaceID, objectKey string, flags uint32) (backingFD int, backingInode uint64, err error)

	// HandleRelease is called when the FUSE daemon closes a backing fd.
	HandleRelease(namespaceID string, inode uint64)

	// HandleBypass is called when the daemon detects a bypass condition
	// (e.g. fanotify event on the backing mount from a non-FUSE process).
	HandleBypass(namespaceID string)

	// HandleFDPassFailed is called when the SCM_RIGHTS fd pass to the
	// daemon fails after the adapter has already opened the backing file.
	HandleFDPassFailed(namespaceID string, expectedInode uint64)

	// OnHealthFail is called when the daemon fails consecutive health
	// checks. The adapter should restart the daemon.
	OnHealthFail(namespaceID string)
}

// ConnectHandler is an optional extension to OpenHandler. If the handler
// implements this interface, HandleConnect is called when the FUSE daemon
// establishes a new connection. The adapter should send an initial DIR_UPDATE
// to populate the daemon's directory cache.
type ConnectHandler interface {
	HandleConnect(namespaceID string)
}

// FSOpHandler is an optional extension to OpenHandler for filesystem mutation
// operations (mkdir, unlink, rmdir, rename). The FUSE daemon sends MSG_FS_OP
// for these operations; if the handler does not implement FSOpHandler the
// server replies with EPERM.
type FSOpHandler interface {
	// HandleMkdir creates a directory at path on the backing filesystem(s).
	// Returns the inode and mtime of the new directory on success.
	HandleMkdir(namespaceID, path string, mode uint32) (inode uint64, mtimeSec int64, mtimeNsec uint32, err error)

	// HandleUnlink deletes a file at path on the backing filesystem(s).
	HandleUnlink(namespaceID, path string) error

	// HandleRmdir removes an empty directory at path on the backing filesystem(s).
	HandleRmdir(namespaceID, path string) error

	// HandleRename renames oldPath to newPath on the backing filesystem(s).
	HandleRename(namespaceID, oldPath, newPath string) error
}
