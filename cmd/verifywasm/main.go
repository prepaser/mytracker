//go:build js && wasm

package main

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"syscall/js"
	"time"

	"github.com/prepaser/mytracker/internal/browserverify"
)

func send(value any) {
	b, err := json.Marshal(value)
	if err != nil {
		js.Global().Call("postMessage", map[string]any{"type": "error", "message": "report_encoding_failed"})
		return
	}
	js.Global().Call("postMessage", js.Global().Get("JSON").Call("parse", string(b)))
}

func main() {
	var busy atomic.Bool
	handler := js.FuncOf(func(this js.Value, args []js.Value) any {
		if len(args) == 0 {
			return nil
		}
		message := args[0].Get("data")
		if message.Get("type").String() != "verify" {
			return nil
		}
		if !busy.CompareAndSwap(false, true) {
			return nil
		}
		go func() {
			defer busy.Store(false)
			defer func() {
				if recover() != nil {
					send(map[string]any{"type": "error", "message": "verification_failed"})
				}
			}()
			input := js.Global().Get("Uint8Array").New(message.Get("archive"))
			if input.Length() == 0 || input.Length() > 64<<20 {
				send(map[string]any{"type": "error", "message": "archive_size_limit"})
				return
			}
			data := make([]byte, input.Length())
			js.CopyBytesToGo(data, input)
			var opts browserverify.Options
			if err := json.Unmarshal([]byte(js.Global().Get("JSON").Call("stringify", message.Get("options")).String()), &opts); err != nil {
				send(map[string]any{"type": "error", "message": "invalid_options"})
				return
			}
			lastPhase := ""
			var lastProgress time.Time
			report, err := browserverify.Verify(context.Background(), data, opts, func(p browserverify.Progress) {
				if p.Phase == lastPhase && p.Completed != p.Total && time.Since(lastProgress) < 100*time.Millisecond {
					return
				}
				lastPhase, lastProgress = p.Phase, time.Now()
				send(map[string]any{"type": "progress", "phase": p.Phase, "completed": p.Completed, "total": p.Total})
			})
			if err != nil {
				send(map[string]any{"type": "error", "message": err.Error()})
				return
			}
			send(map[string]any{"type": "result", "report": report})
		}()
		return nil
	})
	js.Global().Set("onmessage", handler)
	send(map[string]any{"type": "ready"})
	select {}
}
