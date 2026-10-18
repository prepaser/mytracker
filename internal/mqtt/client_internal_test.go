package mqtt

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"
	"github.com/prepaser/mytracker/internal/config"
	"github.com/prepaser/mytracker/internal/store"
)

type fakeToken struct {
	done chan struct{}
	err  error
}

func (t *fakeToken) Wait() bool { <-t.done; return true }
func (t *fakeToken) WaitTimeout(d time.Duration) bool {
	select {
	case <-t.done:
		return true
	case <-time.After(d):
		return false
	}
}
func (t *fakeToken) Done() <-chan struct{} { return t.done }
func (t *fakeToken) Error() error          { return t.err }

func TestParseTopicAndWait(t *testing.T) {
	for _, tc := range []struct {
		topic        string
		user, device string
	}{
		{topic: "owntracks/alice/phone", user: "alice", device: "phone"},
		{topic: "customprefix/alice/phone/event", user: "alice", device: "phone"},
		{topic: "owntracks/alice", user: "", device: ""},
		{topic: "owntracks//phone", user: "", device: ""},
		{topic: "owntracks/alice/", user: "", device: ""},
	} {
		user, device := parseTopic(tc.topic)
		if user != tc.user || device != tc.device {
			t.Errorf("parseTopic(%q) = %q/%q, want %q/%q", tc.topic, user, device, tc.user, tc.device)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if wait(ctx, time.Hour) {
		t.Fatal("canceled wait completed")
	}
	if !wait(context.Background(), time.Millisecond) {
		t.Fatal("timer did not complete")
	}
}

func TestMatchesTopic(t *testing.T) {
	for _, tc := range []struct {
		filter, topic string
		want          bool
	}{
		{filter: "owntracks/#", topic: "owntracks/alice/phone", want: true},
		{filter: "owntracks/+/phone", topic: "owntracks/alice/phone", want: true},
		{filter: "owntracks/+/phone", topic: "owntracks/alice/tablet", want: false},
		{filter: "owntracks/#", topic: "custom/alice/phone", want: false},
		{filter: "$share/trackers/owntracks/#", topic: "owntracks/alice/phone", want: true},
		{filter: "$share/trackers/owntracks/+/phone", topic: "owntracks/alice/phone", want: true},
		{filter: "$share/trackers/owntracks/#", topic: "custom/alice/phone", want: false},
		{filter: "$share/trackers/owntracks/+/phone", topic: "owntracks/alice/tablet", want: false},
		{filter: "$share//owntracks/#", topic: "owntracks/alice/phone", want: false},
		{filter: "$share/+/owntracks/#", topic: "owntracks/alice/phone", want: false},
		{filter: "$share/#/owntracks/#", topic: "owntracks/alice/phone", want: false},
		{filter: "$share/trackers", topic: "owntracks/alice/phone", want: false},
		{filter: "$share/trackers/", topic: "owntracks/alice/phone", want: false},
	} {
		t.Run(tc.filter+"/"+tc.topic, func(t *testing.T) {
			if got := matchesTopic(tc.filter, tc.topic); got != tc.want {
				t.Fatalf("matchesTopic(%q, %q)=%t, want %t", tc.filter, tc.topic, got, tc.want)
			}
		})
	}
}

func TestPendingSubscriptionDelivery(t *testing.T) {
	for _, tc := range []struct {
		filter string
		stored bool
	}{
		{filter: "owntracks/#", stored: true},
		{filter: "$share/trackers/owntracks/#", stored: true},
		{filter: "$share/trackers/custom/#", stored: false},
	} {
		t.Run(tc.filter, func(t *testing.T) {
			cfg := config.Default()
			cfg.MQTTEnabled = true
			cfg.MQTTBroker, cfg.MQTTTopic = "tcp://broker:1883", tc.filter
			mgr := config.NewManager(cfg, nil)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			client := &fakeClient{disconnected: make(chan struct{})}
			var acked atomic.Bool
			stored := false
			body := []byte(`{"_type":"location"}`)
			_ = connectWithFactory(ctx, mgr, cfg, func(user, device string, payload []byte) IngestResult {
				if user != "alice" || device != "phone" || string(payload) != string(body) {
					t.Fatalf("unexpected pending delivery: %q/%q %s", user, device, payload)
				}
				stored = true
				return IngestStored
			}, func(opts *paho.ClientOptions) paho.Client {
				opts.DefaultPublishHandler(client, fakeMessage{topic: "owntracks/alice/phone", payload: body, acked: &acked})
				cancel()
				return client
			})
			if stored != tc.stored || !acked.Load() {
				t.Fatalf("stored=%t want=%t acked=%t", stored, tc.stored, acked.Load())
			}
		})
	}
}

func TestWatchConfigCancelsOnChange(t *testing.T) {
	cfg := config.Default()
	st, err := store.Open(filepath.Join(t.TempDir(), "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	mgr := config.NewManager(cfg, st)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		watchConfig(ctx, mgr, cfg.MQTTFingerprint(), cancel)
		close(done)
	}()
	if _, err := mgr.Update(func(next *config.Settings) error {
		next.MQTTEnabled = true
		next.MQTTBroker = "tcp://localhost:1883"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("config watcher did not stop")
	}
}

func TestConnectRejectsInvalidCA(t *testing.T) {
	cfg := config.Default()
	cfg.MQTTEnabled = true
	cfg.MQTTBroker = "mqtts://localhost:8883"
	cfg.MQTTCAFile = filepath.Join(t.TempDir(), "missing.pem")
	mgr := config.NewManager(cfg, nil)
	for _, broker := range []string{"mqtts://localhost:8883", "wss://localhost:8883", "tcps://localhost:8883", "mqtt+ssl://localhost:8883"} {
		cfg.MQTTBroker = broker
		err := connect(context.Background(), mgr, cfg, func(string, string, []byte) IngestResult { return IngestDropped })
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s did not load configured CA: %v", broker, err)
		}
	}
	badCA := filepath.Join(t.TempDir(), "bad.pem")
	if err := os.WriteFile(badCA, []byte("not pem"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg.MQTTCAFile = badCA
	if err := connect(t.Context(), mgr, cfg, func(string, string, []byte) IngestResult { return IngestDropped }); err == nil {
		t.Fatal("invalid CA accepted")
	}
}

func TestNotifyState(t *testing.T) {
	notifyState(nil, true)
	notifyState([]func(bool){nil}, true)
	called := false
	notifyState([]func(bool){func(value bool) { called = value }}, true)
	if !called {
		t.Fatal("state callback not called")
	}
}

func TestWaitTokenHonorsContextAndResult(t *testing.T) {
	pending := &fakeToken{done: make(chan struct{})}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := waitToken(ctx, pending, time.Hour, "subscribe"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel err=%v", err)
	}
	want := errors.New("subscribe failed")
	completed := &fakeToken{done: make(chan struct{}), err: want}
	close(completed.done)
	if err := waitToken(t.Context(), completed, time.Second, "subscribe"); !errors.Is(err, want) {
		t.Fatalf("result err=%v", err)
	}
}

func TestSetConnectedReportsTransitions(t *testing.T) {
	connected.Store(false)
	var values []bool
	callback := []func(bool){func(value bool) { values = append(values, value) }}
	setConnected(true, callback)
	setConnected(true, callback)
	setConnected(false, callback)
	if len(values) != 2 || !values[0] || values[1] {
		t.Fatalf("values=%v", values)
	}
}

func TestConnectRejectsInvalidBroker(t *testing.T) {
	cfg := config.Default()
	cfg.MQTTEnabled = true
	cfg.MQTTBroker = "invalid://broker"
	mgr := config.NewManager(cfg, nil)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := connect(ctx, mgr, cfg, func(string, string, []byte) IngestResult { return IngestDropped }); err == nil {
		t.Fatal("invalid broker accepted")
	}
}

type fakeClient struct {
	paho.Client
	mu           sync.Mutex
	handler      paho.MessageHandler
	disconnected chan struct{}
	disconnect   sync.Once
}

func (c *fakeClient) Connect() paho.Token {
	return completedToken()
}

func (c *fakeClient) Subscribe(topic string, _ byte, handler paho.MessageHandler) paho.Token {
	c.mu.Lock()
	c.handler = handler
	c.mu.Unlock()
	return &fakeSubscribeToken{Token: completedToken(), results: map[string]byte{topic: 1}}
}

type fakeSubscribeToken struct {
	paho.Token
	results map[string]byte
}

func (t *fakeSubscribeToken) Result() map[string]byte { return t.results }

func (c *fakeClient) Disconnect(_ uint) {
	c.disconnect.Do(func() {
		close(c.disconnected)
	})
}

func (c *fakeClient) messageHandler() paho.MessageHandler {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.handler
}

func completedToken() paho.Token {
	t := &fakeToken{done: make(chan struct{})}
	close(t.done)
	return t
}

type fakeMessage struct {
	paho.Message
	topic   string
	payload []byte
	acked   *atomic.Bool
}

func (m fakeMessage) Topic() string   { return m.topic }
func (m fakeMessage) Payload() []byte { return m.payload }
func (m fakeMessage) Ack() {
	if m.acked != nil {
		m.acked.Store(true)
	}
}

func TestConnectDrainsIngestCallbackBeforeReturn(t *testing.T) {
	cfg := config.Default()
	cfg.MQTTEnabled = true
	cfg.MQTTBroker = "tcp://broker:1883"
	mgr := config.NewManager(cfg, nil)
	client := &fakeClient{disconnected: make(chan struct{})}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	done := make(chan error, 1)
	go func() {
		done <- connectWithFactory(ctx, mgr, cfg, func(string, string, []byte) IngestResult {
			calls.Add(1)
			close(started)
			<-release
			return IngestStored
		}, func(*paho.ClientOptions) paho.Client { return client })
	}()

	var handler paho.MessageHandler
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	for handler == nil {
		select {
		case <-deadline.C:
			t.Fatal("subscribe handler was not installed")
		default:
			handler = client.messageHandler()
			time.Sleep(time.Millisecond)
		}
	}
	go handler(client, fakeMessage{topic: "owntracks/alice/phone", payload: []byte(`{"_type":"location"}`)})
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("ingest callback did not start")
	}
	cancel()
	select {
	case err := <-done:
		t.Fatalf("connect returned before callback drained: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	select {
	case <-client.disconnected:
	case <-time.After(time.Second):
		t.Fatal("client was not disconnected")
	}
	close(release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("connect did not return after callback drained")
	}
	if calls.Load() != 1 {
		t.Fatalf("calls=%d, want 1", calls.Load())
	}
	handler(client, fakeMessage{topic: "owntracks/alice/phone", payload: []byte(`{"_type":"location"}`)})
	if calls.Load() != 1 {
		t.Fatalf("late callback was accepted, calls=%d", calls.Load())
	}
}

func TestCallbackGateConcurrentStop(t *testing.T) {
	gate := newCallbackGate(t.Context(), func(string, string, []byte) IngestResult { return IngestDropped })
	start := make(chan struct{})
	var callbacks sync.WaitGroup
	for range 64 {
		callbacks.Go(func() {
			<-start
			gate.handle(nil, fakeMessage{topic: "owntracks/u/d"})
		})
	}
	close(start)
	gate.stop()
	gate.wait()
	callbacks.Wait()
	gate.handle(nil, fakeMessage{topic: "owntracks/u/d"})
}

func TestCallbackGateRejectsIncompleteTopic(t *testing.T) {
	var calls atomic.Int32
	gate := newCallbackGate(t.Context(), func(string, string, []byte) IngestResult {
		calls.Add(1)
		return IngestDropped
	})
	for _, topic := range []string{"owntracks/alice", "owntracks//phone", "owntracks/alice/"} {
		gate.handle(nil, fakeMessage{topic: topic})
	}
	gate.handle(nil, fakeMessage{topic: "customprefix/alice/phone/event"})
	if calls.Load() != 1 {
		t.Fatalf("calls=%d, want 1", calls.Load())
	}
}

func TestCallbackGateAcknowledgesOnlyTerminalResults(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result IngestResult
		ack    bool
	}{
		{name: "stored", result: IngestStored, ack: true},
		{name: "duplicate", result: IngestDuplicate, ack: true},
		{name: "invalid", result: IngestInvalid, ack: true},
		{name: "dropped", result: IngestDropped, ack: true},
		{name: "retry", result: IngestRetry},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var acked atomic.Bool
			gate := newCallbackGate(t.Context(), func(string, string, []byte) IngestResult { return tc.result })
			gate.handle(nil, fakeMessage{topic: "owntracks/u/d", acked: &acked})
			if got := acked.Load(); got != tc.ack {
				t.Fatalf("acked=%t, want %t", got, tc.ack)
			}
		})
	}

	var acked atomic.Bool
	newCallbackGate(t.Context(), func(string, string, []byte) IngestResult {
		t.Fatal("invalid topic reached ingest")
		return IngestRetry
	}).handle(nil, fakeMessage{topic: "owntracks/u", acked: &acked})
	if !acked.Load() {
		t.Fatal("invalid topic was not acknowledged")
	}
}
