package feishu

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

const downloadRangeBytes int64 = 4 << 20

// downloadResource normally makes exactly one GET. Only Feishu's explicit
// large-resource rejection opts into ranges; a JSON attachment is still a file.
// The caller has already authorized the message, author and exact resource key.
func (c *Client) downloadResource(ctx context.Context, target, token, destination string, limit int64) error {
	client := *c.http
	client.Timeout = 60 * time.Second
	request := func(start, end int64, etag string) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		if err != nil {
			return nil, safeFailure("download", err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Accept-Encoding", "identity")
		if start >= 0 {
			req.Header.Set("Range", "bytes="+strconv.FormatInt(start, 10)+"-"+strconv.FormatInt(end, 10))
			if strings.HasPrefix(etag, `"`) {
				req.Header.Set("If-Range", etag)
			}
		}
		resp, err := c.doHTTP(req, &client)
		if err != nil {
			var admission *APIError
			if errors.As(err, &admission) {
				return nil, admission
			}
			return nil, safeFailure("download", err)
		}
		return resp, nil
	}
	resp, err := request(-1, -1, "")
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusOK {
		defer resp.Body.Close()
		return writeDownload(ctx, destination, func(out *os.File) error { return copyWholeDownload(out, resp, limit) })
	}
	failure := c.downloadFailure(resp)
	_ = resp.Body.Close()
	if failure.Code != 234037 {
		return failure
	}
	return writeDownload(ctx, destination, func(out *os.File) error {
		var offset, total int64
		var etag string
		// Each accepted chunk advances by exactly the requested bytes. With the
		// hard 100 MiB ceiling this permits at most 25 range requests, no HEAD,
		// unlimited retry, in-memory assembly or parallel chunk buffering.
		for offset == 0 || offset < total {
			end := min(offset+downloadRangeBytes, limit) - 1
			if total > 0 {
				end = min(end, total-1)
			}
			part, err := request(offset, end, etag)
			if err != nil {
				return err
			}
			if part.StatusCode == http.StatusOK {
				// A server may ignore the very first Range. Its complete body is
				// still safe to accept within the same byte cap, never to append.
				if offset != 0 {
					_ = part.Body.Close()
					return errors.New("feishu: resource changed or server ignored a resumed range")
				}
				err = copyWholeDownload(out, part, limit)
				_ = part.Body.Close()
				return err
			}
			if part.StatusCode != http.StatusPartialContent {
				failure = c.downloadFailure(part)
				_ = part.Body.Close()
				return failure
			}
			start, last, length, valid := parseContentRange(part.Header.Get("Content-Range"))
			partETag := part.Header.Get("ETag")
			if !valid || start != offset || last != min(end, length-1) || length > limit || (offset > 0 && (total != length || etag != partETag)) || len(partETag) > 256 || strings.ContainsAny(partETag, "\r\n") {
				_ = part.Body.Close()
				return errors.New("feishu: invalid or changed resource range")
			}
			total, etag = length, partETag
			err = copyDownloadBytes(out, part, last-start+1, true)
			_ = part.Body.Close()
			if err != nil {
				return err
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			offset = last + 1
		}
		return nil
	})
}

func (c *Client) downloadFailure(resp *http.Response) *APIError {
	var envelope struct { Code int `json:"code"` }
	// Provider prose, URLs and arbitrary fields never enter logs or diagnostics.
	structured := readJSON(resp.Body, &envelope, 64<<10) == nil
	if !structured {
		envelope.Code = 0
	}
	failure := c.responseFailure(resp.Request, resp, envelope.Code, structured, false)
	failure.Operation = "download"
	return failure
}

func writeDownload(ctx context.Context, destination string, copyBody func(*os.File) error) error {
	out, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return errors.New("feishu: attachment destination is unavailable")
	}
	success := false
	defer func() {
		_ = out.Close()
		if !success {
			_ = os.Remove(destination)
		}
	}()
	if err = copyBody(out); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if err = out.Close(); err != nil {
		return errors.New("feishu: attachment write failed")
	}
	success = true
	return nil
}

func copyWholeDownload(out io.Writer, resp *http.Response, limit int64) error {
	if resp.ContentLength > limit {
		return errors.New("feishu: attachment exceeds byte limit")
	}
	return copyDownloadBytes(out, resp, limit, false)
}

func copyDownloadBytes(out io.Writer, resp *http.Response, count int64, exact bool) error {
	if encoding := resp.Header.Get("Content-Encoding"); encoding != "" && encoding != "identity" {
		return errors.New("feishu: encoded resource ranges are unsupported")
	}
	if exact && resp.ContentLength >= 0 && resp.ContentLength != count {
		return errors.New("feishu: resource range length does not match its headers")
	}
	n, err := io.Copy(out, io.LimitReader(resp.Body, count+1))
	if err != nil {
		return safeFailure("download body", err)
	}
	if n > count {
		return errors.New("feishu: attachment exceeds byte limit or range length")
	}
	if (exact && n != count) || (resp.ContentLength >= 0 && n != resp.ContentLength) {
		return errors.New("feishu: truncated resource body")
	}
	return nil
}

func parseContentRange(value string) (start, end, total int64, valid bool) {
	if len(value) > 128 || !strings.HasPrefix(value, "bytes ") {
		return 0, 0, 0, false
	}
	interval, length, ok := strings.Cut(strings.TrimPrefix(value, "bytes "), "/")
	first, last, hasEnd := strings.Cut(interval, "-")
	if !ok || !hasEnd {
		return 0, 0, 0, false
	}
	parse := func(s string) (int64, bool) {
		if s == "" {
			return 0, false
		}
		for _, ch := range s {
			if ch < '0' || ch > '9' {
				return 0, false
			}
		}
		n, err := strconv.ParseInt(s, 10, 64)
		return n, err == nil
	}
	var a, b, c bool
	start, a = parse(first)
	end, b = parse(last)
	total, c = parse(length)
	return start, end, total, a && b && c && start <= end && end < total
}
