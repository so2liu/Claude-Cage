package hostproxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const parallelDownloadMinimum = 8 << 20

type downloadOutput struct {
	sync.Mutex
	io.Writer
}

func (w *downloadOutput) Write(p []byte) (int, error) {
	w.Lock()
	defer w.Unlock()
	return w.Writer.Write(p)
}

type downloadRange struct {
	start, end int64
	path       string
	saved      int64
}

// Files contain contiguous bytes only. Bounded ranges remain separate until
// they can be appended in order; a sparse file's length is never resume state.
func downloadRanges(parent context.Context, client *http.Client, address, target, name, accept string, out io.Writer, idle time.Duration, offset, total int64) error {
	root := target + ".ranges"
	if err := os.MkdirAll(root, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() {
		return errors.New("安装包分段缓存目录无效")
	}
	flags := os.O_CREATE | os.O_WRONLY
	if offset == 0 {
		flags |= os.O_TRUNC
	}
	file, err := os.OpenFile(target, flags, 0600)
	if err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	width := (total + 3) / 4
	var parts []downloadRange
	received := offset
	for i := int64(0); i < 4; i++ {
		start, end := max(offset, i*width), min(total, (i+1)*width)-1
		if start > end {
			continue
		}
		part := downloadRange{start: start, end: end, path: filepath.Join(root, fmt.Sprintf("%d-%d", start, end))}
		info, err := os.Lstat(part.path)
		if err == nil {
			if !info.Mode().IsRegular() {
				return errors.New("安装包分段缓存路径无效")
			}
			if info.Size() > end-start+1 {
				if err := os.Remove(part.path); err != nil {
					return err
				}
			} else {
				part.saved = info.Size()
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		received += part.saved
		parts = append(parts, part)
	}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	var progress sync.Mutex
	report := func(n int) {
		progress.Lock()
		defer progress.Unlock()
		received += int64(n)
		fmt.Fprintf(out, "CAGE_HOST_BYTES %s %d %d\n", name, received, total)
	}
	fmt.Fprintf(out, "%s：最多 4 连接分段下载\n", name)
	report(0)
	results := make(chan error, len(parts))
	for _, part := range parts {
		go func() {
			err := downloadRangeOnce(ctx, client, address, accept, part, total, idle, report)
			if err != nil {
				cancel()
			}
			results <- err
		}()
	}
	var failed error
	for range parts {
		if err := <-results; err != nil && failed == nil {
			failed = err
		}
	}
	// Join every worker before merging, even when cancellation interrupts the
	// request. Later ranges remain reusable on the next installation attempt.
	file, err = os.OpenFile(target, os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	defer file.Close()
	current := offset
	for _, part := range parts {
		if current != part.start {
			break
		}
		source, err := os.Open(part.path)
		if errors.Is(err, os.ErrNotExist) {
			break
		}
		if err != nil {
			return err
		}
		n, copyErr := io.Copy(file, source)
		closeErr := source.Close()
		if err := errors.Join(copyErr, closeErr); err != nil {
			return err
		}
		if err := os.Remove(part.path); err != nil {
			return err
		}
		current += n
		if current != part.end+1 {
			break
		}
	}
	if err := file.Close(); err != nil {
		return err
	}
	if parent.Err() != nil {
		return parent.Err()
	}
	if failed != nil {
		return failed
	}
	if current != total {
		return errors.New("安装包分段未收齐")
	}
	return nil
}

func downloadRangeOnce(parent context.Context, client *http.Client, address, accept string, part downloadRange, total int64, idle time.Duration, report func(int)) error {
	start := part.start + part.saved
	if start == part.end+1 {
		return nil
	}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil || req.URL.Scheme != "https" || req.URL.User != nil {
		return errors.New("安装材料必须通过 HTTPS 下载")
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, part.end))
	req.Header.Set("Accept-Encoding", "identity")
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	response, err := client.Do(req)
	if err != nil {
		return errors.New("分段连接失败或等待响应超时")
	}
	defer response.Body.Close()
	length := part.end - start + 1
	if response.StatusCode != http.StatusPartialContent ||
		response.Header.Get("Content-Range") != fmt.Sprintf("bytes %d-%d/%d", start, part.end, total) ||
		(response.ContentLength >= 0 && response.ContentLength != length) ||
		(response.Header.Get("Content-Encoding") != "" && response.Header.Get("Content-Encoding") != "identity") {
		return errInvalidRange
	}
	file, err := os.OpenFile(part.path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	defer file.Close()
	if err := copyDownload(ctx, cancel, response.Body, file, idle, length, report); err != nil {
		return err
	}
	return file.Close()
}
