//go:build !linux

package platform

// SocketScan is false: live preview servers are not detected here, only
// the ones Cerberus starts itself (Run preview).
const SocketScan = false

// Procs is not available here.
func Procs() []Proc { return nil }

// ProcEnv is not available here.
func ProcEnv(pid int, name string) string { return "" }

// ProcUnits is not available here.
func ProcUnits(pid int) []string { return nil }

// ListenSockets is not available here.
func ListenSockets(pids []int) []ListenSock { return nil }
