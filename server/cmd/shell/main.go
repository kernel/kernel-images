package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	oapi "github.com/kernel/kernel-images/server/lib/oapi"
	openapi_types "github.com/oapi-codegen/runtime/types"
	"golang.org/x/term"
)

func main() {
	var serverURL string
	flag.StringVar(&serverURL, "server", "http://localhost:444", "Base URL to API server (e.g., http://localhost:444)")
	flag.Parse()

	u, err := ensureHTTPURL(serverURL)
	if err != nil {
		log.Fatalf("invalid server URL: %v", err)
	}

	// Determine terminal size (cols, rows)
	cols, rows := 80, 24
	if term.IsTerminal(int(os.Stdin.Fd())) {
		if w, h, err := term.GetSize(int(os.Stdin.Fd())); err == nil {
			cols, rows = w, h
		}
	}

	// Prepare client
	client, err := oapi.NewClientWithResponses(u.String())
	if err != nil {
		log.Fatalf("failed to init client: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Spawn bash with TTY
	body := oapi.ProcessSpawnJSONRequestBody{
		Command: "/bin/bash",
	}
	body.AllocateTty = boolPtr(true)
	body.Rows = &rows
	body.Cols = &cols
	args := []string{}
	body.Args = &args

	resp, err := client.ProcessSpawnWithResponse(ctx, body)
	if err != nil {
		log.Fatalf("spawn request failed: %v", err)
	}
	if resp.JSON200 == nil || resp.JSON200.ProcessId == nil {
		log.Fatalf("unexpected response: %+v", resp)
	}
	procID := resp.JSON200.ProcessId.String()

	// Attach via HTTP hijack
	var (
		rawConn net.Conn
	)
	{
		addr := u.Host
		if addr == "" {
			// Fallback for URLs like http://localhost
			addr = u.Hostname()
			if port := u.Port(); port != "" {
				addr = net.JoinHostPort(addr, port)
			} else {
				// Default ports by scheme
				if u.Scheme == "https" {
					addr = net.JoinHostPort(addr, "443")
				} else {
					addr = net.JoinHostPort(addr, "80")
				}
			}
		}
		// Dial based on scheme
		switch u.Scheme {
		case "https":
			tlsConf := &tls.Config{
				ServerName: u.Hostname(),
				MinVersion: tls.VersionTLS12,
			}
			rc, err := tls.Dial("tcp", addr, tlsConf)
			if err != nil {
				log.Fatalf("failed to tls dial %s: %v", addr, err)
			}
			rawConn = rc
		default:
			rc, err := net.Dial("tcp", addr)
			if err != nil {
				log.Fatalf("failed to dial %s: %v", addr, err)
			}
			rawConn = rc
		}
	}
	defer rawConn.Close()

	pathPrefix := strings.TrimRight(u.Path, "/")
	if pathPrefix == "/" {
		pathPrefix = ""
	}
	path := fmt.Sprintf("%s/%s/%s/%s", pathPrefix, "process", procID, "attach")
	req := fmt.Sprintf("GET %s HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", path, u.Host)
	// For TLS, ensure we speak HTTP/1.1 and not attempt an HTTP/2 preface
	// by writing the raw bytes directly over the established connection.
	if _, err := rawConn.Write([]byte(req)); err != nil {
		log.Fatalf("failed to write attach request: %v", err)
	}

	// Read and consume HTTP response headers (until \r\n\r\n)
	br := bufio.NewReader(rawConn)
	if err := readHTTPHeaders(br); err != nil {
		log.Fatalf("failed to read attach response headers: %v", err)
	}

	// Put local terminal into raw mode
	var oldState *term.State
	if term.IsTerminal(int(os.Stdin.Fd())) {
		s, err := term.MakeRaw(int(os.Stdin.Fd()))
		if err != nil {
			log.Fatalf("failed to set raw mode: %v", err)
		}
		oldState = s
		defer func() {
			_ = term.Restore(int(os.Stdin.Fd()), oldState)
			fmt.Println()
		}()
	}

	// Handle window resize (SIGWINCH)
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	go func() {
		for range winch {
			if term.IsTerminal(int(os.Stdin.Fd())) {
				if w, h, err := term.GetSize(int(os.Stdin.Fd())); err == nil {
					// rows=h, cols=w
					rows := h
					cols := w
					// best-effort resize; do not cancel main ctx
					go func() {
						ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
						defer cancel()
						uid, _ := uuid.Parse(procID)
						_, _ = client.ProcessResizeWithResponse(ctx, openapi_types.UUID(uid), oapi.ProcessResizeJSONRequestBody{
							Rows: rows,
							Cols: cols,
						})
					}()
				}
			}
		}
	}()

	// Bi-directional piping
	errCh := make(chan error, 2)
	go func() {
		_, err := io.Copy(rawConn, os.Stdin)
		errCh <- err
	}()
	go func() {
		// Use the buffered reader to include any bytes already read beyond headers
		_, err := io.Copy(os.Stdout, br)
		errCh <- err
	}()

	// Wait for either side to close/error
	<-errCh
}

func ensureHTTPURL(s string) (*url.URL, error) {
	if !strings.HasPrefix(s, "http://") && !strings.HasPrefix(s, "https://") {
		s = "http://" + s
	}
	u, err := url.Parse(s)
	if err != nil {
		return nil, err
	}
	if u.Scheme == "" {
		u.Scheme = "http"
	}
	if u.Host == "" && u.Path != "" {
		// Allow bare host:port without scheme
		u.Host = u.Path
		u.Path = ""
	}
	return u, nil
}

func readHTTPHeaders(r *bufio.Reader) error {
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return err
		}
		if line == "\r\n" {
			return nil
		}
		// continue until empty line
	}
}

func boolPtr(b bool) *bool { return &b }
