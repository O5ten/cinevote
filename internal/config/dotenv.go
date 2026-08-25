package config

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// DefaultEnvFile is the file LoadEnvFile reads when nothing else is named.
const DefaultEnvFile = ".env"

// LoadEnvFile reads a .env file into the process environment, if one is there.
// A missing file is not an error: the file is a convenience for local runs and
// for a container that has one mounted, while a plain `docker run -e ...` or a
// compose `environment:` block needs nothing.
//
// Variables already set in the environment win, so an explicit -e or a compose
// setting is never overwritten by a file that happened to be lying about.
//
// The format is the common one: KEY=value per line, # for comments, optional
// "export" prefix, and single or double quotes around values that need them.
func LoadEnvFile(path string) error {
	if path == "" {
		path = DefaultEnvFile
	}
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for line := 1; scanner.Scan(); line++ {
		text := strings.TrimSpace(scanner.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		text = strings.TrimPrefix(text, "export ")

		key, value, ok := strings.Cut(text, "=")
		if !ok {
			return fmt.Errorf("%s:%d: expected KEY=value, got %q", path, line, text)
		}
		key = strings.TrimSpace(key)
		if key == "" {
			return fmt.Errorf("%s:%d: missing a name before =", path, line)
		}
		if _, taken := os.LookupEnv(key); taken {
			continue // the real environment wins
		}
		if err := os.Setenv(key, unquote(strings.TrimSpace(value))); err != nil {
			return fmt.Errorf("%s:%d: %w", path, line, err)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	return nil
}

// unquote strips one layer of matching quotes and drops a trailing comment from
// an unquoted value.
func unquote(value string) string {
	if len(value) >= 2 {
		first, last := value[0], value[len(value)-1]
		if (first == '"' && last == '"') || (first == '\'' && last == '\'') {
			return value[1 : len(value)-1]
		}
	}
	// Only an unquoted value can carry a trailing comment; a quoted one may
	// legitimately contain a #.
	if i := strings.Index(value, " #"); i >= 0 {
		value = value[:i]
	}
	return strings.TrimSpace(value)
}
