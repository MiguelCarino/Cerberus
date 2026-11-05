package platform

// Proc is one process: its id, its parent's and its session's.
type Proc struct{ PID, PPID, SID int }

// ListenSock is a listening TCP socket a preview may proxy to: bound to
// loopback (Loopback) or to every interface, over IPv6 only (V6) or IPv4
// (or both, for a dual-stack wildcard).
type ListenSock struct {
	Port     int
	Loopback bool
	V6       bool
}
