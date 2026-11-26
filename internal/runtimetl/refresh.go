package runtimetl

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

const (
	sourceURL    = "https://raw.githubusercontent.com/telegramdesktop/tdesktop/dev/Telegram/SourceFiles/mtproto/scheme/api.tl"
	maxSchema    = 2 << 20
	refreshAfter = 5 * time.Minute
)

func Refresh(ctx context.Context, path string) (*Schema, error) {
	return refresh(ctx, path, time.Now(), func(ctx context.Context) ([]byte, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, sourceURL, nil)
		if err != nil {
			return nil, err
		}
		client := &http.Client{
			Timeout:       7 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("Telegram Desktop schema returned HTTP %d", resp.StatusCode)
		}
		return readBounded(resp.Body)
	})
}

func refresh(ctx context.Context, path string, now time.Time, fetch func(context.Context) ([]byte, error)) (*Schema, error) {
	previous, stamp := readCache(path)
	if previous != nil && !stamp.After(now) && now.Sub(stamp) < refreshAfter {
		return previous, nil
	}
	data, err := fetch(ctx)
	var next *Schema
	if err == nil {
		next, err = Parse(data)
	}
	if latest, _ := readCache(path); latest != nil {
		previous = latest
	}
	if err != nil || (previous != nil && previous.Layer > next.Layer) {
		if previous == nil {
			return nil, err
		}
		// A failed refresh starts a new retry window without replacing the cache.
		_ = os.Chtimes(path, now, now)
		return previous, nil
	}
	// The freshly fetched schema is usable even when the cache cannot be written.
	_ = writeCache(path, data)
	return next, nil
}

func readBounded(reader io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(reader, maxSchema+1))
	if err == nil && len(data) > maxSchema {
		return nil, fmt.Errorf("Telegram schema exceeds 2 MiB")
	}
	return data, err
}

func readCache(path string) (*Schema, time.Time) {
	file, err := os.Open(path)
	if err != nil {
		return nil, time.Time{}
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxSchema {
		return nil, time.Time{}
	}
	data, err := readBounded(file)
	if err != nil {
		return nil, time.Time{}
	}
	schema, err := Parse(data)
	if err != nil {
		return nil, time.Time{}
	}
	return schema, info.ModTime()
}

func writeCache(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".telegram-api-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	_, writeErr := file.Write(data)
	closeErr := file.Close()
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(file.Name(), path)
}
