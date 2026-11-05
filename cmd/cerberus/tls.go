package main

import (
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

// certCheckInterval is how often the cert files' mtimes are compared;
// `tailscale cert` renewals are rare, so an hourly look costs nothing.
var certCheckInterval = time.Hour

// certReloader serves the current key pair to TLS handshakes and swaps it
// when the files change. A failed reload keeps the previous pair.
type certReloader struct {
	certFile, keyFile string
	log               *slog.Logger

	mu      sync.RWMutex
	cert    *tls.Certificate
	modCert time.Time
	modKey  time.Time
}

func newCertReloader(certFile, keyFile string, log *slog.Logger) *certReloader {
	c := &certReloader{certFile: certFile, keyFile: keyFile, log: log}
	if err := c.reload(); err != nil {
		// Not fatal: the loopback listener still works, and a later SIGHUP or
		// mtime check picks the files up once they exist.
		log.Error("tls certificate not loaded", "cert", certFile, "key", keyFile, "err", err)
	}
	return c
}

func (c *certReloader) reload() error {
	mc, mk := modTime(c.certFile), modTime(c.keyFile)
	pair, err := tls.LoadX509KeyPair(c.certFile, c.keyFile)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.cert, c.modCert, c.modKey = &pair, mc, mk
	c.mu.Unlock()
	return nil
}

// changed reports whether either file's mtime differs from the loaded pair's.
func (c *certReloader) changed() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.cert == nil || !modTime(c.certFile).Equal(c.modCert) || !modTime(c.keyFile).Equal(c.modKey)
}

func (c *certReloader) getCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.cert == nil {
		return nil, errors.New("no tls certificate loaded")
	}
	return c.cert, nil
}

func (c *certReloader) tlsConfig() *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: c.getCertificate}
}

// watch reloads on SIGHUP (always) and on the periodic mtime check (when
// changed) until ctx ends.
func (c *certReloader) watch(ctx context.Context) {
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	tick := time.NewTicker(certCheckInterval)
	defer tick.Stop()
	for {
		var why string
		select {
		case <-ctx.Done():
			return
		case <-hup:
			why = "sighup"
		case <-tick.C:
			if !c.changed() {
				continue
			}
			why = "files changed"
		}
		if err := c.reload(); err != nil {
			c.log.Error("tls certificate reload failed", "reason", why, "err", err)
		} else {
			c.log.Info("tls certificate reloaded", "reason", why)
		}
	}
}

func modTime(path string) time.Time {
	st, err := os.Stat(path)
	if err != nil {
		return time.Time{}
	}
	return st.ModTime()
}
