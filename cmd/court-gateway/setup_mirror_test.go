package main

import (
	"bufio"
	"bytes"
	"strings"
	"testing"

	"github.com/Padelapp-Club/court-gateway/internal/setup"
)

// The second CRM is asked as yes or no; Enter is no.
func TestAskMirror(t *testing.T) {
	for _, c := range []struct{ in, want string }{{"\n", ""}, {"да\n", "lunda"}, {"Y\n", "lunda"}, {"может\nнет\n", ""}} {
		var w bytes.Buffer
		got, err := askMirror(bufio.NewReader(strings.NewReader(c.in)), &w)
		if err != nil || got != c.want {
			t.Fatalf("%q: %q %v", c.in, got, err)
		}
		if strings.HasPrefix(c.in, "может") && !strings.Contains(w.String(), "Ответьте «да» или «нет»") {
			t.Fatalf("%q: %s", c.in, w.String())
		}
	}
}

// 1C:Fitness asks for the client of the mirrors only when there is a
// second CRM, and setup names what that CRM needs.
func TestPromptAsksTheMirrorClientOnlyWithAMirror(t *testing.T) {
	for _, mirror := range []string{"", "lunda"} {
		in := setup.Input{HubURL: "https://hub.example.test", HubKey: "k", JoinCode: "x", CRMType: "fitness1c", Mirror: mirror,
			Password: "p", Secrets: map[string]string{"FITNESS1C_APIKEY": "k"},
			CRM: map[string]string{"base_url": "https://1c.example.test", "username": "u", "client_phone": "+79990000001", "partner": "padelapp"}}
		var w bytes.Buffer
		if err := prompt(&in, bufio.NewReader(strings.NewReader("+79990000002\n")), &w); err != nil {
			t.Fatal(err)
		}
		asked := strings.Contains(w.String(), "второго технического клиента")
		if asked != (mirror != "") || (mirror != "") != strings.Contains(w.String(), "Для второй CRM (Лунда)") {
			t.Fatalf("mirror %q: %s", mirror, w.String())
		}
		if mirror != "" && in.CRM["mirror_client_phone"] != "+79990000002" {
			t.Fatalf("mirror client: %q", in.CRM["mirror_client_phone"])
		}
	}
}
