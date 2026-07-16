package downloader

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"strconv"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/nicoxiang/geektime-downloader/internal/pkg/logger"
)

// Part use this struct as a channel type.
// The struct will have 2 fields. One for housing data and the other specifying its location within the final file.
type Part struct {
	Data  []byte
	Index int
	Offset int64
}

// StatusError is returned when a file download response is not 2xx,
// so callers never mistake an error page for file content.
type StatusError struct {
	StatusCode int
	Body       string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("download responded status %d: %s", e.StatusCode, e.Body)
}

const defaultSegmentTimeout = 60 * time.Second

var httpClient = &http.Client{
	Transport: &http.Transport{
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		IdleConnTimeout:      90 * time.Second,
	},
}

func segmentTimeoutOrDefault(d time.Duration) time.Duration {
	if d <= 0 {
		return defaultSegmentTimeout
	}
	return d
}

// DownloadFileConcurrently download file in chunks, return total file size
func DownloadFileConcurrently(ctx context.Context, filepath string, url string, headers map[string]string, concurrency int, segmentTimeout time.Duration) (int64, error) {
	headCtx, headCancel := context.WithTimeout(ctx, segmentTimeoutOrDefault(segmentTimeout))
	req, err := http.NewRequestWithContext(headCtx, "HEAD", url, nil)
	if err != nil {
		headCancel()
		return 0, err
	}
	// propagate headers (if any)
	for k, v := range headers {
		req.Header.Add(k, v)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		headCancel()
		return 0, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		_ = resp.Body.Close()
		headCancel()
		return 0, &StatusError{StatusCode: resp.StatusCode, Body: string(body)}
	}
	_ = resp.Body.Close()
	headCancel()

	fileSize, _ := strconv.ParseInt(resp.Header.Get("Content-Length"), 10, 64)
	if fileSize <= 0 {
		out, err := os.OpenFile(filepath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o666)
		if err != nil {
			return 0, err
		}
		defer func() {
			_ = out.Close()
		}()
		return fileSize, nil
	}

	if concurrency <= 0 {
		concurrency = 1
	}
	if int64(concurrency) > fileSize {
		concurrency = int(fileSize)
	}

	g, ctx := errgroup.WithContext(ctx)

	results := make(chan Part, concurrency)

	chunkSize := fileSize / int64(concurrency)

	for i := 0; i < concurrency; i++ {
		i := i
		g.Go(func() error {
			return download(ctx, concurrency, i, chunkSize, url, headers, segmentTimeout, results)
		})
	}

	errCh := make(chan error, 1)
	go func() {
		err := g.Wait()
		close(results)
		errCh <- err
		close(errCh)
	}()

	out, err := os.OpenFile(filepath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o666)
	if err != nil {
		return 0, err
	}
	removeOnError := false
	defer func() {
		_ = out.Close()
		if removeOnError {
			_ = os.Remove(filepath)
		}
	}()

	nextIndex := 0
	pending := make(map[int]Part, concurrency)
	var writeErr error

	for part := range results {
		if writeErr != nil {
			continue
		}
		pending[part.Index] = part
		for {
			nextPart, ok := pending[nextIndex]
			if !ok {
				break
			}
			_, err := out.WriteAt(nextPart.Data, nextPart.Offset)
			if err != nil {
				writeErr = err
				break
			}
			delete(pending, nextIndex)
			nextIndex++
		}
	}

	if writeErr != nil {
		removeOnError = true
		return 0, writeErr
	}

	if err := <-errCh; err != nil {
		removeOnError = true
		return 0, err
	}

	return fileSize, nil
}

func download(ctx context.Context, workers int, index int, chunkSize int64, url string, headers map[string]string, segmentTimeout time.Duration, c chan Part) error {
	// calculate offset by multiplying
	// index with size
	start := int64(index) * chunkSize

	// Write data range in correct format
	// I'm reducing one from the end size to account for
	// the next chunk starting there
	dataRange := fmt.Sprintf("bytes=%d-%d", start, start+chunkSize-1)

	// if this is downloading the last chunk
	// rewrite the header. It's an easy way to specify
	// getting the rest of the file
	if index == workers-1 {
		dataRange = fmt.Sprintf("bytes=%d-", start)
	}

	timeout := segmentTimeoutOrDefault(segmentTimeout)
	err := retry(ctx, 3, 700*time.Millisecond, func() error {
		rctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		req, err := http.NewRequestWithContext(rctx, "GET", url, nil)
		if err != nil {
			return err
		}
		for k, v := range headers {
			req.Header.Add(k, v)
		}
		req.Header.Add("Range", dataRange)
		// fix error: http2: server sent GOAWAY and closed the connection; LastStreamID=1999
		// error comes from io read, not request
		resp, err := httpClient.Do(req)
		if err != nil {
			return err
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
			return &StatusError{StatusCode: resp.StatusCode, Body: string(body)}
		}
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return err
		}
		c <- Part{Index: index, Offset: start, Data: body}
		return nil
	})
	return err
}

func retry(ctx context.Context, attempts int, sleep time.Duration, f func() error) (err error) {
	for i := 0; i < attempts; i++ {
		if i > 0 {
			jitter := time.Duration(rand.Intn(int(sleep/5) + 1))
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(sleep + jitter):
			}
			sleep = sleep * 2
			logger.Infof("retry happen, times: %s", strconv.Itoa(i))
		}
		err = f()
		// context.Canceled = parent (job) cancelled → terminal, do not retry.
		// Per-request DeadlineExceeded (segment timeout) IS retried (transient);
		// the loop's ctx.Done() guard above prevents retrying when the parent
		// job ctx itself has expired/cancelled.
		if err == nil || errors.Is(err, context.Canceled) {
			return err
		}
		var se *StatusError
		if errors.As(err, &se) {
			return err // server-side rejection (4xx): do not retry
		}
	}
	return fmt.Errorf("after %d attempts, last error: %s", attempts, err)
}
