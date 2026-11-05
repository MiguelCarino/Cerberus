package main

import (
	"errors"
	"log/slog"
	"os"
)

// Self-restart (POST /api/config/restart, day phase 4). serve returns
// errRestart after a clean shutdown (sessions stopped, models it spawned
// unloaded, listeners closed); serveCmd then starts the daemon again:
//
//   - under the cerberus.service unit (it sets CERBERUS_SUPERVISOR=systemd,
//     and systemd sets INVOCATION_ID) it exits with restartExitCode, a
//     failure code, so the unit's RestartForceExitStatus starts it again.
//     INVOCATION_ID alone is not enough: every process started from a
//     terminal that runs as a systemd unit inherits it, and exiting there
//     would leave no daemon. A unit installed before the marker re-execs in
//     place, which systemd also follows (same pid);
//   - otherwise, on unix, it replaces itself in place with the same binary,
//     arguments and environment (syscall.Exec: same pid, same terminal);
//   - where re-exec is not possible (windows) it exits with
//     restartExitCode for whatever supervises it.

// errRestart is serve's result when a restart was requested.
var errRestart = errors.New("restart requested")

// restartExitCode is EX_TEMPFAIL: "try again", and a failure for systemd.
const restartExitCode = 75

// supervisorEnv is the marker packaging/cerberus.service sets.
const supervisorEnv = "CERBERUS_SUPERVISOR"

// supervised is read once at start; the marker is then removed from the
// environment so agent sessions and other children do not inherit it.
var supervised = takeSupervisor()

func takeSupervisor() bool {
	v := os.Getenv(supervisorEnv)
	_ = os.Unsetenv(supervisorEnv)
	return supervisedBy(v, os.Getenv("INVOCATION_ID"))
}

// supervisedBy reports whether the marker and systemd's INVOCATION_ID both
// say the cerberus.service unit started this process.
func supervisedBy(marker, invocationID string) bool {
	return marker == "systemd" && invocationID != ""
}

// underSystemd reports whether the systemd unit started this process.
func underSystemd() bool { return supervised }

// restartSelf starts the daemon again after errRestart. On success under
// re-exec it does not return.
func restartSelf(log *slog.Logger) int {
	if underSystemd() {
		log.Info("restarting: exiting for systemd to start the service again", "code", restartExitCode)
		return restartExitCode
	}
	log.Info("restarting in place")
	if err := execSelf(); err != nil {
		log.Error("re-exec failed; exiting for the supervisor", "err", err, "code", restartExitCode)
	}
	return restartExitCode
}
