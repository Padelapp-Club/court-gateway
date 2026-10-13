package core

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// EnvFileName is the secrets file kept next to the config file.
const EnvFileName = ".env"

// ParseDotEnv parses KEY=VALUE lines. Blank lines and lines starting with
// # are ignored; an optional "export " prefix and matching single or double
// quotes around the value are removed. Values are never logged.
func ParseDotEnv(data []byte) (map[string]string, error) {
	out := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(data))
	n := 0
	for sc.Scan() {
		n++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		k = strings.TrimSpace(k)
		if !ok || !envNameRe.MatchString(k) {
			// the line number only: the line itself may hold a secret
			return nil, fmt.Errorf("%s: строка %d — ожидается ИМЯ=значение", EnvFileName, n)
		}
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		}
		out[k] = v
	}
	return out, sc.Err()
}

// EnvWithDotEnv returns a getenv that prefers the process environment and
// falls back to the .env file next to cfgPath, when that file exists. The
// service managers on every OS start the gateway without a shell, so the
// gateway reads its secrets file itself.
func EnvWithDotEnv(cfgPath string, getenv func(string) string) (func(string) string, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	p := filepath.Join(filepath.Dir(cfgPath), EnvFileName)
	data, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		return getenv, nil
	}
	if err != nil {
		return nil, fmt.Errorf("не удалось прочитать %s: %w", p, err)
	}
	vals, err := ParseDotEnv(data)
	if err != nil {
		return nil, err
	}
	return func(k string) string {
		if v := getenv(k); v != "" {
			return v
		}
		return vals[k]
	}, nil
}
