//go:build integration

package pi_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"
)

// This deliberately small experiment client is not the production Pi adapter.
type rpcClient struct {
	command *exec.Cmd
	stdin   io.WriteCloser
	stdout  io.ReadCloser
	stderr  lockedBuffer
	done    chan struct{}
	notify  chan struct{}
	nextID  atomic.Uint64
	writeMu sync.Mutex
	mu      sync.Mutex
	records []map[string]any
	failure error
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

func startRPC(command *exec.Cmd) (*rpcClient, error) {
	c := &rpcClient{command: command, done: make(chan struct{}), notify: make(chan struct{})}
	var err error
	c.stdin, err = command.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		return nil, err
	}
	c.stdout = stdout
	command.Stderr = &c.stderr
	if err := command.Start(); err != nil {
		return nil, err
	}
	go func() {
		scanner := bufio.NewScanner(stdout) // ScanLines splits LF/CRLF, not Unicode separators.
		scanner.Buffer(make([]byte, 4096), 16<<20)
		var parseErr error
		for scanner.Scan() {
			var record map[string]any
			if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
				parseErr = errors.New("non-JSON record on Pi stdout")
				break
			}
			c.mu.Lock()
			c.records = append(c.records, record)
			close(c.notify) // Broadcast to every outstanding request/event waiter.
			c.notify = make(chan struct{})
			c.mu.Unlock()
		}
		if parseErr != nil {
			_ = command.Process.Kill()
		}
		waitErr := command.Wait()
		c.mu.Lock()
		c.failure = errors.Join(parseErr, scanner.Err(), waitErr)
		c.mu.Unlock()
		close(c.done)
	}()
	return c, nil
}

func (c *rpcClient) mark() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.records)
}

func (c *rpcClient) wait(ctx context.Context, start int, match func(map[string]any) bool) (map[string]any, error) {
	exited := false
	for {
		c.mu.Lock()
		for ; start < len(c.records); start++ {
			if record := c.records[start]; match(record) {
				c.mu.Unlock()
				return record, nil
			}
		}
		notify := c.notify
		c.mu.Unlock()
		if exited {
			return nil, errors.New("Pi transport closed before the expected record")
		}
		select {
		case <-notify:
		case <-c.done:
			exited = true // Rescan records delivered immediately before EOF.
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func (c *rpcClient) request(ctx context.Context, kind string, fields map[string]any) (map[string]any, error) {
	id, start, err := c.send(kind, fields)
	if err != nil {
		return nil, err
	}
	return c.wait(ctx, start, func(record map[string]any) bool { return record["type"] == "response" && record["id"] == id })
}

func (c *rpcClient) send(kind string, fields map[string]any) (string, int, error) {
	id := fmt.Sprintf("request-%d", c.nextID.Add(1))
	command := map[string]any{"id": id, "type": kind}
	for name, value := range fields {
		command[name] = value
	}
	content, err := json.Marshal(command)
	if err != nil {
		return "", 0, err
	}
	start := c.mark()
	c.writeMu.Lock()
	_, err = c.stdin.Write(append(content, '\n'))
	c.writeMu.Unlock()
	return id, start, err
}

func (c *rpcClient) close() error {
	_ = c.stdin.Close()
	select {
	case <-c.done:
	case <-time.After(10 * time.Second):
		_ = c.command.Process.Kill()
		_ = c.stdout.Close()
		select {
		case <-c.done:
		case <-time.After(6 * time.Second):
		}
		return errors.New("Pi transport did not exit after stdin EOF; backend cleanup still required")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.failure
}
