package lab

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// LogWriter returns where a lab service's logger should write.
//
// It is os.Stdout plus one append-only file per service, <service>.log under
// dir, which is the format and layout read_service_logs expects. Both, not the
// file alone: `docker compose logs` is still how a human looks at a container.
//
// An empty dir means stdout only, which is what a developer running the binary
// outside Compose wants.
//
// There is no rotation. This is a lab, and rotation would add the
// file-discovery problem the tool boundary's "no paths" decision exists to
// avoid — at the cost that a log left running for days grows without bound.
func LogWriter(dir, service string) (io.Writer, func() error, error) {
	if dir == "" {
		return os.Stdout, func() error { return nil }, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, nil, fmt.Errorf("creating the log directory %s: %w", dir, err)
	}
	path := filepath.Join(dir, service+".log")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, nil, fmt.Errorf("opening %s: %w", path, err)
	}
	return io.MultiWriter(os.Stdout, f), f.Close, nil
}
