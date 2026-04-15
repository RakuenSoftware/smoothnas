package fuse

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// Binary framing protocol message types.
const (
	MsgOpenRequest   uint32 = 1
	MsgOpenResponse  uint32 = 2
	MsgReleaseNotify uint32 = 3
	MsgHealthPing    uint32 = 4
	MsgHealthPong    uint32 = 5
	MsgError         uint32 = 6
	MsgDirUpdate     uint32 = 7
	MsgQuiesce       uint32 = 8
	MsgQuiesceAck    uint32 = 9
	MsgRelease       uint32 = 10
	MsgFsOp          uint32 = 11 // daemon→tierd: filesystem operation request
	MsgFsOpResponse  uint32 = 12 // tierd→daemon: filesystem operation response
)

// FS op types embedded in MsgFsOp payload.
const (
	FsOpMkdir  = 1
	FsOpUnlink = 2
	FsOpRmdir  = 3
	FsOpRename = 4
)

const maxPayload = 65536

const healthPingInterval = 10 * time.Second
const healthPongTimeout = 5 * time.Second
const healthMissLimit = 3

// DirEntry represents one entry in a directory tree snapshot sent to the daemon.
type DirEntry struct {
	Inode     uint64
	Type      uint8 // 0=file, 1=dir
	Path      string
	Mode      uint32
	UID       uint32
	GID       uint32
	Size      uint64
	MtimeSec  int64
	MtimeNsec uint32
}

// NsListener holds the per-namespace socket state.
type NsListener struct {
	listener   net.Listener
	socketPath string
	stopCh     chan struct{}
}

// NsConn holds the active connection state for one namespace.
type NsConn struct {
	conn         *net.UnixConn
	sendMu       sync.Mutex
	stopCh       chan struct{}
	pongCh       chan struct{}
	quiesceAckCh chan struct{}
}

// SocketServer serves a Unix-domain socket per namespace. The FUSE daemon
// connects to tierd (not the other way around) on the per-namespace socket.
// This is backend-agnostic — any adapter implementing OpenHandler can use it.
type SocketServer struct {
	mu        sync.Mutex
	socketDir string
	handler   OpenHandler
	logPrefix string
	listeners map[string]*NsListener
	conns     map[string]*NsConn
}

// NewSocketServer creates a SocketServer that will place per-namespace socket
// files under socketDir and dispatch events to handler.
func NewSocketServer(socketDir string, handler OpenHandler) *SocketServer {
	return &SocketServer{
		socketDir: socketDir,
		handler:   handler,
		logPrefix: "fuse",
		listeners: make(map[string]*NsListener),
		conns:     make(map[string]*NsConn),
	}
}

// SetLogPrefix overrides the default log prefix for this server.
func (s *SocketServer) SetLogPrefix(prefix string) {
	s.logPrefix = prefix
}

// Start creates a Unix-domain socket for namespaceID and begins accepting
// connections. Returns the socket path.
func (s *SocketServer) Start(namespaceID string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.listeners[namespaceID]; ok {
		return "", fmt.Errorf("socket server already started for namespace %q", namespaceID)
	}

	socketPath := SocketPathFor(s.socketDir, namespaceID)
	// The socket directory must already exist (created by systemd RuntimeDirectory=).
	// Return a clear error rather than silently creating it, so a missing directory
	// surfaces as a misconfiguration rather than a permissions surprise.
	if _, err := os.Stat(filepath.Dir(socketPath)); err != nil {
		return "", fmt.Errorf("socket dir %q does not exist (is RuntimeDirectory= set in the service unit?): %w",
			filepath.Dir(socketPath), err)
	}
	_ = os.Remove(socketPath)

	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		return "", fmt.Errorf("listen on socket %q: %w", socketPath, err)
	}

	nl := &NsListener{
		listener:   ln,
		socketPath: socketPath,
		stopCh:     make(chan struct{}),
	}
	s.listeners[namespaceID] = nl

	go s.serve(namespaceID, nl)
	return socketPath, nil
}

// Stop closes the socket for namespaceID.
func (s *SocketServer) Stop(namespaceID string) {
	s.mu.Lock()
	nl, ok := s.listeners[namespaceID]
	if ok {
		delete(s.listeners, namespaceID)
	}
	nc := s.conns[namespaceID]
	if nc != nil {
		delete(s.conns, namespaceID)
	}
	s.mu.Unlock()

	if ok {
		close(nl.stopCh)
		nl.listener.Close()
		_ = os.Remove(nl.socketPath)
	}
	if nc != nil {
		select {
		case <-nc.stopCh:
		default:
			close(nc.stopCh)
		}
		nc.conn.Close()
	}
}

// StopAll closes all open sockets.
func (s *SocketServer) StopAll() {
	s.mu.Lock()
	ids := make([]string, 0, len(s.listeners))
	for id := range s.listeners {
		ids = append(ids, id)
	}
	s.mu.Unlock()
	for _, id := range ids {
		s.Stop(id)
	}
}

// SendQuiesce sends a QUIESCE message and waits for acknowledgement.
func (s *SocketServer) SendQuiesce(namespaceID string, timeout time.Duration) error {
	s.mu.Lock()
	nc := s.conns[namespaceID]
	s.mu.Unlock()
	if nc == nil {
		return nil
	}
	if err := SendMsg(nc, MsgQuiesce, nil); err != nil {
		return fmt.Errorf("send QUIESCE: %w", err)
	}
	select {
	case <-nc.quiesceAckCh:
		return nil
	case <-nc.stopCh:
		return nil
	case <-time.After(timeout):
		return fmt.Errorf("quiesce ack timeout after %v", timeout)
	}
}

// SendRelease sends a RELEASE message to the daemon, resuming O_CREAT.
func (s *SocketServer) SendRelease(namespaceID string) {
	s.mu.Lock()
	nc := s.conns[namespaceID]
	s.mu.Unlock()
	if nc == nil {
		return
	}
	_ = SendMsg(nc, MsgRelease, nil)
}

// SendDirUpdate encodes entries in binary DIR_UPDATE format and sends them.
func (s *SocketServer) SendDirUpdate(namespaceID string, entries []DirEntry) error {
	s.mu.Lock()
	nc := s.conns[namespaceID]
	s.mu.Unlock()
	if nc == nil {
		return fmt.Errorf("no active connection for namespace %q", namespaceID)
	}
	payload := EncodeDirUpdate(entries)
	return SendMsg(nc, MsgDirUpdate, payload)
}

// EncodeDirUpdate encodes a slice of DirEntry into the binary DIR_UPDATE format.
func EncodeDirUpdate(entries []DirEntry) []byte {
	var buf []byte
	le := binary.LittleEndian

	for _, e := range entries {
		pathBytes := []byte(e.Path)
		pathLen := uint16(len(pathBytes))
		entrySize := 8 + 1 + 2 + int(pathLen) + 1 + 4 + 4 + 4 + 8 + 8 + 4
		entry := make([]byte, entrySize)
		off := 0

		le.PutUint64(entry[off:], e.Inode)
		off += 8
		entry[off] = e.Type
		off++
		le.PutUint16(entry[off:], pathLen)
		off += 2
		copy(entry[off:], pathBytes)
		off += int(pathLen)
		entry[off] = 0
		off++
		le.PutUint32(entry[off:], e.Mode)
		off += 4
		le.PutUint32(entry[off:], e.UID)
		off += 4
		le.PutUint32(entry[off:], e.GID)
		off += 4
		le.PutUint64(entry[off:], e.Size)
		off += 8
		le.PutUint64(entry[off:], uint64(e.MtimeSec))
		off += 8
		le.PutUint32(entry[off:], e.MtimeNsec)

		buf = append(buf, entry...)
	}
	return buf
}

// SocketPathFor returns the socket path for a given namespace under socketDir.
func SocketPathFor(socketDir, namespaceID string) string {
	safe := namespaceID
	if len(safe) > 32 {
		safe = safe[:32]
	}
	return socketDir + "/fuse-" + safe + ".sock"
}

// serve accepts connections.
func (s *SocketServer) serve(namespaceID string, nl *NsListener) {
	for {
		conn, err := nl.listener.Accept()
		if err != nil {
			select {
			case <-nl.stopCh:
				return
			default:
				log.Printf("%s socket [%s]: accept error: %v", s.logPrefix, namespaceID, err)
				return
			}
		}
		go s.handleConn(namespaceID, conn)
	}
}

func (s *SocketServer) handleConn(namespaceID string, conn net.Conn) {
	defer conn.Close()

	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		log.Printf("%s socket [%s]: connection is not a UnixConn", s.logPrefix, namespaceID)
		return
	}

	nc := &NsConn{
		conn:         unixConn,
		stopCh:       make(chan struct{}),
		pongCh:       make(chan struct{}, 1),
		quiesceAckCh: make(chan struct{}, 1),
	}

	s.mu.Lock()
	s.conns[namespaceID] = nc
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		if s.conns[namespaceID] == nc {
			delete(s.conns, namespaceID)
		}
		s.mu.Unlock()
	}()

	// Notify the adapter that a daemon has connected so it can send the
	// initial DIR_UPDATE to populate the daemon's directory cache.
	if ch, ok := s.handler.(ConnectHandler); ok {
		go ch.HandleConnect(namespaceID)
	}

	go s.runHealthPing(namespaceID, nc)

	for {
		msgType, payload, err := RecvMsg(unixConn)
		if err != nil {
			select {
			case <-nc.stopCh:
			default:
				log.Printf("%s socket [%s]: recv error: %v", s.logPrefix, namespaceID, err)
			}
			return
		}

		switch msgType {
		case MsgOpenRequest:
			go s.handleOpenRequest(namespaceID, nc, payload)
		case MsgFsOp:
			go s.handleFsOp(namespaceID, nc, payload)
		case MsgReleaseNotify:
			if len(payload) < 8 {
				continue
			}
			inode := binary.LittleEndian.Uint64(payload[:8])
			s.handler.HandleRelease(namespaceID, inode)
		case MsgHealthPong:
			select {
			case nc.pongCh <- struct{}{}:
			default:
			}
		case MsgQuiesceAck:
			select {
			case nc.quiesceAckCh <- struct{}{}:
			default:
			}
		case MsgError:
			log.Printf("%s socket [%s]: ERROR from daemon: %s", s.logPrefix, namespaceID, string(payload))
		default:
			_ = SendMsg(nc, MsgError, []byte("unknown message type"))
		}
	}
}

func (s *SocketServer) handleOpenRequest(namespaceID string, nc *NsConn, payload []byte) {
	if len(payload) < 9 {
		return
	}

	reqID := binary.LittleEndian.Uint32(payload[0:4])
	flags := binary.LittleEndian.Uint32(payload[4:8])

	key := string(payload[8:])
	if len(key) > 0 && key[len(key)-1] == 0 {
		key = key[:len(key)-1]
	}

	fd, inode, err := s.handler.HandleOpen(namespaceID, key, flags)
	if err != nil {
		code := OpenErrToCode(err)
		errPayload := make([]byte, 5)
		binary.LittleEndian.PutUint32(errPayload[0:4], reqID)
		errPayload[4] = code
		_ = SendMsg(nc, MsgOpenResponse, errPayload)
		return
	}

	respPayload := make([]byte, 13)
	binary.LittleEndian.PutUint32(respPayload[0:4], reqID)
	respPayload[4] = 0
	binary.LittleEndian.PutUint64(respPayload[5:], inode)

	if err := SendMsgWithFD(nc, MsgOpenResponse, respPayload, fd); err != nil {
		log.Printf("%s socket [%s]: sendMsgWithFD: %v", s.logPrefix, namespaceID, err)
		s.handler.HandleFDPassFailed(namespaceID, inode)
	}
	_ = syscall.Close(fd)
}

// handleFsOp handles a MSG_FS_OP from the FUSE daemon.
//
// Payload format:
//
//	[4-byte request_id LE]
//	[1-byte op_type: 1=mkdir, 2=unlink, 3=rmdir, 4=rename]
//	[4-byte mode LE]
//	[null-terminated path1]
//	[null-terminated path2 (rename only; empty otherwise)]
func (s *SocketServer) handleFsOp(namespaceID string, nc *NsConn, payload []byte) {
	if len(payload) < 9 {
		return
	}

	reqID := binary.LittleEndian.Uint32(payload[0:4])
	op := payload[4]
	mode := binary.LittleEndian.Uint32(payload[5:9])
	rest := payload[9:]

	// Parse null-terminated path1.
	idx1 := bytes.IndexByte(rest, 0)
	var path1, path2 string
	if idx1 < 0 {
		path1 = string(rest)
	} else {
		path1 = string(rest[:idx1])
		rest = rest[idx1+1:]
		// Parse optional null-terminated path2.
		idx2 := bytes.IndexByte(rest, 0)
		if idx2 < 0 {
			path2 = string(rest)
		} else {
			path2 = string(rest[:idx2])
		}
	}

	fsh, hasFSH := s.handler.(FSOpHandler)
	if !hasFSH {
		s.sendFsOpResponse(nc, reqID, uint32(syscall.EPERM), 0, 0, 0)
		return
	}

	switch op {
	case FsOpMkdir:
		inode, mtimeSec, mtimeNsec, err := fsh.HandleMkdir(namespaceID, path1, mode)
		if err != nil {
			s.sendFsOpResponse(nc, reqID, toErrno(err), 0, 0, 0)
		} else {
			s.sendFsOpResponse(nc, reqID, 0, inode, mtimeSec, mtimeNsec)
		}
	case FsOpUnlink:
		err := fsh.HandleUnlink(namespaceID, path1)
		s.sendFsOpResponse(nc, reqID, toErrno(err), 0, 0, 0)
	case FsOpRmdir:
		err := fsh.HandleRmdir(namespaceID, path1)
		s.sendFsOpResponse(nc, reqID, toErrno(err), 0, 0, 0)
	case FsOpRename:
		err := fsh.HandleRename(namespaceID, path1, path2)
		s.sendFsOpResponse(nc, reqID, toErrno(err), 0, 0, 0)
	default:
		s.sendFsOpResponse(nc, reqID, uint32(syscall.ENOSYS), 0, 0, 0)
	}
}

// sendFsOpResponse sends a MSG_FS_OP_RESPONSE.
//
// Payload format:
//
//	[4-byte request_id LE]
//	[4-byte errno LE] (0 = success)
//	[8-byte inode LE]
//	[8-byte mtime_sec LE]
//	[4-byte mtime_nsec LE]
func (s *SocketServer) sendFsOpResponse(nc *NsConn, reqID uint32, errnum uint32, inode uint64, mtimeSec int64, mtimeNsec uint32) {
	buf := make([]byte, 28)
	binary.LittleEndian.PutUint32(buf[0:], reqID)
	binary.LittleEndian.PutUint32(buf[4:], errnum)
	binary.LittleEndian.PutUint64(buf[8:], inode)
	binary.LittleEndian.PutUint64(buf[16:], uint64(mtimeSec))
	binary.LittleEndian.PutUint32(buf[24:], mtimeNsec)
	_ = SendMsg(nc, MsgFsOpResponse, buf)
}

// toErrno converts an error to a uint32 errno value (0 on nil).
func toErrno(err error) uint32 {
	if err == nil {
		return 0
	}
	if errno, ok := err.(syscall.Errno); ok {
		return uint32(errno)
	}
	return uint32(syscall.EIO)
}

// OpenErrToCode converts a Go error to an OPEN_RESPONSE result code.
func OpenErrToCode(err error) byte {
	if err == nil {
		return 0
	}
	errno, ok := err.(syscall.Errno)
	if !ok {
		return 2 // EIO
	}
	switch errno {
	case syscall.ENOENT:
		return 1
	case syscall.EIO:
		return 2
	case syscall.EAGAIN:
		return 3
	case syscall.EACCES:
		return 5
	default:
		return 2
	}
}

func (s *SocketServer) runHealthPing(namespaceID string, nc *NsConn) {
	ticker := time.NewTicker(healthPingInterval)
	defer ticker.Stop()

	consecutive := 0

	for {
		select {
		case <-nc.stopCh:
			return
		case <-ticker.C:
			if err := SendMsg(nc, MsgHealthPing, nil); err != nil {
				return
			}

			pongReceived := false
			deadline := time.After(healthPongTimeout)
		waitPong:
			for {
				select {
				case <-nc.stopCh:
					return
				case <-nc.pongCh:
					pongReceived = true
					break waitPong
				case <-deadline:
					break waitPong
				}
			}

			if pongReceived {
				consecutive = 0
			} else {
				consecutive++
				log.Printf("%s socket [%s]: missed HEALTH_PONG (%d/%d)", s.logPrefix, namespaceID, consecutive, healthMissLimit)
				if consecutive >= healthMissLimit {
					s.handler.OnHealthFail(namespaceID)
					return
				}
			}
		}
	}
}

// SendMsg sends a binary-framed message over nc.
func SendMsg(nc *NsConn, msgType uint32, payload []byte) error {
	nc.sendMu.Lock()
	defer nc.sendMu.Unlock()

	hdr := make([]byte, 8)
	binary.LittleEndian.PutUint32(hdr[0:], msgType)
	binary.LittleEndian.PutUint32(hdr[4:], uint32(len(payload)))

	data := append(hdr, payload...)
	_, err := nc.conn.Write(data)
	return err
}

// SendMsgWithFD sends a binary-framed message with an fd via SCM_RIGHTS.
// The header is sent first via a normal write so the daemon's reader thread
// can read it with read(). The payload is then sent via sendmsg with the fd
// attached as SCM_RIGHTS ancillary data, which the daemon receives via recvmsg.
func SendMsgWithFD(nc *NsConn, msgType uint32, payload []byte, fd int) error {
	nc.sendMu.Lock()
	defer nc.sendMu.Unlock()

	hdr := make([]byte, 8)
	binary.LittleEndian.PutUint32(hdr[0:], msgType)
	binary.LittleEndian.PutUint32(hdr[4:], uint32(len(payload)))

	// Send header via normal write (no ancillary data).
	if _, err := nc.conn.Write(hdr); err != nil {
		return fmt.Errorf("write header: %w", err)
	}

	// Send payload + fd via sendmsg so SCM_RIGHTS arrives with recvmsg.
	rights := syscall.UnixRights(fd)
	rawConn, err := nc.conn.SyscallConn()
	if err != nil {
		return fmt.Errorf("SyscallConn: %w", err)
	}
	var sendErr error
	err = rawConn.Control(func(s uintptr) {
		sendErr = syscall.Sendmsg(int(s), payload, rights, nil, 0)
	})
	if err != nil {
		return fmt.Errorf("rawConn.Control: %w", err)
	}
	return sendErr
}

// RecvMsg reads one binary-framed message from conn.
func RecvMsg(conn net.Conn) (uint32, []byte, error) {
	hdr := make([]byte, 8)
	if _, err := readFull(conn, hdr); err != nil {
		return 0, nil, fmt.Errorf("read header: %w", err)
	}

	msgType := binary.LittleEndian.Uint32(hdr[0:])
	payloadLen := binary.LittleEndian.Uint32(hdr[4:])

	if payloadLen > maxPayload {
		return 0, nil, fmt.Errorf("payload length %d exceeds maximum %d", payloadLen, maxPayload)
	}

	if payloadLen == 0 {
		return msgType, nil, nil
	}

	payload := make([]byte, payloadLen)
	if _, err := readFull(conn, payload); err != nil {
		return 0, nil, fmt.Errorf("read payload: %w", err)
	}

	return msgType, payload, nil
}

func readFull(conn net.Conn, buf []byte) (int, error) {
	total := 0
	for total < len(buf) {
		n, err := conn.Read(buf[total:])
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}
