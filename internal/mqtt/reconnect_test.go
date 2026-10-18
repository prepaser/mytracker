package mqtt

import (
	"context"
	"net"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eclipse/paho.mqtt.golang/packets"
	"github.com/prepaser/mytracker/internal/config"
	"github.com/prepaser/mytracker/internal/store"
)

func testBroker(t *testing.T) (*net.TCPListener, *config.Manager) {
	t.Helper()
	l, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	if err := l.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.MQTTEnabled, cfg.MQTTBroker = true, "tcp://"+l.Addr().String()
	return l, config.NewManager(cfg, nil)
}

func acceptSubscription(t *testing.T, l *net.TCPListener, code byte) net.Conn {
	return acceptSubscriptionTopic(t, l, code, "owntracks/#")
}

func acceptSubscriptionTopic(t *testing.T, l *net.TCPListener, code byte, expectedTopic string) net.Conn {
	t.Helper()
	c, err := l.Accept()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if err := c.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	p, err := packets.ReadPacket(c)
	connect, ok := p.(*packets.ConnectPacket)
	if err != nil || !ok {
		t.Fatalf("CONNECT: %T %v", p, err)
	}
	if connect.CleanSession {
		t.Fatal("CONNECT requested a clean session")
	}
	if err := packets.NewControlPacket(packets.Connack).Write(c); err != nil {
		t.Fatal(err)
	}
	p, err = packets.ReadPacket(c)
	sub, ok := p.(*packets.SubscribePacket)
	if err != nil || !ok || len(sub.Topics) != 1 || sub.Topics[0] != expectedTopic {
		t.Fatalf("SUBSCRIBE: %v %v", p, err)
	}
	ack := packets.NewControlPacket(packets.Suback).(*packets.SubackPacket)
	ack.MessageID, ack.ReturnCodes = sub.MessageID, []byte{code}
	if err := ack.Write(c); err != nil {
		t.Fatal(err)
	}
	return c
}

func acceptSubscriptionTopicWithPending(t *testing.T, l *net.TCPListener, code byte, expectedTopic string, id uint16) (net.Conn, bool) {
	t.Helper()
	c, err := l.Accept()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if err := c.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	p, err := packets.ReadPacket(c)
	connect, ok := p.(*packets.ConnectPacket)
	if err != nil || !ok {
		t.Fatalf("CONNECT: %T %v", p, err)
	}
	if connect.CleanSession {
		t.Fatal("CONNECT requested a clean session")
	}
	if err := packets.NewControlPacket(packets.Connack).Write(c); err != nil {
		t.Fatal(err)
	}
	publishQoS1(t, c, id, true)
	p, err = packets.ReadPacket(c)
	acked := false
	if puback, ok := p.(*packets.PubackPacket); ok {
		if puback.MessageID != id {
			t.Fatalf("PUBACK: id=%v want=%v", puback.MessageID, id)
		}
		acked = true
		p, err = packets.ReadPacket(c)
	}
	sub, ok := p.(*packets.SubscribePacket)
	if err != nil || !ok || len(sub.Topics) != 1 || sub.Topics[0] != expectedTopic {
		t.Fatalf("SUBSCRIBE: %v %v", p, err)
	}
	ack := packets.NewControlPacket(packets.Suback).(*packets.SubackPacket)
	ack.MessageID, ack.ReturnCodes = sub.MessageID, []byte{code}
	if err := ack.Write(c); err != nil {
		t.Fatal(err)
	}
	return c, acked
}

func testBrokerWithStore(t *testing.T) (*net.TCPListener, *config.Manager) {
	t.Helper()
	l, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	if err := l.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.MQTTEnabled, cfg.MQTTBroker = true, "tcp://"+l.Addr().String()
	db, err := store.Open(filepath.Join(t.TempDir(), "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return l, config.NewManager(cfg, db)
}

func publishQoS1(t *testing.T, c net.Conn, id uint16, duplicate bool) {
	t.Helper()
	msg := packets.NewControlPacket(packets.Publish).(*packets.PublishPacket)
	msg.TopicName = "owntracks/alice/phone"
	msg.Payload = []byte(`{"_type":"location"}`)
	msg.Qos, msg.MessageID, msg.Dup = 1, id, duplicate
	if err := msg.Write(c); err != nil {
		t.Fatal(err)
	}
}

func readPuback(t *testing.T, c net.Conn, id uint16) {
	t.Helper()
	p, err := packets.ReadPacket(c)
	ack, ok := p.(*packets.PubackPacket)
	if err != nil || !ok || ack.MessageID != id {
		t.Fatalf("PUBACK: %T id=%v err=%v", p, ackID(ack), err)
	}
}

func ackID(p *packets.PubackPacket) uint16 {
	if p == nil {
		return 0
	}
	return p.MessageID
}

func TestRunResubscribesAfterBrokerDisconnect(t *testing.T) {
	l, mgr := testBroker(t)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	received := make(chan string, 2)
	go func() {
		defer close(done)
		Run(ctx, mgr, func(user, device string, body []byte) IngestResult {
			received <- user + "/" + device + ":" + string(body)
			return IngestStored
		})
	}()
	t.Cleanup(func() { cancel(); <-done })
	for range 2 {
		conn := acceptSubscription(t, l, 1)
		msg := packets.NewControlPacket(packets.Publish).(*packets.PublishPacket)
		msg.TopicName, msg.Payload = "owntracks/alice/phone", []byte("location")
		if err := msg.Write(conn); err != nil {
			t.Fatal(err)
		}
		select {
		case got := <-received:
			if got != "alice/phone:location" {
				t.Fatalf("message=%q", got)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("subscribed connection did not receive message")
		}
		_ = conn.Close()
	}
}

func TestRunAcknowledgesTerminalMessages(t *testing.T) {
	l, mgr := testBroker(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	received := make(chan struct{}, 2)
	var calls atomic.Int32
	done := make(chan struct{})
	go func() {
		defer close(done)
		Run(ctx, mgr, func(string, string, []byte) IngestResult {
			received <- struct{}{}
			if calls.Add(1) == 1 {
				return IngestStored
			}
			return IngestInvalid
		})
	}()
	conn := acceptSubscription(t, l, 1)
	for id := uint16(1); id <= 2; id++ {
		publishQoS1(t, conn, id, false)
		select {
		case <-received:
		case <-time.After(time.Second):
			t.Fatal("publish did not reach ingest")
		}
		readPuback(t, conn, id)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("client did not stop")
	}
}

func TestRunUnsubscribesPreviousTopicOnReload(t *testing.T) {
	l, mgr := testBrokerWithStore(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		Run(ctx, mgr, func(string, string, []byte) IngestResult { return IngestInvalid })
	}()
	conn := acceptSubscription(t, l, 1)
	if _, err := mgr.Update(func(next *config.Settings) error {
		next.MQTTTopic = "custom/#"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	p, err := packets.ReadPacket(conn)
	unsub, ok := p.(*packets.UnsubscribePacket)
	if err != nil || !ok || len(unsub.Topics) != 1 || unsub.Topics[0] != "owntracks/#" {
		t.Fatalf("UNSUBSCRIBE: %T %v", p, err)
	}
	ack := packets.NewControlPacket(packets.Unsuback).(*packets.UnsubackPacket)
	ack.MessageID = unsub.MessageID
	if err := ack.Write(conn); err != nil {
		t.Fatal(err)
	}
	p, err = packets.ReadPacket(conn)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := p.(*packets.DisconnectPacket); !ok {
		t.Fatalf("expected DISCONNECT, got %T", p)
	}
	_ = conn.Close()
	_ = acceptSubscriptionTopic(t, l, 1, "custom/#")
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("client did not stop")
	}
}

func TestRunRedeliversUnacknowledgedMessageAfterReconnect(t *testing.T) {
	l, mgr := testBroker(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	received := make(chan struct{}, 2)
	var calls atomic.Int32
	done := make(chan struct{})
	go func() {
		defer close(done)
		Run(ctx, mgr, func(string, string, []byte) IngestResult {
			if calls.Add(1) == 1 {
				received <- struct{}{}
				return IngestRetry
			}
			received <- struct{}{}
			return IngestStored
		})
	}()
	conn := acceptSubscription(t, l, 1)
	publishQoS1(t, conn, 1, false)
	select {
	case <-received:
	case <-time.After(time.Second):
		t.Fatal("first publish did not reach ingest")
	}
	if err := conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if packet, err := packets.ReadPacket(conn); err == nil {
		if _, ok := packet.(*packets.PubackPacket); ok {
			t.Fatal("transient ingest failure was acknowledged")
		}
		if _, ok := packet.(*packets.DisconnectPacket); !ok {
			t.Fatalf("unexpected packet after transient failure: %T", packet)
		}
	}
	_ = conn.Close()

	var acked bool
	conn, acked = acceptSubscriptionTopicWithPending(t, l, 1, "owntracks/#", 1)
	select {
	case <-received:
	case <-time.After(7 * time.Second):
		t.Fatal("unacknowledged publish was not redelivered")
	}
	if !acked {
		readPuback(t, conn, 1)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("client did not stop")
	}
}

func TestBrokerRejectsIncompleteTopic(t *testing.T) {
	l, mgr := testBroker(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	received := make(chan string, 1)
	done := make(chan error, 1)
	go func() {
		done <- connect(ctx, mgr, mgr.Snapshot(), func(user, device string, _ []byte) IngestResult {
			received <- user + "/" + device
			return IngestInvalid
		})
	}()
	conn := acceptSubscription(t, l, 1)
	for _, topic := range []string{"owntracks/alice", "owntracks//phone", "owntracks/alice/"} {
		msg := packets.NewControlPacket(packets.Publish).(*packets.PublishPacket)
		msg.TopicName, msg.Payload = topic, []byte(`{"_type":"location"}`)
		if err := msg.Write(conn); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case got := <-received:
		t.Fatalf("incomplete topic reached ingest: %q", got)
	case <-time.After(100 * time.Millisecond):
	}
	msg := packets.NewControlPacket(packets.Publish).(*packets.PublishPacket)
	msg.TopicName, msg.Payload = "owntracks/alice/phone/event", []byte(`{"_type":"location"}`)
	if err := msg.Write(conn); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-received:
		if got != "alice/phone" {
			t.Fatalf("message=%q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("complete topic did not reach ingest")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("connect did not stop")
	}
}

func TestConnectRejectsSUBACKFailure(t *testing.T) {
	l, mgr := testBroker(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	ready := make(chan bool, 2)
	go func() {
		done <- connect(ctx, mgr, mgr.Snapshot(), func(string, string, []byte) IngestResult { return IngestInvalid }, func(value bool) { ready <- value })
	}()
	_ = acceptSubscription(t, l, 0x80)
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "subscription rejected") {
			t.Fatalf("SUBACK rejection: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("rejected subscription did not stop client")
	}
	if Connected() {
		t.Fatal("rejected subscription reported connected")
	}
	select {
	case value := <-ready:
		if value {
			t.Fatal("connected event emitted before subscription accepted")
		}
	default:
	}
}
