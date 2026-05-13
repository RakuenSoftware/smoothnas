//go:build linux

package nonraid

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"sync"

	"golang.org/x/sys/unix"
)

const (
	nbdSetSock       = 0xab00
	nbdSetBlockSize  = 0xab01
	nbdSetSize       = 0xab02
	nbdDoIt          = 0xab03
	nbdClearSock     = 0xab04
	nbdClearQueue    = 0xab05
	nbdDisconnect    = 0xab08
	nbdSetTimeout    = 0xab09
	nbdSetFlags      = 0xab0a
	nbdRequestMagic  = 0x25609513
	nbdReplyMagic    = 0x67446698
	nbdCmdRead       = 0
	nbdCmdWrite      = 1
	nbdCmdDisconnect = 2
	nbdCmdFlush      = 3
	nbdCmdTrim       = 4
	nbdCmdMask       = 0x0000ffff
)

// NBDServer exposes one nonRaid data slot as a Linux nbd device.
type NBDServer struct {
	Path   string
	Slot   int
	engine *Engine

	nbdFile *os.File
	conn    *os.File
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

// StartNBD exposes data slot as nbdPath until ctx is cancelled or Stop is
// called. Slots are zero-based; nbd paths are ordinary /dev/nbdN devices.
func StartNBD(ctx context.Context, nbdPath string, engine *Engine, slot int) (*NBDServer, error) {
	size, err := engine.DataSize(slot)
	if err != nil {
		return nil, err
	}
	nbd, err := os.OpenFile(nbdPath, os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", nbdPath, err)
	}
	socks, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		_ = nbd.Close()
		return nil, fmt.Errorf("socketpair: %w", err)
	}
	cleanupSock := true
	defer func() {
		if cleanupSock {
			_ = unix.Close(socks[0])
			_ = unix.Close(socks[1])
		}
	}()

	if err := ioctlSetInt(nbd, nbdSetBlockSize, 4096); err != nil {
		_ = nbd.Close()
		return nil, fmt.Errorf("%s NBD_SET_BLKSIZE: %w", nbdPath, err)
	}
	if err := ioctlSetUintptr(nbd, nbdSetSize, uintptr(size)); err != nil {
		_ = nbd.Close()
		return nil, fmt.Errorf("%s NBD_SET_SIZE: %w", nbdPath, err)
	}
	if err := ioctlSetInt(nbd, nbdSetTimeout, 30); err != nil {
		_ = nbd.Close()
		return nil, fmt.Errorf("%s NBD_SET_TIMEOUT: %w", nbdPath, err)
	}
	if err := ioctlSetInt(nbd, nbdSetFlags, 0); err != nil {
		_ = nbd.Close()
		return nil, fmt.Errorf("%s NBD_SET_FLAGS: %w", nbdPath, err)
	}
	if err := ioctlSetInt(nbd, nbdSetSock, socks[1]); err != nil {
		_ = nbd.Close()
		return nil, fmt.Errorf("%s NBD_SET_SOCK: %w", nbdPath, err)
	}

	serverCtx, cancel := context.WithCancel(ctx)
	srv := &NBDServer{
		Path:    nbdPath,
		Slot:    slot,
		engine:  engine,
		nbdFile: nbd,
		conn:    os.NewFile(uintptr(socks[0]), nbdPath+".sock"),
		cancel:  cancel,
	}
	cleanupSock = false
	_ = unix.Close(socks[1])

	srv.wg.Add(2)
	go srv.runKernel()
	go srv.serve(serverCtx)
	return srv, nil
}

func ioctlSetInt(f *os.File, req uintptr, value int) error {
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, f.Fd(), req, uintptr(value))
	if errno != 0 {
		return errno
	}
	return nil
}

func ioctlSetUintptr(f *os.File, req uintptr, value uintptr) error {
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, f.Fd(), req, value)
	if errno != 0 {
		return errno
	}
	return nil
}

func (s *NBDServer) runKernel() {
	defer s.wg.Done()
	_, _, _ = unix.Syscall(unix.SYS_IOCTL, s.nbdFile.Fd(), nbdDoIt, 0)
	_ = ioctlSetInt(s.nbdFile, nbdClearQueue, 0)
	_ = ioctlSetInt(s.nbdFile, nbdClearSock, 0)
}

func (s *NBDServer) serve(ctx context.Context) {
	defer s.wg.Done()
	defer s.cancel()
	go func() {
		<-ctx.Done()
		_ = s.conn.Close()
	}()
	for {
		if err := s.handleOne(); err != nil {
			return
		}
	}
}

func (s *NBDServer) handleOne() error {
	var req [28]byte
	if _, err := io.ReadFull(s.conn, req[:]); err != nil {
		return err
	}
	if binary.BigEndian.Uint32(req[0:4]) != nbdRequestMagic {
		_ = s.reply(req[8:16], unix.EINVAL, nil)
		return fmt.Errorf("invalid nbd request magic")
	}
	cmd := binary.BigEndian.Uint32(req[4:8]) & nbdCmdMask
	handle := req[8:16]
	offset := int64(binary.BigEndian.Uint64(req[16:24]))
	length := int(binary.BigEndian.Uint32(req[24:28]))

	switch cmd {
	case nbdCmdRead:
		buf := make([]byte, length)
		errno := unix.Errno(0)
		if err := s.engine.ReadData(s.Slot, offset, buf); err != nil {
			errno = unix.EIO
			buf = nil
		}
		return s.reply(handle, errno, buf)
	case nbdCmdWrite:
		payload := make([]byte, length)
		if _, err := io.ReadFull(s.conn, payload); err != nil {
			return err
		}
		errno := unix.Errno(0)
		if err := s.engine.WriteData(s.Slot, offset, payload); err != nil {
			errno = unix.EIO
		}
		return s.reply(handle, errno, nil)
	case nbdCmdFlush:
		errno := unix.Errno(0)
		if err := s.engine.Sync(); err != nil {
			errno = unix.EIO
		}
		return s.reply(handle, errno, nil)
	case nbdCmdTrim:
		errno := unix.Errno(0)
		if err := s.engine.ZeroData(s.Slot, offset, length); err != nil {
			errno = unix.EIO
		}
		return s.reply(handle, errno, nil)
	case nbdCmdDisconnect:
		return io.EOF
	default:
		return s.reply(handle, unix.EINVAL, nil)
	}
}

func (s *NBDServer) reply(handle []byte, errno unix.Errno, payload []byte) error {
	var rsp [16]byte
	binary.BigEndian.PutUint32(rsp[0:4], nbdReplyMagic)
	binary.BigEndian.PutUint32(rsp[4:8], uint32(errno))
	copy(rsp[8:16], handle)
	if _, err := s.conn.Write(rsp[:]); err != nil {
		return err
	}
	if len(payload) > 0 {
		_, err := s.conn.Write(payload)
		return err
	}
	return nil
}

func (s *NBDServer) Stop() error {
	s.cancel()
	_ = ioctlSetInt(s.nbdFile, nbdDisconnect, 0)
	_ = s.conn.Close()
	s.wg.Wait()
	err := s.nbdFile.Close()
	return err
}
