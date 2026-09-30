// Package testdb gives integration tests a disposable Postgres database. When
// TEST_DATABASE_URL is unset it starts a throwaway Docker container on the first
// free port at or above BasePort and removes it when the tests finish.
package testdb

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	// BasePort is deliberately far from Postgres's 5432 and the dev database's 54329.
	BasePort  = 55432
	portTries = 50
	image     = "postgres:16-alpine"
	readyWait = 60 * time.Second
)

// Main runs an integration test binary. An explicit TEST_DATABASE_URL (for
// example a provider database, to check its TLS settings) is used unchanged.
func Main(m *testing.M) {
	if os.Getenv("TEST_DATABASE_URL") != "" {
		os.Exit(m.Run())
	}
	dsn, stop, err := Start()
	if err != nil {
		fmt.Fprintf(os.Stderr, "testdb: %v\n", err)
		os.Exit(1)
	}
	if err := os.Setenv("TEST_DATABASE_URL", dsn); err != nil {
		stop()
		fmt.Fprintf(os.Stderr, "testdb: %v\n", err)
		os.Exit(1)
	}
	code := m.Run()
	stop()
	os.Exit(code)
}

// Start launches a Postgres container and returns its DSN and a function that
// removes it. `go test` runs packages in parallel, so each package gets its own
// container; if another process claims a port between the check and Docker
// binding it, Start moves on to the next port.
func Start() (string, func(), error) {
	if _, err := exec.LookPath("docker"); err != nil {
		return "", nil, errors.New("docker is required to start a test database (or set TEST_DATABASE_URL)")
	}
	for port := BasePort; port < BasePort+portTries; port++ {
		if !PortAvailable(port) {
			continue
		}
		name := fmt.Sprintf("gather-your-party-test-%d-%d", os.Getpid(), port)
		out, err := exec.Command("docker", "run", "-d", "--rm", "--name", name,
			"--label", "gather-your-party-test=1",
			"-e", "POSTGRES_USER=test", "-e", "POSTGRES_PASSWORD=test", "-e", "POSTGRES_DB=test",
			"-p", "127.0.0.1:"+strconv.Itoa(port)+":5432",
			image).CombinedOutput()
		stop := func() { _ = exec.Command("docker", "rm", "-f", "-v", name).Run() }
		if err != nil {
			stop()
			if msg := string(out); strings.Contains(msg, "already allocated") || strings.Contains(msg, "address already in use") {
				continue
			}
			return "", nil, fmt.Errorf("docker run: %v: %s", err, strings.TrimSpace(string(out)))
		}
		dsn := fmt.Sprintf("postgres://test:test@127.0.0.1:%d/test?sslmode=disable", port)
		if err := waitReady(name, dsn); err != nil {
			stop()
			return "", nil, err
		}
		return dsn, stop, nil
	}
	return "", nil, fmt.Errorf("no free port in %d-%d", BasePort, BasePort+portTries-1)
}

// PortAvailable reports whether nothing is listening on the port and it can be
// bound on both loopback and all interfaces.
func PortAvailable(port int) bool {
	addr := "127.0.0.1:" + strconv.Itoa(port)
	if conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond); err == nil {
		_ = conn.Close()
		return false
	}
	for _, bind := range []string{addr, ":" + strconv.Itoa(port)} {
		l, err := net.Listen("tcp", bind)
		if err != nil {
			return false
		}
		_ = l.Close()
	}
	return true
}

// waitReady waits for the server to accept connections from the host. The image
// first runs a socket-only init server, so TCP pg_isready inside the container
// only succeeds once the real server is up.
func waitReady(name, dsn string) error {
	deadline := time.Now().Add(readyWait)
	for time.Now().Before(deadline) {
		if exec.Command("docker", "exec", name, "pg_isready", "-q", "-h", "127.0.0.1", "-U", "test", "-d", "test").Run() == nil {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			conn, err := pgx.Connect(ctx, dsn)
			if err == nil {
				err = conn.Ping(ctx)
				_ = conn.Close(ctx)
			}
			cancel()
			if err == nil {
				return nil
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	return fmt.Errorf("test database %s not ready after %s", name, readyWait)
}
