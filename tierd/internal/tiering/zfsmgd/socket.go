package zfsmgd

import fusepkg "github.com/JBailes/SmoothNAS/tierd/internal/tiering/fuse"

// SocketServer is an alias for the shared FUSE socket server.
type SocketServer = fusepkg.SocketServer

// OpenHandler is an alias for the shared FUSE open handler interface.
type OpenHandler = fusepkg.OpenHandler

// DirEntry is an alias for the shared FUSE directory entry type.
type DirEntry = fusepkg.DirEntry

// NewSocketServer creates a SocketServer with the "zfsmgd" log prefix.
func NewSocketServer(socketDir string, handler OpenHandler) *SocketServer {
	s := fusepkg.NewSocketServer(socketDir, handler)
	s.SetLogPrefix("zfsmgd")
	return s
}
