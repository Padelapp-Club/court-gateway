package core

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRedaction(t *testing.T) {
	var buf bytes.Buffer
	log := NewLogger(&buf, slog.LevelDebug, []string{"hunter2-secret", "partner-key-abcdef"})
	log.Info("login with hunter2-secret failed",
		"password", "plain",
		"note", "Ivan Petrov +7 999",
		"client", map[string]string{"phone": "123"},
		"partner", "aggregator",
		"error", errors.New("upstream said partner-key-abcdef is bad"),
		slog.Group("req", "authorization", "Bearer x", "path", "/v1/clubs"),
	)
	log.With("comment", "hidden").Info("with attrs")
	out := buf.String()
	for _, leak := range []string{"hunter2-secret", "plain", "Ivan Petrov", "123", "partner-key-abcdef", "Bearer x", "hidden"} {
		if strings.Contains(out, leak) {
			t.Fatalf("leaked %q: %s", leak, out)
		}
	}
	for _, keep := range []string{"aggregator", "/v1/clubs", `"note":"[redacted]"`} {
		if !strings.Contains(out, keep) {
			t.Fatalf("lost %q: %s", keep, out)
		}
	}
}

func TestTrimStartupLog(t *testing.T) {
	p := filepath.Join(t.TempDir(), "stderr.log")
	if err := os.WriteFile(p, make([]byte, 2048), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	TrimStartupLog(f, 4096)
	if info, _ := os.Stat(p); info.Size() != 2048 {
		t.Fatalf("a small file was cut: %d", info.Size())
	}
	TrimStartupLog(f, 1024)
	if _, err := f.WriteString("next start\n"); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(p); string(data) != "next start\n" {
		t.Fatalf("not trimmed, or appending broke: %q", data)
	}
}
