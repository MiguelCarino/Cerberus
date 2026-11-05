package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"carino.systems/cerberus/internal/config"
)

// parseAnywhere parses flags that may come before, between or after the
// positional arguments (Go's flag package stops at the first positional),
// and returns the positionals. Everything after "--" is positional.
func parseAnywhere(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return pos, nil
		}
		if i := len(args) - len(rest); i > 0 && args[i-1] == "--" {
			return append(pos, rest...), nil
		}
		pos = append(pos, rest[0])
		args = rest[1:]
	}
}

// clientCmd is clientFlags with flags allowed anywhere. It returns the
// config and positionals, or a nil config and the exit code.
func clientCmd(name string, args []string, stderr io.Writer, extra func(*flag.FlagSet)) (*config.Config, []string, int) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	setUsage(fs)
	cfgPath := fs.String("config", "", "config file (default ~/.config/cerberus/config.json)")
	if extra != nil {
		extra(fs)
	}
	pos, err := parseAnywhere(fs, args)
	if err != nil {
		return nil, nil, 2
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintln(stderr, "cerberus:", err)
		return nil, nil, 1
	}
	return cfg, pos, 0
}

// isTerminal reports whether r is an interactive terminal.
func isTerminal(r io.Reader) bool {
	f, ok := r.(*os.File)
	if !ok {
		return false
	}
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

// readLine reads one line (without its line ending) from r. At most max
// bytes are kept; a longer line is an error.
func readLine(r io.Reader, max int) (string, error) {
	br := bufio.NewReaderSize(r, 4096)
	var sb strings.Builder
	for {
		chunk, err := br.ReadSlice('\n')
		sb.Write(chunk)
		if sb.Len() > max+2 {
			return "", fmt.Errorf("input longer than %d bytes", max)
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		if err != nil && err != io.EOF {
			return "", err
		}
		line := strings.TrimSuffix(strings.TrimSuffix(sb.String(), "\n"), "\r")
		if err == io.EOF && sb.Len() == 0 {
			return "", io.EOF
		}
		return line, nil
	}
}

// confirm asks a yes/no question; only "y" or "yes" agree.
func confirm(stdin io.Reader, out io.Writer, question string) bool {
	fmt.Fprintf(out, "%s [y/N] ", question)
	ans, err := readLine(stdin, 64)
	if err != nil {
		fmt.Fprintln(out)
		return false
	}
	ans = strings.ToLower(strings.TrimSpace(ans))
	return ans == "y" || ans == "yes"
}
