package core

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
)

const proxyInstallation = "hub: { url: https://hub.example.test, installation: test-net, key: AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=, proxy_url: \"%s\", no_proxy: \"crm.corp.test, .lan\" }\ncrm: { type: demo }\n"

// A club behind a corporate proxy: hub.proxy_url (or HTTPS_PROXY from
// .env) routes the hub, the CRM and the release checks, except no_proxy.
func TestProxyFromConfigAndEnv(t *testing.T) {
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "via proxy to "+r.URL.String())
	}))
	defer proxy.Close()
	s, err := Load([]byte(strings.Replace(proxyInstallation, "%s", proxy.URL, 1)), env(goodEnv))
	if err != nil {
		t.Fatal(err)
	}
	route := func(p ProxyFunc, rawURL string) string {
		u, _ := url.Parse(rawURL)
		got, _ := p(&http.Request{URL: u})
		if got == nil {
			return "direct"
		}
		return got.String()
	}
	if route(s.Hub.Proxy, "https://hub.example.test/tunnel/poll") != proxy.URL || route(s.Hub.Proxy, "https://crm.corp.test/") != "direct" ||
		route(s.Hub.Proxy, "https://a.b.lan/") != "direct" || route(s.Hub.Proxy, "https://evil-crm.corp.test.example/") != proxy.URL {
		t.Fatal("proxy routing")
	}
	if s.Driver.Proxy == nil {
		t.Fatal("the CRM client is not given the proxy")
	}
	// the transport really goes through it
	resp, err := (&http.Client{Transport: s.Hub.Proxy.Transport()}).Get("http://hub.example.test/health/live")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "via proxy to http://hub.example.test/health/live" {
		t.Fatalf("answer %q", body)
	}

	// from .env when gateway.yaml has none
	e := map[string]string{"HTTPS_PROXY": "http://proxy.corp.test:3128"}
	for k, v := range goodEnv {
		e[k] = v
	}
	s, err = Load([]byte("hub: { url: https://hub.example.test, installation: test-net, key: AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA= }\ncrm: { type: demo }\n"), env(e))
	if err != nil || route(s.Hub.Proxy, "https://hub.example.test/") != "http://proxy.corp.test:3128" {
		t.Fatalf("HTTPS_PROXY: %v", err)
	}
	if _, err := Load([]byte(strings.Replace(proxyInstallation, "%s", "socks5://proxy:1080", 1)), env(goodEnv)); err == nil || !strings.Contains(err.Error(), "hub.proxy_url") {
		t.Fatalf("bad proxy: %v", err)
	}
}

// A proxy URL with a login: the password is redacted from logs, and a
// config error shows the URL without it, also when the URL does not parse.
func TestProxyCredentialsNeverShown(t *testing.T) {
	const pw = "Proxy-Pa55-x7"
	s, err := Load([]byte(strings.Replace(proxyInstallation, "%s", "http://corp-user:"+pw+"@proxy.corp.test:3128", 1)), env(goodEnv))
	if err != nil || !slices.Contains(s.Secrets, pw) {
		t.Fatalf("proxy password not among the redacted secrets: %v", err)
	}
	for _, raw := range []string{"socks5://corp-user:" + pw + "@proxy:1080", "http://corp-user:" + pw + "%zz@proxy:3128", "http://corp-user:" + pw + "@"} {
		_, err := Load([]byte(strings.Replace(proxyInstallation, "%s", raw, 1)), env(goodEnv))
		if err == nil || !strings.Contains(err.Error(), "hub.proxy_url") || strings.Contains(err.Error(), pw) || strings.Contains(err.Error(), "corp-user") {
			t.Fatalf("%s: error %v", raw, err)
		}
	}
}
