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
// # are ignored, and an optional "export " prefix is removed. Quoting
// follows docker compose for what setup writes: 'single-quoted' is literal
// except \' for a quote; "double-quoted" understands \\, \" and $$ for $.
// Values are never logged.
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
		out[k] = unquote(strings.TrimSpace(v))
	}
	return out, sc.Err()
}

func unquote(v string) string {
	if len(v) < 2 || (v[0] != '"' && v[0] != '\'') || v[len(v)-1] != v[0] {
		return v
	}
	inner := v[1 : len(v)-1]
	if v[0] == '\'' {
		return strings.ReplaceAll(inner, `\'`, "'")
	}
	return strings.NewReplacer(`\\`, `\`, `\"`, `"`, "$$", "$").Replace(inner)
}

// EnvWithDotEnv returns a getenv that prefers the process environment and
// falls back to the .env file next to cfgPath, when that file exists. The
// service managers on every OS start the gateway without a shell, so the
// gateway reads its secrets file itself. The CRM password is the
// exception: .env first, where setup and set-password write it and the
// running service re-reads it, so a CRM_PASSWORD left in the environment
// of the machine cannot make the service log in with another password than
// the one set-password checked.
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
		if v := vals[k]; k == PasswordEnv && v != "" {
			return v
		}
		if v := getenv(k); v != "" {
			return v
		}
		return vals[k]
	}, nil
}
