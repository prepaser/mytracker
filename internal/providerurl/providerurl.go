package providerurl

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

func Normalize(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("URL is empty")
	}
	raw = normalizeAuthorityEscapes(raw)
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	if u.Opaque != "" || u.User != nil || u.Host == "" {
		return "", fmt.Errorf("URL must have an authority without credentials")
	}
	u.Scheme = strings.ToLower(u.Scheme)
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("scheme must be http or https")
	}
	if u.RawQuery != "" || u.ForceQuery {
		return "", fmt.Errorf("query parameters are not allowed")
	}
	host := strings.ToLower(u.Hostname())
	if host == "" || strings.Contains(host, "%") {
		return "", fmt.Errorf("host is invalid")
	}
	if !strings.Contains(host, ":") {
		host = strings.TrimSuffix(host, ".")
		if host == "" {
			return "", fmt.Errorf("host is invalid")
		}
	} else if ip := net.ParseIP(host); ip != nil {
		host = ip.String()
	}
	port := u.Port()
	if port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return "", fmt.Errorf("invalid port")
		}
		if (u.Scheme == "http" && n == 80) || (u.Scheme == "https" && n == 443) {
			port = ""
		} else {
			port = strconv.Itoa(n)
		}
	}
	if port == "" {
		if strings.Contains(host, ":") {
			u.Host = "[" + host + "]"
		} else {
			u.Host = host
		}
	} else {
		u.Host = net.JoinHostPort(host, port)
	}
	u.Fragment = ""
	if err := normalizePath(u); err != nil {
		return "", err
	}
	return strings.TrimRight(u.String(), "/"), nil
}

func NormalizeLoopbackHTTP(raw string) (string, error) {
	clean, err := Normalize(raw)
	if err != nil {
		return "", err
	}
	u, err := url.Parse(clean)
	if err != nil {
		return "", err
	}
	if u.Scheme == "http" && !isLoopbackHost(u.Hostname()) {
		return "", fmt.Errorf("HTTP is allowed only for loopback providers")
	}
	return clean, nil
}

func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func normalizePath(u *url.URL) error {
	escaped := normalizeEscapes(u.EscapedPath())
	escaped = removeDotSegments(escaped)
	escaped = strings.TrimRight(escaped, "/")
	if escaped == "" {
		u.Path, u.RawPath = "", ""
		return nil
	}
	path, err := url.PathUnescape(escaped)
	if err != nil {
		return err
	}
	u.Path, u.RawPath = path, escaped
	return nil
}

func removeDotSegments(input string) string {
	output := ""
	for input != "" {
		switch {
		case strings.HasPrefix(input, "../"):
			input = input[3:]
		case strings.HasPrefix(input, "./"):
			input = input[2:]
		case strings.HasPrefix(input, "/./"):
			input = input[2:]
		case input == "/.":
			input = "/"
		case strings.HasPrefix(input, "/../"):
			input = input[3:]
			output = removeLastSegment(output)
		case input == "/..":
			input = "/"
			output = removeLastSegment(output)
		case input == "." || input == "..":
			input = ""
		default:
			end := strings.IndexByte(input[1:], '/')
			if end < 0 {
				end = len(input)
			} else {
				end++
			}
			output += input[:end]
			input = input[end:]
		}
	}
	return output
}

func removeLastSegment(path string) string {
	if before, ok := strings.CutSuffix(path, "/"); ok {
		return before
	}
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		return path[:i]
	}
	return ""
}

func normalizeAuthorityEscapes(raw string) string {
	start := strings.Index(raw, "://")
	if start < 0 {
		return raw
	}
	start += 3
	end := len(raw)
	if i := strings.IndexAny(raw[start:], "/?#"); i >= 0 {
		end = start + i
	}
	return raw[:start] + normalizeEscapes(raw[start:end]) + raw[end:]
}

func normalizeEscapes(raw string) string {
	const hex = "0123456789abcdefABCDEF"
	var out strings.Builder
	for i := 0; i < len(raw); i++ {
		if raw[i] != '%' || i+2 >= len(raw) || !strings.ContainsRune(hex, rune(raw[i+1])) || !strings.ContainsRune(hex, rune(raw[i+2])) {
			out.WriteByte(raw[i])
			continue
		}
		value, _ := strconv.ParseUint(raw[i+1:i+3], 16, 8)
		if isUnreserved(byte(value)) {
			out.WriteByte(byte(value))
		} else {
			out.WriteByte('%')
			out.WriteString(strings.ToUpper(raw[i+1 : i+3]))
		}
		i += 2
	}
	return out.String()
}

func isUnreserved(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("-._~", rune(c))
}
