package config

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"

	"github.com/prepaser/mytracker/internal/providerurl"
)

func (c *Config) ShareURL(token, requestHost string) string {
	return c.ShareURLForRequest(token, requestHost, false)
}

func (c *Config) ShareURLForRequest(token, requestHost string, secure bool) string {
	base := c.publicBase(requestHost, secure)
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		u = &url.URL{Scheme: "http", Host: "localhost"}
	}
	u.Path = "/share/" + token
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}

func (c *Config) PubURL(requestHost string) string {
	return c.PubURLForRequest(requestHost, false)
}

func (c *Config) PubURLForRequest(requestHost string, secure bool) string {
	base := c.publicBase(requestHost, secure)
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		u = &url.URL{Scheme: "http", Host: "localhost"}
	}
	u.User = url.UserPassword(c.PubUser, c.PubPass)
	u.Path = "/pub"
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}

func (c *Config) publicBase(requestHost string, secure bool) string {
	if c.PublicURL != "" {
		return strings.TrimRight(strings.TrimSpace(c.PublicURL), "/")
	}
	h := validRequestHost(requestHost)
	if h == "" {
		h = "localhost"
	}
	u, _ := url.Parse("//" + h)
	addr := c.ListenAddr
	if c.TLSConfigured() {
		addr = c.HTTPSListenAddr
		if !secure {
			_, port, _ := net.SplitHostPort(addr)
			h = u.Hostname()
			if port != "" && port != "443" {
				h = net.JoinHostPort(h, port)
			} else if strings.Contains(h, ":") {
				h = "[" + h + "]"
			}
			return "https://" + h
		}
	}
	if u.Port() == "" {
		if !secure && addr != "" && addr[0] == ':' {
			port := strings.TrimPrefix(addr, ":")
			scheme := c.URLScheme()
			if (scheme != "https" || port != "443") && (scheme != "http" || port != "80") {
				h = net.JoinHostPort(u.Hostname(), port)
			}
		}
	}
	scheme := c.URLScheme()
	if secure {
		scheme = "https"
	}
	return scheme + "://" + h
}

func (c *Config) URLScheme() string {
	if c.TLSConfigured() {
		return "https"
	}
	return "http"
}

func validRequestHost(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.ContainsAny(raw, `/\@#?`) {
		return ""
	}
	u, err := url.Parse("//" + raw)
	if err != nil || u.Host != raw || u.User != nil || u.Hostname() == "" {
		return ""
	}
	host := u.Hostname()
	if net.ParseIP(host) == nil && !validDNSName(host) {
		return ""
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return ""
		}
	}
	return raw
}

func validDNSName(host string) bool {
	if len(host) > 253 {
		return false
	}
	for label := range strings.SplitSeq(host, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, ch := range label {
			if (ch < 'a' || ch > 'z') && (ch < 'A' || ch > 'Z') &&
				(ch < '0' || ch > '9') && ch != '-' {
				return false
			}
		}
	}
	return true
}

func cleanHTTPURL(raw string, keepPath bool) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", err
	}
	u.Scheme = strings.ToLower(u.Scheme)
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("scheme must be http or https")
	}
	if u.Host == "" {
		return "", fmt.Errorf("host is required")
	}
	if u.User != nil {
		return "", fmt.Errorf("credentials are not allowed")
	}
	u.Fragment = ""
	if !keepPath {
		escapedPath := strings.TrimRight(u.EscapedPath(), "/")
		if escapedPath == "" {
			u.Path, u.RawPath = "", ""
		} else if escapedPath != u.EscapedPath() {
			path, err := url.PathUnescape(escapedPath)
			if err != nil {
				return "", err
			}
			u.Path, u.RawPath = path, escapedPath
		}
	}
	if u.RawQuery != "" || u.ForceQuery {
		return u.String(), nil
	}
	return strings.TrimRight(u.String(), "/"), nil
}

func cleanProviderURL(raw string) (string, error) {
	return providerurl.NormalizeLoopbackHTTP(raw)
}
