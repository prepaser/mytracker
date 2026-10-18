package canonical_test

import (
	"encoding/json"
	"testing"

	"github.com/prepaser/mytracker/internal/canonical"
)

func FuzzLocationPayload(f *testing.F) {
	f.Add([]byte(`{"tst":1,"lat":37.5,"lon":127.0}`))
	f.Add([]byte(`null`))
	f.Fuzz(func(t *testing.T, data []byte) {
		var raw map[string]any
		if json.Unmarshal(data, &raw) != nil || raw == nil {
			return
		}
		record, err := canonical.FromLocation("user", "device", raw)
		if err == nil {
			_, _ = record.Digest()
		}
	})
}
