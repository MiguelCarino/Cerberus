package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"strings"
	"text/tabwriter"

	"carino.systems/cerberus/internal/config"
	"carino.systems/cerberus/internal/vault"
)

// secretCmd manages the vault:
//
//	cerberus secret list
//	cerberus secret set NAME [--note TEXT]   value from stdin
//	cerberus secret rm NAME
//
// It goes through the running daemon (which owns the vault and its cache)
// and opens the vault directly only when no daemon answers. For set, the
// value is read from stdin: pipe it (`printf %s "$KEY" | cerberus secret set
// OPENAI`, or `< keyfile`) to keep it out of shell history and argv. On a
// terminal it is prompted for with echo turned off where the platform allows.
func secretCmd(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: cerberus secret list | set NAME [--note TEXT] | rm NAME")
		return 2
	}
	sub, rest := args[0], args[1:]
	if isHelpArg(sub) {
		printHelp(stdout, "secret")
		return 0
	}
	var note string
	cfg, pos, code := clientCmd("secret "+sub, rest, stderr, func(fs *flag.FlagSet) {
		if sub == "set" {
			fs.StringVar(&note, "note", "", "what the secret is for (shown in lists)")
		}
	})
	if cfg == nil {
		return code
	}
	be := &secretClient{api: newClient(cfg), cfg: cfg, stderr: stderr}
	switch sub {
	case "list", "ls":
		if len(pos) != 0 {
			fmt.Fprintln(stderr, "usage: cerberus secret list")
			return 2
		}
		backend, items, err := be.list()
		if err != nil {
			fmt.Fprintln(stderr, "cerberus secret:", err)
			return 1
		}
		printSecrets(stdout, backend, items)
		return 0
	case "set":
		if len(pos) != 1 {
			fmt.Fprintln(stderr, "usage: cerberus secret set NAME [--note TEXT]   (value on stdin)")
			return 2
		}
		name := pos[0]
		if !vault.ValidName(name) {
			fmt.Fprintln(stderr, "cerberus secret:", vault.ErrInvalidName)
			return 2
		}
		value, err := readSecretValue(stdin, stderr, name)
		if err != nil {
			fmt.Fprintln(stderr, "cerberus secret:", err)
			return 1
		}
		if err := be.set(name, value, note); err != nil {
			fmt.Fprintln(stderr, "cerberus secret:", scrubValue(err.Error(), value))
			return 1
		}
		fmt.Fprintf(stdout, "stored %s (%d bytes)\n", name, len(value))
		return 0
	case "rm", "delete", "del":
		if len(pos) != 1 {
			fmt.Fprintln(stderr, "usage: cerberus secret rm NAME")
			return 2
		}
		if !vault.ValidName(pos[0]) {
			fmt.Fprintln(stderr, "cerberus secret:", vault.ErrInvalidName)
			return 2
		}
		if err := be.rm(pos[0]); err != nil {
			fmt.Fprintln(stderr, "cerberus secret:", err)
			return 1
		}
		fmt.Fprintln(stdout, "removed", pos[0])
		return 0
	}
	fmt.Fprintf(stderr, "cerberus secret: unknown subcommand %q (list, set, rm)\n", sub)
	return 2
}

// readSecretValue reads the value: one line from a terminal (echo off where
// possible), or everything piped in, minus one trailing line ending.
func readSecretValue(stdin io.Reader, stderr io.Writer, name string) (string, error) {
	if stdin == nil {
		return "", errors.New("no value: pipe it on stdin")
	}
	var value string
	var err error
	if isTerminal(stdin) {
		fmt.Fprintf(stderr, "Value for %s: ", name)
		restore := noEcho(stdin)
		value, err = readLine(stdin, vault.MaxValueBytes)
		restore()
		fmt.Fprintln(stderr)
	} else {
		var raw []byte
		raw, err = io.ReadAll(io.LimitReader(stdin, vault.MaxValueBytes+3))
		if len(raw) > vault.MaxValueBytes+2 {
			return "", vault.ErrValueSize
		}
		value = strings.TrimSuffix(strings.TrimSuffix(string(raw), "\n"), "\r")
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	if value == "" {
		return "", errors.New("empty value (pipe it on stdin, e.g. printf %s \"$KEY\" | cerberus secret set NAME)")
	}
	if len(value) > vault.MaxValueBytes {
		return "", vault.ErrValueSize
	}
	return value, nil
}

func printSecrets(w io.Writer, backend string, items []vault.Item) {
	fmt.Fprintf(w, "Vault backend: %s\n", backend)
	if len(items) == 0 {
		fmt.Fprintln(w, "No secrets stored.")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tLENGTH\tUPDATED\tNOTE")
	for _, it := range items {
		upd := "-"
		if !it.Updated.IsZero() {
			upd = it.Updated.Local().Format("2006-01-02 15:04")
		}
		fmt.Fprintf(tw, "%s\t%d\t%s\t%s\n", it.Name, it.Length, upd, it.Note)
	}
	tw.Flush()
}

func scrubValue(msg, value string) string {
	if value == "" {
		return msg
	}
	return strings.ReplaceAll(msg, value, "••••••")
}

// secretClient talks to the daemon, falling back to the local vault when
// the daemon does not answer.
type secretClient struct {
	api    *client
	cfg    *config.Config
	stderr io.Writer
}

func (s *secretClient) local(err error) (*vault.Vault, bool, error) {
	var ue *unreachableError
	if !errors.As(err, &ue) {
		return nil, false, err
	}
	fmt.Fprintln(s.stderr, "cerberus secret: the daemon is not running; using the vault directly")
	v, oerr := vault.Open(s.cfg, slog.New(slog.DiscardHandler))
	return v, true, oerr
}

func (s *secretClient) list() (string, []vault.Item, error) {
	var out struct {
		Backend string       `json:"backend"`
		Items   []vault.Item `json:"items"`
	}
	err := s.api.do(context.Background(), "GET", "/api/secrets", nil, &out)
	if err == nil {
		return out.Backend, out.Items, nil
	}
	v, ok, err := s.local(err)
	if !ok || err != nil {
		return "", nil, err
	}
	items, err := v.List()
	return v.Backend(), items, err
}

func (s *secretClient) set(name, value, note string) error {
	body := map[string]string{"value": value, "note": note}
	err := s.api.do(context.Background(), "PUT", "/api/secrets/"+url.PathEscape(name), body, nil)
	if err == nil {
		return nil
	}
	v, ok, err := s.local(err)
	if !ok || err != nil {
		return err
	}
	err = v.Set(name, value, note)
	cliAudit(s.stderr, "secret.set", outcome(err), name)
	return err
}

func (s *secretClient) rm(name string) error {
	err := s.api.do(context.Background(), "DELETE", "/api/secrets/"+url.PathEscape(name), nil, nil)
	if err == nil {
		return nil
	}
	v, ok, err := s.local(err)
	if !ok || err != nil {
		return err
	}
	err = v.Delete(name)
	cliAudit(s.stderr, "secret.delete", outcome(err), name)
	return err
}
