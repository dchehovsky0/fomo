package logger

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

type Bundle struct {
	Log  *slog.Logger
	File *os.File
}

func New(dir string) (*Bundle, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	name := filepath.Join(dir, "fomo-"+time.Now().UTC().Format("20060102T150405Z")+".jsonl")
	f, err := os.OpenFile(name, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	out := io.MultiWriter(os.Stdout, f)
	h := slog.NewJSONHandler(out, &slog.HandlerOptions{Level: slog.LevelDebug})
	return &Bundle{Log: slog.New(h), File: f}, nil
}

func (b *Bundle) Close() error {
	if b == nil || b.File == nil {
		return nil
	}
	if err := b.File.Sync(); err != nil {
		return fmt.Errorf("sync log: %w", err)
	}
	return b.File.Close()
}
