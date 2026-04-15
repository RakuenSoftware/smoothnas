package zfsmgd

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

// ErrFanotifyUnavailable is returned by StartFanotifyWatch when fanotify is not
// available on this kernel (ENOSYS) or the caller lacks permission (EPERM).
var ErrFanotifyUnavailable = errors.New("fanotify unavailable")

// fanotify syscall numbers for amd64.
const (
	sysFanotifyInit = 300
	sysFanotifyMark = 301
)

// fanotify_init flags.
const (
	fanClassNotif  = 0x00000000
	fanCloexec     = 0x00000001
	fanNonblock    = 0x00000002
)

// fanotify_mark flags and event masks.
const (
	fanMarkAdd    = 0x00000001
	fanMarkMount  = 0x00000010
	fanOpen       = 0x00000020
	fanCreate     = 0x00000100
)

// fanotifyEventMetadata is the kernel struct for fanotify events.
// See struct fanotify_event_metadata in <linux/fanotify.h>.
type fanotifyEventMetadata struct {
	EventLen    uint32
	Vers        uint8
	Reserved    uint8
	MetadataLen uint16
	Mask        uint64
	Fd          int32
	Pid         int32
}

const fanotifyMetadataSize = 24 // sizeof(struct fanotify_event_metadata)

// FanotifyWatcher watches a backing dataset mountpoint for bypass accesses.
// It installs a fanotify watch and reports any FAN_OPEN or FAN_CREATE event
// from a PID other than the expected daemon PID.
type FanotifyWatcher struct {
	fd       int
	stopCh   chan struct{}
	doneCh   chan struct{}
}

// StartFanotifyWatch installs a fanotify watch on mountPath.
// daemonPID is the PID of the C FUSE daemon (only events from other PIDs are reported).
// onBypass is called when bypass is detected.
// Returns (watcher, nil) on success, or (nil, ErrFanotifyUnavailable) if
// fanotify is not available on this kernel.
func StartFanotifyWatch(mountPath string, daemonPID int, namespaceID string, onBypass func()) (*FanotifyWatcher, error) {
	// fanotify_init(FAN_CLASS_NOTIF | FAN_CLOEXEC | FAN_NONBLOCK, O_RDONLY)
	initFlags := uintptr(fanClassNotif | fanCloexec | fanNonblock)
	r, _, errno := syscall.Syscall(sysFanotifyInit, initFlags, uintptr(os.O_RDONLY), 0)
	if errno != 0 {
		if errno == syscall.ENOSYS || errno == syscall.EPERM {
			return nil, ErrFanotifyUnavailable
		}
		return nil, fmt.Errorf("fanotify_init: %w", errno)
	}
	faFd := int(r)

	// fanotify_mark(faFd, FAN_MARK_ADD | FAN_MARK_MOUNT, FAN_OPEN | FAN_CREATE, AT_FDCWD, mountPath)
	atFdcwd := uintptr(^uint(0) - 99) // AT_FDCWD = -100 as uintptr
	pathPtr, err := syscall.BytePtrFromString(mountPath)
	if err != nil {
		_ = syscall.Close(faFd)
		return nil, fmt.Errorf("invalid mount path: %w", err)
	}
	markFlags := uintptr(fanMarkAdd | fanMarkMount)
	mask := uintptr(fanOpen | fanCreate)
	_, _, errno = syscall.Syscall6(sysFanotifyMark,
		uintptr(faFd), markFlags, mask,
		atFdcwd, uintptr(unsafe.Pointer(pathPtr)), 0)
	if errno != 0 {
		_ = syscall.Close(faFd)
		if errno == syscall.ENOSYS || errno == syscall.EPERM {
			return nil, ErrFanotifyUnavailable
		}
		return nil, fmt.Errorf("fanotify_mark: %w", errno)
	}

	w := &FanotifyWatcher{
		fd:     faFd,
		stopCh: make(chan struct{}),
		doneCh: make(chan struct{}),
	}

	go w.readLoop(daemonPID, onBypass)
	return w, nil
}

// readLoop reads fanotify events and calls onBypass for events from non-daemon PIDs.
// The fanotify fd is opened with FAN_NONBLOCK; we poll it using the ppoll(2) syscall.
func (w *FanotifyWatcher) readLoop(daemonPID int, onBypass func()) {
	defer close(w.doneCh)

	buf := make([]byte, 4096)
	for {
		select {
		case <-w.stopCh:
			return
		default:
		}

		// Wait up to 200ms for data to be available using ppoll.
		// ppoll: SYS_PPOLL = 271 on amd64
		// struct pollfd { int fd; short events; short revents; }
		type pollFd struct {
			fd      int32
			events  int16
			revents int16
		}
		pfd := pollFd{fd: int32(w.fd), events: 0x0001 /* POLLIN */}
		ts := syscall.Timespec{Nsec: 200_000_000} // 200ms
		_, _, errno := syscall.Syscall6(
			271, // SYS_PPOLL (amd64)
			uintptr(unsafe.Pointer(&pfd)), 1,
			uintptr(unsafe.Pointer(&ts)), 0, 0, 0,
		)
		if errno != 0 && errno != syscall.EINTR {
			return
		}
		if pfd.revents == 0 {
			continue
		}

		n, err := syscall.Read(w.fd, buf)
		if err != nil {
			if err == syscall.EINTR || err == syscall.EAGAIN || err == syscall.EWOULDBLOCK {
				continue
			}
			// fd closed or unrecoverable error.
			return
		}

		for off := 0; off+fanotifyMetadataSize <= n; {
			meta := (*fanotifyEventMetadata)(unsafe.Pointer(&buf[off]))
			if meta.EventLen < fanotifyMetadataSize {
				break
			}
			if meta.Fd >= 0 {
				// Close the event fd; we only care about the pid.
				_ = syscall.Close(int(meta.Fd))
			}
			if int(meta.Pid) != daemonPID {
				onBypass()
			}
			off += int(meta.EventLen)
		}
	}
}

// Stop closes the fanotify fd and stops the goroutine.
func (w *FanotifyWatcher) Stop() {
	select {
	case <-w.stopCh:
	default:
		close(w.stopCh)
	}
	_ = syscall.Close(w.fd)
	<-w.doneCh
}
