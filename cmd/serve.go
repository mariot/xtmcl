package cmd

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/mariot/xtmcl/platform"
	"github.com/spf13/cobra"
)

var serveCmd = &cobra.Command{
	Use:    "serve",
	Short:  "Run the local platform in the foreground (started by up)",
	Hidden: true,
	Args:   cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		e, err := loadEnvironment()
		if err != nil {
			return err
		}
		return serve(e)
	},
}

func init() {
	rootCmd.AddCommand(serveCmd)
}

func serve(e *environment) error {
	store, err := platform.OpenStore(e.path(stateFile))
	if err != nil {
		return err
	}
	pub, err := platform.LoadPublicKey(e.path(keyFile))
	if err != nil {
		return err
	}
	var upstream *url.URL
	if e.Upstream != "" {
		if upstream, err = url.Parse(e.Upstream); err != nil {
			return fmt.Errorf("invalid upstream %q: %w", e.Upstream, err)
		}
	}
	logger := log.New(os.Stdout, "", log.LstdFlags)
	handler := platform.NewServer(store, platform.Options{
		Version:       platformVersion,
		ManagerID:     e.ManagerID,
		ComposerToken: e.ComposerToken,
		ControlToken:  e.ControlToken,
		PublicKey:     pub,
		Upstream:      upstream,
		Logger:        logger,
	})
	srv := &http.Server{
		Addr:              net.JoinHostPort(e.Listen, strconv.Itoa(e.Port)),
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}
	listener, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		return err
	}
	if err := os.WriteFile(e.path(pidFile), []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		return err
	}
	defer func() { _ = os.Remove(e.path(pidFile)) }()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-stop
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()

	logger.Printf("Platform listening on %s, Composer URL %s", srv.Addr, e.platformURL())
	if upstream != nil {
		logger.Printf("Forwarding the other requests to %s", upstream)
	}
	if err := srv.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	logger.Printf("Platform stopped")
	return nil
}

// serverPID returns the PID of the running platform server, or 0.
func serverPID(e *environment) int {
	data, err := os.ReadFile(e.path(pidFile))
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || !processAlive(pid) {
		return 0
	}
	return pid
}

// stopServer stops the platform server, if it runs, and waits for it to exit.
func stopServer(e *environment) error {
	pid := serverPID(e)
	if pid == 0 {
		return nil
	}
	if err := stopProcess(pid); err != nil {
		return err
	}
	for i := 0; i < 50 && processAlive(pid); i++ {
		time.Sleep(100 * time.Millisecond)
	}
	if processAlive(pid) {
		return fmt.Errorf("the platform server (pid %d) didn't stop", pid)
	}
	return nil
}

// startServer starts the platform server in the background and waits for it.
func startServer(e *environment) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	logFile, err := os.OpenFile(e.path(serverLogFile), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = logFile.Close() }()
	if err := startDetached(exe, []string{"serve"}, logFile); err != nil {
		return err
	}
	client := newControlClient(e)
	var lastErr error
	for i := 0; i < 50; i++ {
		var st platform.Status
		if lastErr = client.do("GET", "/control/status", nil, &st); lastErr == nil {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("the platform server didn't start (see %s): %w", e.path(serverLogFile), lastErr)
}
