//go:build linux && amd64

package qemu

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestQMPNegotiatesIgnoresEventsAndQueriesStatus(t *testing.T) {
	path := filepath.Join(t.TempDir(), "qmp.sock")
	l, err := net.Listen("unix", path)
	if errors.Is(err, syscall.EPERM) {
		t.Skip("sandbox forbids Unix listeners")
	}
	require.NoError(t, err)
	defer l.Close()
	go func() {
		c, e := l.Accept()
		if e != nil {
			return
		}
		defer c.Close()
		_, _ = c.Write([]byte(`{"QMP":{"version":{},"capabilities":[]}}` + "\n"))
		r := bufio.NewReader(c)
		for {
			line, e := r.ReadBytes('\n')
			if e != nil {
				return
			}
			var req struct {
				Execute string `json:"execute"`
				ID      uint64 `json:"id"`
			}
			_ = json.Unmarshal(line, &req)
			_, _ = c.Write([]byte(`{"event":"RESET"}` + "\n"))
			if req.Execute == "query-status" {
				_ = json.NewEncoder(c).Encode(map[string]any{"id": req.ID, "return": map[string]string{"status": "running"}})
			} else {
				_ = json.NewEncoder(c).Encode(map[string]any{"id": req.ID, "return": map[string]any{}})
			}
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	q, err := dialQMP(ctx, path)
	require.NoError(t, err)
	defer q.Close()
	s, err := q.status(ctx)
	require.NoError(t, err)
	require.Equal(t, "running", s)
}

func TestQMPReportsProtocolErrors(t *testing.T) {
	for _, tc := range []struct{ reply, want string }{
		{`{"id":1,"error":{"class":"GenericError","desc":"no"}}`, "GenericError"},
		{`{"id":2,"return":{}}`, "unexpected reply id"},
		{`{"id":1}`, "neither return nor error"},
	} {
		t.Run(tc.want, func(t *testing.T) {
			a, b := net.Pipe()
			defer a.Close()
			defer b.Close()
			q := &qmpClient{c: a, r: bufio.NewReader(a)}
			go func() {
				_, _ = bufio.NewReader(b).ReadBytes('\n')
				_, _ = b.Write([]byte(tc.reply + "\n"))
			}()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_, err := q.command(ctx, "stop", nil)
			require.ErrorContains(t, err, tc.want)
		})
	}
}

func TestQueuedQMPCommandHonorsCancellation(t *testing.T) {
	q := &qmpClient{}
	q.mu.Lock()
	defer q.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := q.command(ctx, "stop", nil)
	require.ErrorIs(t, err, context.DeadlineExceeded)
}
