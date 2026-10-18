package backup

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
)

const maxManifestValueBytes = 16 << 10

type manifestReader struct {
	io.Reader
	remaining int
	read      int64
}

func (r *manifestReader) Read(p []byte) (int, error) {
	if r.remaining == 0 {
		return 0, fmt.Errorf("manifest value exceeds size limit")
	}
	if len(p) > r.remaining {
		p = p[:r.remaining]
	}
	n, err := r.Reader.Read(p)
	r.remaining -= n
	r.read += int64(n)
	if r.read > maxBackupManifestSize {
		return n, fmt.Errorf("manifest exceeds size limit")
	}
	return n, err
}

func readManifest(ctx context.Context, r io.Reader, index *backupIndex) (Manifest, int64, error) {
	var m Manifest
	br := &manifestReader{Reader: r, remaining: maxManifestValueBytes}
	d := json.NewDecoder(br)
	token := func() (json.Token, error) { br.remaining = maxManifestValueBytes; return d.Token() }
	decode := func(v any) error { br.remaining = maxManifestValueBytes; return d.Decode(v) }
	opening, err := token()
	if err != nil || opening != json.Delim('{') {
		return m, br.read, fmt.Errorf("invalid backup manifest")
	}
	if err := index.Begin(ctx); err != nil {
		return m, br.read, err
	}
	seen := make(map[string]bool, 4)
	for d.More() {
		if err := ctx.Err(); err != nil {
			return m, br.read, err
		}
		key, err := token()
		if err != nil {
			return m, br.read, err
		}
		name, ok := key.(string)
		if !ok || seen[name] {
			return m, br.read, fmt.Errorf("invalid backup manifest field")
		}
		seen[name] = true
		switch name {
		case "format_version":
			err = decode(&m.FormatVersion)
		case "schema_version":
			err = decode(&m.SchemaVersion)
		case "created_at":
			err = decode(&m.CreatedAt)
		case "files":
			var files json.Token
			files, err = token()
			if err == nil && files != json.Delim('{') {
				err = fmt.Errorf("invalid backup file manifest")
			}
			for err == nil && d.More() {
				if err = ctx.Err(); err != nil {
					break
				}
				var key json.Token
				key, err = token()
				if err != nil {
					break
				}
				filename, ok := key.(string)
				if !ok || filename == "manifest.json" || !validBackupName(filename) {
					err = fmt.Errorf("invalid backup file name")
					break
				}
				var info FileInfo
				if err = decode(&info); err != nil {
					break
				}
				var hash []byte
				hash, err = hex.DecodeString(info.SHA256)
				if err == nil && (len(hash) != 32 || info.Size < 0 || info.Size > entryLimit(filename)) {
					err = fmt.Errorf("invalid backup file metadata")
				}
				if err == nil {
					err = index.SetInfo(ctx, filename, info)
				}
			}
			if err == nil {
				_, err = token()
			}
		default:
			err = fmt.Errorf("unknown backup manifest field")
		}
		if err != nil {
			return m, br.read, err
		}
	}
	if _, err := token(); err != nil {
		return m, br.read, err
	}
	var extra any
	if err := decode(&extra); err != io.EOF {
		return m, br.read, fmt.Errorf("invalid backup manifest suffix")
	}
	if !seen["files"] || m.FormatVersion != FormatVersion || m.SchemaVersion < 1 {
		return m, br.read, fmt.Errorf("unsupported backup format or schema")
	}
	if err := index.Commit(); err != nil {
		return m, br.read, err
	}
	return m, br.read, nil
}

func writeManifest(ctx context.Context, w io.Writer, m Manifest, index *backupIndex) error {
	header, err := json.Marshal(struct {
		FormatVersion int    `json:"format_version"`
		SchemaVersion int    `json:"schema_version"`
		CreatedAt     string `json:"created_at"`
	}{m.FormatVersion, m.SchemaVersion, m.CreatedAt})
	if err != nil {
		return err
	}
	if _, err := w.Write(header[:len(header)-1]); err != nil {
		return err
	}
	if _, err := io.WriteString(w, `,"files":{`); err != nil {
		return err
	}
	first := true
	if err := index.Each(ctx, func(f indexedFile) error {
		name, err := json.Marshal(f.Name)
		if err != nil {
			return err
		}
		info, err := json.Marshal(f.Info)
		if err != nil {
			return err
		}
		if !first {
			if _, err := io.WriteString(w, ","); err != nil {
				return err
			}
		}
		first = false
		if _, err := w.Write(name); err != nil {
			return err
		}
		if _, err := io.WriteString(w, ":"); err != nil {
			return err
		}
		_, err = w.Write(info)
		return err
	}); err != nil {
		return err
	}
	_, err = io.WriteString(w, "}}\n")
	return err
}
