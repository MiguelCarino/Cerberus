package sysmon

import (
	"errors"
	"os"
	"testing"
	"time"

	"carino.systems/cerberus/internal/config"
)

func TestReloadProtected(t *testing.T) {
	tr := newTree(t)
	uid := os.Getuid()
	app := "/user.slice/user-1000.slice/user@1000.service/app.slice/"
	tr.pid(1, 0, "systemd", 0, 0, "/init.scope", "")
	tr.pid(200, 1, "myeditor", 0, uid, app+"app-myeditor-1.scope", "")
	now := time.Unix(1700001000, 0)
	m := newTestMonitor(t, tr, &now)
	if _, err := m.checkTarget(200); err != nil {
		t.Fatalf("before reload: %v", err)
	}
	cfg := config.Defaults()
	cfg.Monitor.Protected = append(cfg.Monitor.Protected, "myeditor")
	m.Reload(cfg)
	if _, err := m.checkTarget(200); !errors.Is(err, ErrProtected) {
		t.Fatalf("after reload: %v, want ErrProtected", err)
	}
	m.Reload(nil)
}
