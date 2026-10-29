package api

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"time"

	sgauth "github.com/RakuenSoftware/smoothgui/auth"
	"github.com/creack/pty/v2"
	"nhooyr.io/websocket"
)

// terminalPingInterval keeps an idle session's connection alive. nginx
// closes a proxied connection that carries no data for proxy_read_timeout
// (60 s by default), which ended any session whose command printed nothing
// for a minute, such as a DKMS build during an upgrade.
const terminalPingInterval = 20 * time.Second

// TerminalHandler serves an interactive shell over a WebSocket.
type TerminalHandler struct{}

// NewTerminalHandler creates a TerminalHandler.
func NewTerminalHandler() *TerminalHandler {
	return &TerminalHandler{}
}

// ServeHTTP upgrades the connection to a WebSocket and attaches a PTY shell.
//
// Text messages from the client are written to the PTY as stdin.
// Binary messages with exactly 4 bytes are treated as a resize event:
//
//	[cols_hi, cols_lo, rows_hi, rows_lo]
//
// All PTY output is sent back as text messages.
func (h *TerminalHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	username := sgauth.GetUsername(r)
	if username == "" {
		jsonAuthRequired(w)
		return
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{})
	if err != nil {
		return
	}
	defer conn.Close(websocket.StatusInternalError, "unexpected close")

	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/bash"
	}

	cmd := exec.Command(shell, "-l")
	cmd.Env = append(os.Environ(),
		"TERM=xterm-256color",
		"USER="+username,
		"HOME=/root",
	)

	ptmx, err := pty.Start(cmd)
	if err != nil {
		conn.Close(websocket.StatusInternalError, "failed to start shell")
		return
	}
	defer ptmx.Close()

	ctx := r.Context()
	var closeOnce sync.Once
	done := make(chan struct{})

	// PTY → WebSocket
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := ptmx.Read(buf)
			if n > 0 {
				if writeErr := conn.Write(ctx, websocket.MessageText, buf[:n]); writeErr != nil {
					break
				}
			}
			if err != nil {
				break
			}
		}
		closeOnce.Do(func() { close(done) })
	}()

	// Keepalive: a ping every terminalPingInterval keeps traffic on the
	// connection while the shell is quiet.
	go func() {
		ticker := time.NewTicker(terminalPingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				pingCtx, cancel := context.WithTimeout(ctx, terminalPingInterval)
				err := conn.Ping(pingCtx)
				cancel()
				if err != nil {
					closeOnce.Do(func() { close(done) })
					return
				}
			}
		}
	}()

	// WebSocket → PTY
	go func() {
		for {
			msgType, data, err := conn.Read(ctx)
			if err != nil {
				break
			}

			if msgType == websocket.MessageBinary && len(data) == 4 {
				// Resize: [cols_hi, cols_lo, rows_hi, rows_lo]
				cols := uint16(data[0])<<8 | uint16(data[1])
				rows := uint16(data[2])<<8 | uint16(data[3])
				pty.Setsize(ptmx, &pty.Winsize{
					Cols: cols,
					Rows: rows,
				})
				continue
			}

			if _, err := ptmx.Write(data); err != nil {
				break
			}
		}
		closeOnce.Do(func() { close(done) })
	}()

	<-done
	cmd.Process.Kill()
	cmd.Wait()
	conn.Close(websocket.StatusNormalClosure, "session ended")
}
