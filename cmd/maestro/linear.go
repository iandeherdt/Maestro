package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/iandeherdt/Maestro/internal/setup"
	"github.com/iandeherdt/Maestro/internal/tracker/linear"
)

const linearUsage = `usage:
  maestro linear          whether a Linear API key is stored, and whom it acts as
  maestro linear key      store the Linear API key the service uses for the board
  maestro linear remove   delete the stored key

The key is kept in the setup folder, readable by you only, and never reaches
the agent. Make it in Linear under Settings, Security & access, Personal API
keys, limited to the board's team with Read and Write. Best is a separate
Linear user for the service: what that user does never counts as your
approval.`

// linearEndpoint is Linear's API; tests point it elsewhere.
var linearEndpoint = linear.DefaultEndpoint

// linearKeyFile is where the Linear API key is kept.
func linearKeyFile(dir string) string { return setup.LinearKeyFile(dir) }

func readLinearKey(dir string) (string, error) { return setup.ReadLinearKey(dir) }

// linear builds the Linear board adapter and checks that its key works.
// The key comes from the setup folder, or else from the variable the
// settings file names.
func (in *install) linear(ctx context.Context, log *slog.Logger) (*linear.Adapter, string, error) {
	opt := linear.Options{Env: os.Getenv, Log: log}
	key, err := readLinearKey(in.dir)
	if err != nil {
		return nil, "", err
	}
	if key != "" {
		opt.Key = func(context.Context) (string, error) { return key, nil }
	}
	la, err := linear.New(in.cfg, opt)
	if err != nil {
		if key == "" {
			return nil, "", explain("%v\nStore the Linear API key with: %s", err, mention("maestro linear key"))
		}
		return nil, "", err
	}
	if _, email, err := la.Me(ctx); err != nil {
		return nil, "", explain("Linear did not accept the API key: %v\nStore a new one with: %s", err, mention("maestro linear key"))
	} else if log != nil {
		log.Info("Linear board", "team", in.cfg.Tracker.Provider["team_key"], "acting_as", email)
	}
	// The key is a secret the secret scan looks for in the agent's work.
	if key, err = la.Key(ctx); err != nil {
		return nil, "", err
	}
	return la, key, nil
}

// runLinear is `maestro linear`.
func runLinear(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	dir, _, rest, ok := parseWords("linear", args, stderr, true)
	if !ok {
		return 2
	}
	if dir == "" {
		found, err := configDir()
		if err != nil {
			return report(stderr, explain("no configuration directory was found for this user, pass --dir: %v", err))
		}
		dir = found
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		return report(stderr, err)
	}
	ctx, stop := interruptible()
	defer stop()
	switch {
	case len(rest) == 0:
		key, err := readLinearKey(dir)
		if err != nil || key == "" {
			fmt.Fprintln(stdout, "No Linear API key is stored. Store one with: "+mention("maestro linear key"))
			return 0
		}
		email, err := linearWho(ctx, dir, key)
		if err != nil {
			return report(stderr, err)
		}
		fmt.Fprintf(stdout, "A Linear API key is stored in %s. It acts as %s.\n", linearKeyFile(dir), plain(email, 100))
		return 0
	case len(rest) == 1 && rest[0] == "remove":
		if err := os.Remove(linearKeyFile(dir)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return report(stderr, err)
		}
		fmt.Fprintln(stdout, "The Linear API key is removed from this machine. Revoke it in Linear's settings too.")
		return 0
	case len(rest) == 1 && rest[0] == "key":
		r := bufio.NewReader(stdin)
		key, err := askSecret(r, stdin, stdout, "Linear API key (it is not shown): ")
		if err != nil {
			return report(stderr, err)
		}
		key = strings.TrimSpace(key)
		if setup.ValidLinearKey(key) != nil {
			fmt.Fprintln(stderr, "That is not an API key. Nothing was stored.")
			return 1
		}
		email, err := linearWho(ctx, dir, key)
		if err != nil {
			fmt.Fprintln(stderr, "Nothing was stored.")
			return report(stderr, err)
		}
		if err := setup.WriteLinearKey(dir, key); err != nil {
			return report(stderr, err)
		}
		fmt.Fprintf(stdout, "Stored. The key acts as %s in Linear.\n", plain(email, 100))
		fmt.Fprintln(stdout, "Restart the service to use it: maestro service install, or stop and start maestro run.")
		return 0
	}
	fmt.Fprintln(stderr, linearUsage)
	return 2
}

// linearWho asks Linear whom the key acts as. The settings file names the
// endpoint, which only matters in tests; without one it is Linear's own.
func linearWho(ctx context.Context, dir, key string) (string, error) {
	c, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	endpoint := linearEndpoint
	if in, err := open(dir, "", nil); err == nil && in.cfg.Tracker.Kind == "linear" {
		if e, ok := in.cfg.Tracker.Provider["endpoint"].(string); ok && e != "" {
			endpoint = e
		}
	}
	_, email, err := linear.Who(c, endpoint, key)
	if err != nil {
		return "", explain("Linear did not accept the key: %v", err)
	}
	return email, nil
}
