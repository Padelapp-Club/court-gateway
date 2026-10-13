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

func parseProxy(rawURL, noProxy string) (ProxyFunc, error) {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("«%s» — нужен адрес вида http://прокси:3128", rawURL)
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
	}, nil
}

// Transport is a clone of the default transport, through p when it is set.
func (p ProxyFunc) Transport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	if p != nil {
		t.Proxy = p
	}
	return t
}
