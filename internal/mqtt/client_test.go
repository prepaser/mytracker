package mqtt_test

import (
	"context"
	"testing"
	"time"

	"github.com/prepaser/mytracker/internal/config"
	mq "github.com/prepaser/mytracker/internal/mqtt"
)

func TestNeedsTLS(t *testing.T) {
	for _, scheme := range []string{"ssl", "tls", "mqtts", "wss", "mqtt+ssl", "tcps", "WSS"} {
		if !mq.NeedsTLS(scheme + "://broker:8883") {
			t.Fatalf("%s should need TLS", scheme)
		}
	}
	for _, scheme := range []string{"tcp", "mqtt", "ws", "unix"} {
		if mq.NeedsTLS(scheme + "://broker:1883") {
			t.Fatalf("%s should not need TLS", scheme)
		}
	}
}

func TestDisabledRunStopsWithContext(t *testing.T) {
	cfg := config.Default()
	mgr := config.NewManager(cfg, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		mq.Run(ctx, mgr, func(string, string, []byte) mq.IngestResult { return mq.IngestDropped })
		close(done)
	}()
	time.Sleep(10 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("run did not stop")
	}
	if mq.Connected() {
		t.Fatal("disabled MQTT reported connected")
	}
}
