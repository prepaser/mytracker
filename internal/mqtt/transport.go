package mqtt

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/url"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"
	"golang.org/x/net/proxy"
)

const mqttMaxRemainingLength = (1 << 20) + 65535 + 4

var (
	errMQTTFrameTooLarge         = errors.New("mqtt frame too large")
	errMQTTMalformedRemainingLen = errors.New("mqtt remaining length exceeds four bytes")
)

type mqttFrameConn struct {
	net.Conn
	maxRemainingLength uint32
	remainingLength    uint32
	length             uint32
	multiplier         uint32
	lengthBytes        uint8
	inHeader           bool
	err                error
}

func openMQTTConnection(uri *url.URL, options paho.ClientOptions) (net.Conn, error) {
	conn, err := dialMQTTConnection(uri, options)
	if err != nil {
		return nil, err
	}
	return &mqttFrameConn{Conn: conn, maxRemainingLength: mqttMaxRemainingLength}, nil
}

func dialMQTTConnection(uri *url.URL, options paho.ClientOptions) (net.Conn, error) {
	timeout := options.ConnectTimeout
	dialer := options.Dialer
	if dialer == nil {
		dialer = &net.Dialer{Timeout: 30 * time.Second}
	}
	ctx := context.Background()
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	switch uri.Scheme {
	case "ws", "wss":
		dialURI := *uri
		dialURI.User = nil
		tlsCfg := options.TLSConfig
		if uri.Scheme == "wss" {
			tlsCfg = tlsConfigForHost(tlsCfg, uri)
		}
		return paho.NewWebsocket(dialURI.String(), tlsCfg, timeout, options.HTTPHeaders, options.WebsocketOptions)
	case "mqtt", "tcp":
		return dialProxyContext(ctx, proxy.FromEnvironmentUsing(dialer), "tcp", uri.Host)
	case "unix":
		if uri.Host != "" {
			return dialer.DialContext(ctx, "unix", uri.Host)
		}
		return dialer.DialContext(ctx, "unix", uri.Path)
	case "ssl", "tls", "mqtts", "mqtt+ssl", "tcps":
		tlsCfg := tlsConfigForHost(options.TLSConfig, uri)
		conn, err := dialProxyContext(ctx, proxy.FromEnvironmentUsing(dialer), "tcp", uri.Host)
		if err != nil {
			return nil, err
		}
		return handshakeTLSContext(ctx, conn, tlsCfg)
	default:
		return nil, errors.New("unknown protocol")
	}
}

func dialProxyContext(ctx context.Context, dialer proxy.Dialer, network, address string) (net.Conn, error) {
	contextDialer, ok := dialer.(proxy.ContextDialer)
	if !ok {
		return nil, errors.New("mqtt proxy dialer does not support context cancellation")
	}
	conn, err := contextDialer.DialContext(ctx, network, address)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return conn, nil
}

func handshakeTLSContext(ctx context.Context, conn net.Conn, cfg *tls.Config) (net.Conn, error) {
	tlsConn := tls.Client(conn, cfg)
	if deadline, ok := ctx.Deadline(); ok {
		if err := tlsConn.SetDeadline(deadline); err != nil {
			_ = conn.Close()
			return nil, err
		}
	}
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		_ = tlsConn.Close()
		return nil, err
	}
	if _, ok := ctx.Deadline(); ok {
		if err := tlsConn.SetDeadline(time.Time{}); err != nil {
			_ = tlsConn.Close()
			return nil, err
		}
	}
	return tlsConn, nil
}

func tlsConfigForHost(cfg *tls.Config, uri *url.URL) *tls.Config {
	if cfg == nil {
		cfg = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	clone := cfg.Clone()
	if clone.ServerName == "" {
		clone.ServerName = uri.Hostname()
	}
	return clone
}

func (c *mqttFrameConn) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if c.err != nil {
		return 0, c.err
	}
	if c.remainingLength > 0 {
		if len(p) > int(c.remainingLength) {
			p = p[:int(c.remainingLength)]
		}
		n, err := c.Conn.Read(p)
		if n > 0 {
			c.remainingLength -= uint32(n)
		}
		return n, err
	}

	if !c.inHeader {
		b, err := c.readByte()
		if err != nil {
			return 0, err
		}
		p[0] = b
		c.inHeader = true
		c.length = 0
		c.multiplier = 1
		c.lengthBytes = 0
		return 1, nil
	}

	b, err := c.readByte()
	if err != nil {
		return 0, err
	}
	c.lengthBytes++
	if c.lengthBytes == 4 && b&0x80 != 0 {
		c.err = errMQTTMalformedRemainingLen
		return 0, c.err
	}
	c.length += uint32(b&0x7f) * c.multiplier
	if c.length > c.maxRemainingLength {
		c.err = errMQTTFrameTooLarge
		return 0, c.err
	}
	c.multiplier *= 128
	if b&0x80 == 0 {
		c.remainingLength = c.length
		c.inHeader = false
	}
	p[0] = b
	return 1, nil
}

func (c *mqttFrameConn) readByte() (byte, error) {
	var b [1]byte
	n, err := c.Conn.Read(b[:])
	if n == 1 {
		return b[0], nil
	}
	if n == 0 && err == nil {
		return 0, errors.New("mqtt connection returned no data")
	}
	return 0, err
}
