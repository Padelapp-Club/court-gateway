package core

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// ProxyFunc is how the gateway reaches the internet behind a corporate
// proxy: every outbound request (hub, CRM, release manifest) goes through
// proxyURL, except to the hosts listed in noProxy (comma-separated: a host
// name, or a domain whose subdomains it covers, with or without a leading
// dot; "*" means none). A service does not see the proxy variables of the
// person's shell, so they are read from gateway.yaml or .env.
type ProxyFunc func(*http.Request) (*url.URL, error)

// parseProxy also returns the proxy password, if any, for the log
// redaction. An error never repeats the login and password of the URL.
func parseProxy(rawURL, noProxy string) (ProxyFunc, []string, error) {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		shown := rawURL
		if err == nil {
			bare := *u
			bare.User = nil
			shown = bare.String()
		} else if at := strings.LastIndex(rawURL, "@"); at >= 0 {
			shown = rawURL[at+1:] // unparsed: everything before the host is dropped
		}
		return nil, nil, fmt.Errorf("«%s» — нужен адрес вида http://прокси:3128", shown)
	}
	var secrets []string
	if pw, ok := u.User.Password(); ok && pw != "" {
		secrets = append(secrets, pw)
		if _, raw, _ := strings.Cut(u.User.String(), ":"); raw != pw {
			secrets = append(secrets, raw) // as written, percent-encoded
		}
	}
	var skip []string
	for _, e := range strings.Split(noProxy, ",") {
		e = strings.ToLower(strings.TrimSpace(e))
		if h, _, err := net.SplitHostPort(e); err == nil {
			e = h
		}
		if e = strings.TrimPrefix(e, "."); e != "" {
			skip = append(skip, e)
		}
	}
	return func(r *http.Request) (*url.URL, error) {
		host := strings.ToLower(r.URL.Hostname())
		for _, e := range skip {
			if e == "*" || host == e || strings.HasSuffix(host, "."+e) {
				return nil, nil
			}
		}
		return u, nil
	}, secrets, nil
}

// Transport is a clone of the default transport, through p when it is set.
func (p ProxyFunc) Transport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	if p != nil {
		t.Proxy = p
	}
	return t
}
