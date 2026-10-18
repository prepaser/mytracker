package mqtt

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/prepaser/mytracker/internal/config"
	"github.com/prepaser/mytracker/internal/metrics"
)

var connected atomic.Bool

const configPollInterval = 250 * time.Millisecond

var errMQTTIngestRetry = errors.New("mqtt ingest retry")

type IngestResult uint8

const (
	IngestStored IngestResult = iota
	IngestDuplicate
	IngestInvalid
	IngestDropped
	IngestRetry
)

func Run(ctx context.Context, cfg *config.Manager, ingest func(user, device string, body []byte) IngestResult, state ...func(bool)) {
	var previousTopic string
	for {
		if ctx.Err() != nil {
			return
		}
		snap := cfg.Snapshot()
		if !snap.MQTTEnabled {
			setConnected(false, state)
			if !wait(ctx, 5*time.Second) {
				return
			}
			continue
		}
		topic := mqttTopic(snap)
		err := connectWithFactoryTopic(ctx, cfg, snap, ingest, mqtt.NewClient, previousTopic, state...)
		previousTopic = topic
		if ctx.Err() != nil {
			return
		}
		if errors.Is(err, context.Canceled) {
			continue
		}
		if err != nil && !errors.Is(err, errMQTTIngestRetry) {
			slog.Error("mqtt disconnected", "err", err)
		}
		if !wait(ctx, 5*time.Second) {
			return
		}
	}
}

func connect(ctx context.Context, mgr *config.Manager, cfg *config.Config, ingest func(user, device string, body []byte) IngestResult, state ...func(bool)) error {
	return connectWithFactoryTopic(ctx, mgr, cfg, ingest, mqtt.NewClient, "", state...)
}

type clientFactory func(*mqtt.ClientOptions) mqtt.Client

func connectWithFactory(ctx context.Context, mgr *config.Manager, cfg *config.Config, ingest func(user, device string, body []byte) IngestResult, newClient clientFactory, state ...func(bool)) error {
	return connectWithFactoryTopic(ctx, mgr, cfg, ingest, newClient, "", state...)
}

func connectWithFactoryTopic(ctx context.Context, mgr *config.Manager, cfg *config.Config, ingest func(user, device string, body []byte) IngestResult, newClient clientFactory, previousTopic string, state ...func(bool)) error {
	fp := cfg.MQTTFingerprint()
	sess, cancel := context.WithCancel(ctx)
	defer cancel()
	go watchConfig(sess, mgr, fp, cancel)

	broker := cfg.MQTTBroker
	clientID := cfg.MQTTClientID
	if clientID == "" {
		host, _ := os.Hostname()
		clientID = fmt.Sprintf("mytracker-%s", host)
	}
	topic := mqttTopic(cfg)
	topicChanged := previousTopic != "" && previousTopic != topic
	callbacks := newCallbackGate(sess, ingest)
	opts := mqtt.NewClientOptions().
		AddBroker(broker).
		SetClientID(clientID).
		SetCleanSession(false).
		SetAutoAckDisabled(true).
		SetAutoReconnect(false).
		SetConnectTimeout(10 * time.Second).
		SetCustomOpenConnectionFn(openMQTTConnection)
	opts.SetDefaultPublishHandler(func(client mqtt.Client, msg mqtt.Message) {
		if matchesTopic(topic, msg.Topic()) {
			callbacks.handle(client, msg)
			return
		}
		msg.Ack()
	})
	lost := make(chan struct{}, 1)
	retry := make(chan struct{}, 1)
	opts.SetConnectionLostHandler(func(mqtt.Client, error) {
		select {
		case lost <- struct{}{}:
		default:
		}
	})
	if cfg.MQTTUser != "" {
		opts.SetUsername(cfg.MQTTUser)
		opts.SetPassword(cfg.MQTTPass)
	}
	if NeedsTLS(broker) {
		tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
		if cfg.MQTTCAFile != "" {
			pem, err := os.ReadFile(cfg.MQTTCAFile)
			if err != nil {
				return fmt.Errorf("mqtt ca: %w", err)
			}
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(pem) {
				return fmt.Errorf("mqtt ca: invalid PEM")
			}
			tlsCfg.RootCAs = pool
		}
		opts.SetTLSConfig(tlsCfg)
	}
	client := newClient(opts)
	callbacks.retry = func() {
		select {
		case retry <- struct{}{}:
		default:
		}
	}
	defer func() {
		cancel()
		setConnected(false, state)
		callbacks.stop()
		var oldTopics []string
		if topicChanged {
			oldTopics = append(oldTopics, previousTopic)
		}
		if mqttTopic(mgr.Snapshot()) != topic {
			oldTopics = append(oldTopics, topic)
		}
		if len(oldTopics) > 0 {
			unsubscribe(client, oldTopics...)
		}
		client.Disconnect(250)
		callbacks.wait()
	}()
	token := client.Connect()
	if err := waitToken(sess, token, 10*time.Second, "connect"); err != nil {
		return err
	}
	token = client.Subscribe(topic, 1, callbacks.handle)
	if err := waitToken(sess, token, 10*time.Second, "subscribe"); err != nil {
		return err
	}
	result, ok := token.(interface{ Result() map[string]byte })
	if !ok {
		return errors.New("mqtt subscription result unavailable")
	}
	qos, ok := result.Result()[topic]
	if !ok || qos > 1 {
		return errors.New("mqtt subscription rejected")
	}
	select {
	case <-sess.Done():
		return sess.Err()
	case <-lost:
		return errors.New("mqtt connection lost")
	case <-retry:
		return errMQTTIngestRetry
	default:
	}
	setConnected(true, state)
	select {
	case <-sess.Done():
		return sess.Err()
	case <-lost:
		return errors.New("mqtt connection lost")
	case <-retry:
		return errMQTTIngestRetry
	}
}

type callbackGate struct {
	ctx       context.Context
	retry     func()
	ingest    func(user, device string, body []byte) IngestResult
	mu        sync.Mutex
	accepting bool
	wg        sync.WaitGroup
}

func newCallbackGate(ctx context.Context, ingest func(user, device string, body []byte) IngestResult) *callbackGate {
	return &callbackGate{ctx: ctx, ingest: ingest, accepting: true}
}

func (g *callbackGate) handle(_ mqtt.Client, msg mqtt.Message) {
	if !g.enter() {
		return
	}
	defer g.wg.Done()
	if g.ctx.Err() != nil {
		return
	}
	user, device := parseTopic(msg.Topic())
	if user == "" || device == "" {
		msg.Ack()
		return
	}
	if g.ingest(user, device, msg.Payload()) != IngestRetry {
		msg.Ack()
		return
	}
	if g.retry != nil {
		g.retry()
	}
}

func (g *callbackGate) enter() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.accepting || g.ctx.Err() != nil {
		return false
	}
	g.wg.Add(1)
	return true
}

func (g *callbackGate) stop() {
	g.mu.Lock()
	g.accepting = false
	g.mu.Unlock()
}

func (g *callbackGate) wait() {
	g.wg.Wait()
}

func waitToken(ctx context.Context, token mqtt.Token, timeout time.Duration, operation string) error {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return fmt.Errorf("mqtt %s timeout", operation)
	case <-token.Done():
		return token.Error()
	}
}

func unsubscribe(client mqtt.Client, topics ...string) {
	if !client.IsConnected() || !client.IsConnectionOpen() {
		return
	}
	token := client.Unsubscribe(topics...)
	if err := waitToken(context.Background(), token, 250*time.Millisecond, "unsubscribe"); err != nil {
		slog.Debug("mqtt unsubscribe", "err", err)
	}
}

func mqttTopic(cfg *config.Config) string {
	if cfg.MQTTTopic == "" {
		return "owntracks/#"
	}
	return cfg.MQTTTopic
}

func matchesTopic(filter, topic string) bool {
	if shared, ok := strings.CutPrefix(filter, "$share/"); ok {
		group, topicFilter, ok := strings.Cut(shared, "/")
		if !ok || group == "" || topicFilter == "" || strings.ContainsAny(group, "+#") {
			return false
		}
		filter = topicFilter
	}
	filters, topics := strings.Split(filter, "/"), strings.Split(topic, "/")
	for i, level := range filters {
		if level == "#" {
			return i == len(filters)-1
		}
		if i >= len(topics) || level != "+" && level != topics[i] {
			return false
		}
	}
	return len(filters) == len(topics)
}

func setConnected(value bool, callbacks []func(bool)) {
	changed := connected.Swap(value) != value
	metrics.SetMQTTConnected(value)
	if changed {
		notifyState(callbacks, value)
	}
}

func notifyState(callbacks []func(bool), value bool) {
	if len(callbacks) > 0 && callbacks[0] != nil {
		callbacks[0](value)
	}
}

func Connected() bool {
	return connected.Load()
}

func wait(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func watchConfig(ctx context.Context, mgr *config.Manager, fp string, cancel context.CancelFunc) {
	t := time.NewTicker(configPollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			cur := mgr.Snapshot()
			if cur.MQTTFingerprint() != fp || !cur.MQTTEnabled {
				cancel()
				return
			}
		}
	}
}

func NeedsTLS(broker string) bool {
	scheme, _, _ := strings.Cut(strings.ToLower(broker), "://")
	switch scheme {
	case "ssl", "tls", "mqtts", "wss", "mqtt+ssl", "tcps":
		return true
	default:
		return false
	}
}

func parseTopic(topic string) (user, device string) {
	parts := strings.Split(topic, "/")
	if len(parts) < 3 || parts[1] == "" || parts[2] == "" {
		return "", ""
	}
	return parts[1], parts[2]
}
