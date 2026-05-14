package nonraid

import core "github.com/RakuenSoftware/nonraid"

const (
	MaxParityDevices = core.MaxParityDevices
	MaxDataDevices   = core.MaxDataDevices

	BackendKind = core.BackendKind

	RoleData   = core.RoleData
	RoleParity = core.RoleParity

	StateConfigured = core.StateConfigured
	StateActive     = core.StateActive
	StateError      = core.StateError

	DefaultFilesystem = core.DefaultFilesystem
	DefaultMountBase  = core.DefaultMountBase
	BackingBase       = core.BackingBase
)

type BlockDevice = core.BlockDevice
type Device = core.Device
type DevicePlan = core.DevicePlan
type Engine = core.Engine
type NBDServer = core.NBDServer
type Plan = core.Plan

var BuildPlan = core.BuildPlan
var DisconnectNBD = core.DisconnectNBD
var NewEngine = core.NewEngine
var StartNBD = core.StartNBD
