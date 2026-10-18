package importzip

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"

	"github.com/prepaser/mytracker/internal/export"
)

type memoryFiles struct{ reader *zip.Reader }

func (m memoryFiles) Walk(ctx context.Context, fn func(int64, *zip.File) error) error {
	for n, f := range m.reader.File {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := fn(int64(n), f); err != nil {
			return err
		}
	}
	return nil
}

func (m memoryFiles) FileAt(offset int64) (*zip.File, error) {
	if offset < 0 || offset >= int64(len(m.reader.File)) {
		return nil, zip.ErrFormat
	}
	return m.reader.File[offset], nil
}

func validateContext(ctx context.Context, zr *zip.Reader) (archiveData, error) {
	return validateFilesContext(ctx, memoryFiles{reader: zr})
}

func proofSHA256Context(ctx context.Context, f *zip.File) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	rc, err := f.Open()
	if err != nil {
		return "", err
	}
	defer func() { _ = rc.Close() }()
	h := sha256.New()
	counted := &countWriter{w: h}
	if err := copyContext(ctx, counted, io.LimitReader(rc, maxProofBytes+1)); err != nil {
		return "", err
	}
	if counted.n > maxProofBytes {
		return "", fmt.Errorf("proof too large")
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func checkManifestContext(ctx context.Context, zr *zip.Reader) (export.Manifest, error) {
	for _, f := range zr.File {
		if err := ctx.Err(); err != nil {
			return export.Manifest{}, err
		}
		if f.Name != "manifest.json" {
			continue
		}
		if f.UncompressedSize64 > maxManifestBytes {
			return export.Manifest{}, fmt.Errorf("manifest too large")
		}
		rc, err := f.Open()
		if err != nil {
			return export.Manifest{}, err
		}
		var m export.Manifest
		dec := json.NewDecoder(io.LimitReader(contextReader{ctx: ctx, r: rc}, maxManifestBytes))
		err = dec.Decode(&m)
		if err == nil {
			var extra any
			err = dec.Decode(&extra)
			switch err {
			case nil:
				err = fmt.Errorf("manifest contains multiple JSON values")
			case io.EOF:
				err = nil
			}
		}
		_ = rc.Close()
		if err != nil {
			return export.Manifest{}, fmt.Errorf("manifest: %w", err)
		}
		if m.Version < 1 || m.Version > export.Version {
			return export.Manifest{}, fmt.Errorf("unsupported export version %d", m.Version)
		}
		return m, nil
	}
	return export.Manifest{}, fmt.Errorf("manifest.json required")
}
