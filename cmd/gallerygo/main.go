// Command gallerygo runs the private gallery service and database tools.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"gallerygo/internal/app"
	"gallerygo/internal/auth"
	"gallerygo/internal/sandbox"
	"gallerygo/internal/store"
	"gallerygo/migrations"
)

var version = "2.0.0-dev.0"

func main() {
	if len(os.Args) > 1 && os.Args[1] == sandbox.ChildCommand {
		os.Exit(sandbox.RunChild(os.Args[2:]))
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, os.Args[1:], os.Stdout); err != nil {
		slog.Error("command failed", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, out io.Writer) error {
	return runWithInput(ctx, args, os.Stdin, out)
}

func runWithInput(ctx context.Context, args []string, input io.Reader, out io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: gallerygo <serve|worker|migrate|bootstrap-admin|doctor|healthcheck|version> [--config config.yaml]")
	}
	if args[0] == "version" {
		if len(args) != 1 {
			return errors.New("version accepts no arguments")
		}
		_, err := fmt.Fprintln(out, version)
		return err
	}
	if args[0] != "serve" && args[0] != "worker" && args[0] != "migrate" && args[0] != "doctor" && args[0] != "healthcheck" && args[0] != "bootstrap-admin" {
		return fmt.Errorf("unknown command %q", args[0])
	}
	flags := flag.NewFlagSet(args[0], flag.ContinueOnError)
	flags.SetOutput(out)
	configPath := flags.String("config", "config.yaml", "Path to the YAML configuration")
	var username string
	var passwordStdin bool
	var workerCheck bool
	if args[0] == "healthcheck" {
		flags.BoolVar(&workerCheck, "worker", false, "Check shared worker readiness from the local database without HTTP")
	}
	if args[0] == "bootstrap-admin" {
		flags.StringVar(&username, "username", "", "Initial administrator username")
		flags.BoolVar(&passwordStdin, "password-stdin", false, "Read one password line from standard input instead of the terminal")
	}
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	c, err := app.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	if args[0] == "serve" {
		return app.Serve(ctx, c, version)
	}
	if args[0] == "worker" {
		return app.Worker(ctx, c)
	}
	if args[0] == "healthcheck" && workerCheck {
		state, err := app.WorkerReadiness(ctx, c)
		if err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(state)
	}
	if args[0] == "healthcheck" {
		host, port, _ := net.SplitHostPort(c.Listen)
		if host == "" || host == "0.0.0.0" {
			host = "127.0.0.1"
		}
		if host == "::" {
			host = "::1"
		}
		u := "http://" + net.JoinHostPort(host, port) + "/api/v2/health/ready"
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return err
		}
		response, err := (&http.Client{Timeout: 3 * time.Second}).Do(request)
		if err != nil {
			return err
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			return errors.New("server is not ready")
		}
		_, err = io.Copy(out, io.LimitReader(response.Body, 4096))
		return err
	}
	db, err := store.Open(ctx, c.Database)
	if err != nil {
		return err
	}
	defer db.Close()
	if args[0] == "migrate" {
		if err := store.Migrate(ctx, db, migrations.Files); err != nil {
			return err
		}
		_, err = fmt.Fprintln(out, "Database migrations applied.")
		return err
	}
	if err := store.VerifySchema(ctx, db, migrations.Files); err != nil {
		return err
	}
	if args[0] == "bootstrap-admin" {
		if _, err := auth.NormalizeUsername(username); err != nil {
			return err
		}
		password, err := readPassword(input, out, passwordStdin)
		if err != nil {
			return err
		}
		service, err := auth.New(db, c.Auth)
		if err != nil {
			return err
		}
		user, err := service.Bootstrap(ctx, username, password)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(out, "Administrator %s created (%s).\n", user.Username, user.ID)
		return err
	}
	var integrity string
	if err := db.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&integrity); err != nil {
		return err
	}
	if integrity != "ok" {
		return errors.New("database integrity check failed")
	}
	return json.NewEncoder(out).Encode(map[string]string{"database": "ok", "schema": "ok", "version": version})
}
