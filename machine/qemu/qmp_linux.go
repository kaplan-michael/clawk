//go:build linux && amd64

package qemu

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"syscall"
	"time"
)

// qmpClient serializes commands and skips asynchronous events between replies.
type qmpClient struct {
	mu       contextMutex
	sequence uint64
	c        net.Conn
	r        *bufio.Reader
}

func dialQMP(ctx context.Context, path string) (*qmpClient, error) {
	var c net.Conn
	var err error
	for {
		c, err = (&net.Dialer{}).DialContext(ctx, "unix", path)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.ECONNREFUSED) && !errors.Is(err, syscall.ENOENT) {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
	q := &qmpClient{c: c, r: bufio.NewReader(c)}
	if err := q.readUntil(ctx, "QMP"); err != nil {
		c.Close()
		return nil, fmt.Errorf("qmp greeting: %w", err)
	}
	if _, err := q.command(ctx, "qmp_capabilities", nil); err != nil {
		c.Close()
		return nil, err
	}
	return q, nil
}

func (q *qmpClient) Close() error { return q.c.Close() }

func (q *qmpClient) readUntil(ctx context.Context, key string) error {
	stop := context.AfterFunc(ctx, func() { _ = q.c.Close() })
	defer stop()
	if d, ok := ctx.Deadline(); ok {
		_ = q.c.SetDeadline(d)
	} else {
		_ = q.c.SetDeadline(time.Now().Add(5 * time.Second))
	}
	for {
		var m map[string]json.RawMessage
		line, err := q.r.ReadBytes('\n')
		if err != nil {
			return err
		}
		if err = json.Unmarshal(line, &m); err != nil {
			return fmt.Errorf("invalid response: %w", err)
		}
		if _, ok := m[key]; ok {
			return nil
		}
		if raw, ok := m["error"]; ok {
			return fmt.Errorf("qmp error: %s", raw)
		}
	}
}

func (q *qmpClient) command(ctx context.Context, name string, args any) (result json.RawMessage, retErr error) {
	// A long migration budget must not turn an individual QMP query into a
	// ten-minute blocking read, or prevent another caller's cancellation.
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := q.mu.LockContext(ctx); err != nil {
		return nil, err
	}
	defer q.mu.Unlock()
	fired := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { _ = q.c.SetDeadline(time.Now()); close(fired) })
	defer func() {
		if !stop() {
			<-fired
		}
		if retErr != nil {
			if n, ok := retErr.(net.Error); ok && n.Timeout() {
				_ = q.Close()
			}
		}
	}()

	deadline, _ := ctx.Deadline() // bounded above, even without a caller deadline
	_ = q.c.SetDeadline(deadline)
	q.sequence++
	req := map[string]any{"execute": name, "id": q.sequence}
	if args != nil {
		req["arguments"] = args
	}
	b, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	if _, err = q.c.Write(append(b, '\n')); err != nil {
		return nil, err
	}
	for {
		line, err := q.r.ReadBytes('\n')
		if err != nil {
			return nil, err
		}
		var m map[string]json.RawMessage
		if err = json.Unmarshal(line, &m); err != nil {
			return nil, fmt.Errorf("invalid response: %w", err)
		}
		if _, event := m["event"]; event {
			continue
		}
		var id uint64
		if err := json.Unmarshal(m["id"], &id); err != nil || id != q.sequence {
			_ = q.Close()
			return nil, fmt.Errorf("qmp %s: unexpected reply id %s", name, m["id"])
		}
		if raw, ok := m["error"]; ok {
			return nil, fmt.Errorf("qmp %s: %s", name, raw)
		}
		if raw, ok := m["return"]; ok {
			return raw, nil
		}
		_ = q.Close()
		return nil, fmt.Errorf("qmp %s: reply has neither return nor error", name)
	}
}

func (q *qmpClient) status(ctx context.Context) (string, error) {
	raw, err := q.command(ctx, "query-status", nil)
	if err != nil {
		return "", err
	}
	var s struct {
		Status string `json:"status"`
	}
	if err = json.Unmarshal(raw, &s); err != nil {
		return "", err
	}
	return s.Status, nil
}
