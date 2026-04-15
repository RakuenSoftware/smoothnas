package zfsmgd

import fusepkg "github.com/JBailes/SmoothNAS/tierd/internal/tiering/fuse"

// DaemonSupervisor is an alias for the shared FUSE daemon supervisor.
type DaemonSupervisor = fusepkg.DaemonSupervisor

// NewDaemonSupervisor creates a new DaemonSupervisor.
var NewDaemonSupervisor = fusepkg.NewDaemonSupervisor
