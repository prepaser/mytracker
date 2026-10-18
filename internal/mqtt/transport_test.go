package mqtt

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/eclipse/paho.mqtt.golang/packets"
	"golang.org/x/net/proxy"
)

type mqttReadConn struct {
	data    []byte
	pos     int
	maxRead int
}

func (c *mqttReadConn) Read(p []byte) (int, error) {
	if c.pos == len(c.data) {
		return 0, io.EOF
	}
	n := min(len(p), c.maxRead)
	if n > len(c.data)-c.pos {
		n = len(c.data) - c.pos
	}
	copy(p[:n], c.data[c.pos:c.pos+n])
	c.pos += n
	return n, nil
}

func (c *mqttReadConn) Write(p []byte) (int, error)      { return len(p), nil }
func (c *mqttReadConn) Close() error                     { return nil }
func (c *mqttReadConn) LocalAddr() net.Addr              { return mqttTestAddr("local") }
func (c *mqttReadConn) RemoteAddr() net.Addr             { return mqttTestAddr("remote") }
func (c *mqttReadConn) SetDeadline(time.Time) error      { return nil }
func (c *mqttReadConn) SetReadDeadline(time.Time) error  { return nil }
func (c *mqttReadConn) SetWriteDeadline(time.Time) error { return nil }

type mqttTestAddr string

func (a mqttTestAddr) Network() string { return "test" }
func (a mqttTestAddr) String() string  { return string(a) }

type blockingProxyDialer struct{}

func (blockingProxyDialer) Dial(string, string) (net.Conn, error) {
	return nil, errors.New("unexpected non-context dial")
}

func (blockingProxyDialer) DialContext(ctx context.Context, _, _ string) (net.Conn, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

var _ proxy.ContextDialer = blockingProxyDialer{}

func TestDialProxyContextHonorsTimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := dialProxyContext(ctx, blockingProxyDialer{}, "tcp", "broker:8883")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("dial error=%v, want deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("dial took %s after context deadline", elapsed)
	}
}

func TestTLSHandshakeHonorsTimeout(t *testing.T) {
	client, server := net.Pipe()
	defer func() { _ = client.Close() }()
	defer func() { _ = server.Close() }()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := handshakeTLSContext(ctx, client, &tls.Config{ServerName: "broker.test", MinVersion: tls.VersionTLS12})
	var timeout net.Error
	if !errors.Is(err, context.DeadlineExceeded) && (!errors.As(err, &timeout) || !timeout.Timeout()) {
		t.Fatalf("TLS handshake error=%v, want timeout", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("TLS handshake took %s after context deadline", elapsed)
	}
}

func TestMQTTFrameConnRejectsOversizedHeadersBeforeBody(t *testing.T) {
	for _, tc := range []struct {
		name   string
		header []byte
		err    error
		read   int
	}{
		{
			name:   "oversized",
			header: []byte{0x30, 0xff, 0xff, 0x44},
			err:    errMQTTFrameTooLarge,
			read:   4,
		},
		{
			name:   "five length bytes",
			header: []byte{0x30, 0x80, 0x80, 0x80, 0x80},
			err:    errMQTTMalformedRemainingLen,
			read:   5,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn := &mqttReadConn{data: append(tc.header, 0xde, 0xad, 0xbe), maxRead: 32}
			framed := &mqttFrameConn{Conn: conn, maxRemainingLength: mqttMaxRemainingLength}
			if _, err := packets.ReadPacket(framed); !errors.Is(err, tc.err) {
				t.Fatalf("ReadPacket error=%v, want %v", err, tc.err)
			}
			if conn.pos != tc.read {
				t.Fatalf("read %d bytes, want %d; body was consumed", conn.pos, tc.read)
			}
		})
	}
}

func TestMQTTFrameConnAllowsPayloadLimitWithMQTTOverhead(t *testing.T) {
	publish := packets.NewControlPacket(packets.Publish).(*packets.PublishPacket)
	publish.Qos = 1
	publish.MessageID = 1
	publish.TopicName = string(bytes.Repeat([]byte{'t'}, 65535))
	publish.Payload = bytes.Repeat([]byte{'p'}, 1<<20)
	var wire bytes.Buffer
	if err := publish.Write(&wire); err != nil {
		t.Fatal(err)
	}

	conn := &mqttReadConn{data: wire.Bytes(), maxRead: 3}
	framed := &mqttFrameConn{Conn: conn, maxRemainingLength: mqttMaxRemainingLength}
	packet, err := packets.ReadPacket(framed)
	if err != nil {
		t.Fatal(err)
	}
	got := packet.(*packets.PublishPacket)
	if len(got.Payload) != 1<<20 || len(got.TopicName) != 65535 {
		t.Fatalf("payload/topic lengths=%d/%d", len(got.Payload), len(got.TopicName))
	}
}

func TestMQTTFrameConnHandlesFragmentedAndCoalescedPackets(t *testing.T) {
	var wire bytes.Buffer
	for _, payload := range []string{"one", "two"} {
		publish := packets.NewControlPacket(packets.Publish).(*packets.PublishPacket)
		publish.TopicName = "owntracks/u/d"
		publish.Payload = []byte(payload)
		if err := publish.Write(&wire); err != nil {
			t.Fatal(err)
		}
	}
	conn := &mqttReadConn{data: wire.Bytes(), maxRead: 2}
	framed := &mqttFrameConn{Conn: conn, maxRemainingLength: mqttMaxRemainingLength}
	for _, want := range []string{"one", "two"} {
		packet, err := packets.ReadPacket(framed)
		if err != nil {
			t.Fatal(err)
		}
		if got := string(packet.(*packets.PublishPacket).Payload); got != want {
			t.Fatalf("payload=%q, want %q", got, want)
		}
	}
}
