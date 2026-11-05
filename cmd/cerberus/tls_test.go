package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"carino.systems/cerberus/internal/config"
)

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// tlsEcho answers "tls" or "plain" depending on how the request arrived.
var tlsEcho = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	if r.TLS != nil {
		io.WriteString(w, "tls")
		return
	}
	io.WriteString(w, "plain")
})

// writeSelfSigned writes a self-signed cert for 127.0.0.1 with the given
// serial and returns its parsed form.
func writeSelfSigned(t *testing.T, certFile, keyFile string, serial int64) *x509.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(serial),
		Subject:               pkix.Name{CommonName: "cerberus-test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kder, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder}), 0o600); err != nil {
		t.Fatal(err)
	}
	c, _ := x509.ParseCertificate(der)
	return c
}

func getBody(t *testing.T, c *http.Client, url string) string {
	t.Helper()
	resp, err := c.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

// TestListenTLSExtra: extras serve HTTPS with the configured pair, the
// primary stays HTTP, and an mtime change swaps the certificate.
func TestListenTLSExtra(t *testing.T) {
	old := certCheckInterval
	certCheckInterval = 20 * time.Millisecond
	defer func() { certCheckInterval = old }()

	dir := t.TempDir()
	certFile, keyFile := filepath.Join(dir, "c.pem"), filepath.Join(dir, "k.pem")
	writeSelfSigned(t, certFile, keyFile, 1)
	sc := config.ServerConfig{Listen: "127.0.0.1:0", ExtraListen: []string{"127.0.0.1:0"}, TLSCert: certFile, TLSKey: keyFile}
	ls, addrs, err := listenAll(sc, tlsEcho, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	defer ls.shutdown(context.Background())
	if len(addrs) != 2 {
		t.Fatalf("addrs = %v", addrs)
	}
	if got := getBody(t, http.DefaultClient, "http://"+addrs[0]+"/"); got != "plain" {
		t.Fatalf("primary answered %q", got)
	}

	// The test trusts any cert but records which one the server presented.
	var seen *x509.Certificate
	tr := &http.Transport{TLSClientConfig: &tls.Config{
		InsecureSkipVerify: true,
		VerifyConnection: func(cs tls.ConnectionState) error {
			seen = cs.PeerCertificates[0]
			return nil
		},
	}, DisableKeepAlives: true}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr}
	if got := getBody(t, client, "https://"+addrs[1]+"/"); got != "tls" {
		t.Fatalf("extra answered %q", got)
	}
	if seen.SerialNumber.Int64() != 1 {
		t.Fatalf("serial = %v", seen.SerialNumber)
	}

	writeSelfSigned(t, certFile, keyFile, 2)
	future := time.Now().Add(time.Minute) // mtime must differ even on coarse filesystems
	os.Chtimes(certFile, future, future)
	os.Chtimes(keyFile, future, future)
	deadline := time.Now().Add(5 * time.Second)
	for {
		getBody(t, client, "https://"+addrs[1]+"/")
		if seen.SerialNumber.Int64() == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("certificate not reloaded after mtime change")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestCertReloaderMissingFiles: a missing pair is not fatal; handshakes fail
// until the files appear and a reload runs.
func TestCertReloaderMissingFiles(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile := filepath.Join(dir, "c.pem"), filepath.Join(dir, "k.pem")
	cr := newCertReloader(certFile, keyFile, quietLog())
	if _, err := cr.getCertificate(nil); err == nil {
		t.Fatal("expected error without a certificate")
	}
	writeSelfSigned(t, certFile, keyFile, 7)
	if !cr.changed() {
		t.Fatal("changed() = false after files appeared")
	}
	if err := cr.reload(); err != nil {
		t.Fatal(err)
	}
	if c, err := cr.getCertificate(nil); err != nil || c == nil {
		t.Fatalf("getCertificate: %v", err)
	}
	if cr.changed() {
		t.Fatal("changed() = true right after reload")
	}
}

// TestListenRetryBinds: an extra address held by someone else binds once
// released, within the (shortened) backoff.
func TestListenRetryBinds(t *testing.T) {
	oldMin, oldMax := retryMin, retryMax
	retryMin, retryMax = 20*time.Millisecond, 80*time.Millisecond
	defer func() { retryMin, retryMax = oldMin, oldMax }()

	holder, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	extra := holder.Addr().String()
	ls, addrs, err := listenAll(config.ServerConfig{Listen: "127.0.0.1:0", ExtraListen: []string{extra}}, tlsEcho, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	defer ls.shutdown(context.Background())
	if len(addrs) != 1 {
		t.Fatalf("held extra reported as bound: %v", addrs)
	}
	time.Sleep(100 * time.Millisecond) // let a few retries fail first
	holder.Close()

	client := &http.Client{Timeout: time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	deadline := time.Now().Add(3 * time.Second)
	for {
		if resp, err := client.Get("http://" + extra + "/"); err == nil {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if string(b) != "plain" {
				t.Fatalf("extra answered %q", b)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("extra address never bound after release")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestListenShutdownStopsRetry: shutdown returns promptly while a retry is
// waiting out a long backoff, and leaves no goroutines behind.
func TestListenShutdownStopsRetry(t *testing.T) {
	oldMin := retryMin
	retryMin = time.Hour
	defer func() { retryMin = oldMin }()

	holder, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	dir := t.TempDir()
	certFile, keyFile := filepath.Join(dir, "c.pem"), filepath.Join(dir, "k.pem")
	writeSelfSigned(t, certFile, keyFile, 1)

	before := runtime.NumGoroutine()
	sc := config.ServerConfig{Listen: "127.0.0.1:0", ExtraListen: []string{holder.Addr().String(), "127.0.0.1:0"},
		TLSCert: certFile, TLSKey: keyFile}
	ls, _, err := listenAll(sc, tlsEcho, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { ls.shutdown(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown blocked on the retry goroutine")
	}
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > before {
		if time.Now().After(deadline) {
			buf := make([]byte, 1<<16)
			t.Fatalf("goroutines %d > %d after shutdown\n%s", runtime.NumGoroutine(), before, buf[:runtime.Stack(buf, true)])
		}
		time.Sleep(10 * time.Millisecond)
	}
}
