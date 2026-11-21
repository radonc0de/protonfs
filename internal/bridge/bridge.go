// Package bridge runs the Node.js protonfs-bridge process and talks to it over
// newline-delimited JSON on its stdin/stdout.
package bridge

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// Error is a failed bridge operation carrying an errno for FUSE.
type Error struct {
	Errno syscall.Errno
	Msg   string
}

func (e *Error) Error() string { return e.Msg }

var codes = map[string]syscall.Errno{
	"ENOENT": syscall.ENOENT, "EEXIST": syscall.EEXIST, "EACCES": syscall.EACCES,
	"EINVAL": syscall.EINVAL, "EAGAIN": syscall.EAGAIN, "EINTR": syscall.EINTR,
	"ENOSYS": syscall.ENOSYS, "EIO": syscall.EIO,
}

// Errno extracts an errno from any error (EIO by default).
func Errno(err error) syscall.Errno {
	var be *Error
	if errors.As(err, &be) {
		return be.Errno
	}
	return syscall.EIO
}

type response struct {
	ID     int64           `json:"id"`
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result"`
	Code   string          `json:"code"`
	Error  string          `json:"error"`
	Event  string          `json:"event"`
	Bytes  int64           `json:"bytes"`
}

type pending struct {
	ch       chan response
	progress func(int64)
}

// Client manages one bridge process, restarting it if it dies.
type Client struct {
	Node, Script string

	mu      sync.Mutex
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	nextID  int64
	pending map[int64]*pending
	alive   bool
}

func New(node, script string) *Client {
	return &Client{Node: node, Script: script, pending: map[int64]*pending{}}
}

// Start launches the bridge and waits for it to report ready (which includes
// loading the session and decrypting account keys).
func (c *Client) Start(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.startLocked(ctx)
}

func (c *Client) startLocked(ctx context.Context) error {
	if c.alive {
		return nil
	}
	cmd := exec.Command(c.Node, c.Script)
	cmd.Stderr = os.Stderr
	cmd.Env = os.Environ()
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting bridge: %w", err)
	}
	ready := make(chan error, 1)
	c.cmd, c.stdin, c.alive = cmd, stdin, true
	go c.readLoop(cmd, stdout, ready)

	select {
	case err := <-ready:
		if err != nil {
			return err
		}
		return nil
	case <-ctx.Done():
		cmd.Process.Kill()
		return ctx.Err()
	case <-time.After(3 * time.Minute):
		cmd.Process.Kill()
		return errors.New("bridge did not become ready")
	}
}

func (c *Client) readLoop(cmd *exec.Cmd, stdout io.Reader, ready chan<- error) {
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	readySent := false
	for sc.Scan() {
		var r response
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			continue
		}
		if r.Event == "ready" {
			if !readySent {
				readySent = true
				ready <- nil
			}
			continue
		}
		c.mu.Lock()
		p := c.pending[r.ID]
		if p != nil && r.Event == "" {
			delete(c.pending, r.ID)
		}
		c.mu.Unlock()
		if p == nil {
			continue
		}
		if r.Event == "progress" {
			if p.progress != nil {
				p.progress(r.Bytes)
			}
			continue
		}
		p.ch <- r
	}
	err := cmd.Wait()
	log.Printf("bridge exited: %v", err)
	if !readySent {
		ready <- fmt.Errorf("bridge exited during startup (%v); is `proton-drive auth login` done?", err)
	}
	c.mu.Lock()
	c.alive = false
	for id, p := range c.pending {
		p.ch <- response{ID: id, Code: "EIO", Error: "bridge exited"}
		delete(c.pending, id)
	}
	c.mu.Unlock()
}

// Call performs op and decodes the result into out (if non-nil).
func (c *Client) Call(ctx context.Context, op string, args any, out any) error {
	return c.CallProgress(ctx, op, args, out, nil)
}

// CallProgress is Call with progress events (bytes so far) for downloads.
func (c *Client) CallProgress(ctx context.Context, op string, args any, out any, progress func(int64)) error {
	c.mu.Lock()
	if !c.alive {
		log.Printf("restarting bridge")
		if err := c.startLocked(context.Background()); err != nil {
			c.mu.Unlock()
			return &Error{syscall.EIO, err.Error()}
		}
	}
	c.nextID++
	id := c.nextID
	p := &pending{ch: make(chan response, 1), progress: progress}
	c.pending[id] = p
	line, err := json.Marshal(map[string]any{"id": id, "op": op, "args": args})
	if err == nil {
		_, err = c.stdin.Write(append(line, '\n'))
	}
	c.mu.Unlock()
	if err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return &Error{syscall.EIO, err.Error()}
	}

	select {
	case r := <-p.ch:
		if !r.OK {
			errno, ok := codes[r.Code]
			if !ok {
				errno = syscall.EIO
			}
			return &Error{errno, fmt.Sprintf("%s: %s", op, r.Error)}
		}
		if out != nil && len(r.Result) > 0 {
			return json.Unmarshal(r.Result, out)
		}
		return nil
	case <-ctx.Done():
		c.Call(context.Background(), "cancel", map[string]any{"target": id}, nil)
		return &Error{syscall.EINTR, ctx.Err().Error()}
	}
}

// Close stops the bridge.
func (c *Client) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.alive {
		c.stdin.Close()
	}
}
