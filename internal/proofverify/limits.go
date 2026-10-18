package proofverify

import (
	"io"
	"os"
)

func readBoundedFile(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errArtifactTooLarge
	}
	return data, nil
}

func stageTrustRoots(path string) (string, func(), error) {
	if path == "" {
		return "", func() {}, nil
	}
	data, err := readBoundedFile(path, maxArtifactBytes)
	if err != nil {
		return "", func() {}, err
	}
	f, err := os.CreateTemp("", ".mytracker-trust-roots-*")
	if err != nil {
		return "", func() {}, err
	}
	tmp := f.Name()
	cleanup := func() { _ = os.Remove(tmp) }
	if err := writeAndSync(f, data); err != nil {
		_ = f.Close()
		cleanup()
		return "", func() {}, err
	}
	if err := f.Close(); err != nil {
		cleanup()
		return "", func() {}, err
	}
	return tmp, cleanup, nil
}

func stageEmptyTrustRoots() (string, func(), error) {
	f, err := os.CreateTemp("", ".mytracker-empty-roots-*")
	if err != nil {
		return "", func() {}, err
	}
	path := f.Name()
	cleanup := func() { _ = os.Remove(path) }
	if err := f.Close(); err != nil {
		cleanup()
		return "", func() {}, err
	}
	return path, cleanup, nil
}

func writeAndSync(f *os.File, data []byte) error {
	if n, err := f.Write(data); err != nil {
		return err
	} else if n != len(data) {
		return io.ErrShortWrite
	}
	return f.Sync()
}
