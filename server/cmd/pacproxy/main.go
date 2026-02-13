package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

const (
	defaultListenAddr = "127.0.0.1:15080"
	defaultPACPath    = "/chromium/proxy.pac"
	defaultPACTester  = "pactester"
)

type routeType int

const (
	routeDirect routeType = iota
	routeHTTPProxy
)

type routeDecision struct {
	typ       routeType
	proxyAddr string
}

type pacResolver struct {
	mu         sync.RWMutex
	pacPath    string
	pacTester  string
	lastPACSHA string
	cache      map[string]routeDecision
}

func newPACResolver(pacPath, pacTester string) *pacResolver {
	return &pacResolver{
		pacPath:   pacPath,
		pacTester: pacTester,
		cache:     make(map[string]routeDecision),
	}
}

func (r *pacResolver) resolve(rawURL string) routeDecision {
	u, err := url.Parse(rawURL)
	if err != nil {
		return routeDecision{typ: routeDirect}
	}

	if !r.pacAvailable() {
		return routeDecision{typ: routeDirect}
	}

	key := strings.ToLower(u.Scheme + "://" + u.Host)

	r.mu.RLock()
	if cached, ok := r.cache[key]; ok {
		r.mu.RUnlock()
		return cached
	}
	r.mu.RUnlock()

	decision := r.evalPAC(rawURL)

	r.mu.Lock()
	r.cache[key] = decision
	r.mu.Unlock()
	return decision
}

func (r *pacResolver) pacAvailable() bool {
	data, err := os.ReadFile(r.pacPath)
	if err != nil {
		r.invalidateCacheIfNeeded("")
		return false
	}
	sum := sha256.Sum256(data)
	r.invalidateCacheIfNeeded(hex.EncodeToString(sum[:]))
	return true
}

func (r *pacResolver) invalidateCacheIfNeeded(pacSHA string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if pacSHA != r.lastPACSHA {
		r.lastPACSHA = pacSHA
		r.cache = make(map[string]routeDecision)
	}
}

func (r *pacResolver) evalPAC(rawURL string) routeDecision {
	cmd := exec.Command(r.pacTester, "-p", r.pacPath, "-u", rawURL)
	out, err := cmd.Output()
	if err != nil {
		log.Printf("pactester failed for %q: %v", rawURL, err)
		return routeDecision{typ: routeDirect}
	}

	line := strings.TrimSpace(string(out))
	if line == "" {
		return routeDecision{typ: routeDirect}
	}

	for _, rawToken := range strings.Split(line, ";") {
		token := strings.TrimSpace(rawToken)
		if token == "" {
			continue
		}
		up := strings.ToUpper(token)
		switch {
		case up == "DIRECT":
			return routeDecision{typ: routeDirect}
		case strings.HasPrefix(up, "PROXY "), strings.HasPrefix(up, "HTTP "), strings.HasPrefix(up, "HTTPS "):
			parts := strings.Fields(token)
			if len(parts) < 2 {
				continue
			}
			proxyAddr := strings.TrimSpace(parts[1])
			if _, _, err := net.SplitHostPort(proxyAddr); err != nil {
				continue
			}
			return routeDecision{typ: routeHTTPProxy, proxyAddr: proxyAddr}
		default:
			continue
		}
	}

	return routeDecision{typ: routeDirect}
}

func main() {
	listenAddr := flag.String("listen", defaultListenAddr, "listen address")
	pacPath := flag.String("pac", defaultPACPath, "path to PAC file")
	pacTester := flag.String("pactester", defaultPACTester, "path to pactester executable")
	flag.Parse()

	if _, err := exec.LookPath(*pacTester); err != nil {
		log.Fatalf("pactester not found: %v", err)
	}

	resolver := newPACResolver(*pacPath, *pacTester)

	ln, err := net.Listen("tcp", *listenAddr)
	if err != nil {
		log.Fatalf("listen failed on %s: %v", *listenAddr, err)
	}
	defer ln.Close()

	log.Printf("pac proxy listening on %s (pac=%s)", *listenAddr, *pacPath)

	for {
		conn, err := ln.Accept()
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Temporary() {
				continue
			}
			log.Printf("accept failed: %v", err)
			continue
		}
		go handleConn(conn, resolver)
	}
}

func handleConn(conn net.Conn, resolver *pacResolver) {
	defer conn.Close()

	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return
	}

	reader := bufio.NewReaderSize(conn, 32*1024)
	peek, err := reader.Peek(1)
	if err != nil {
		return
	}

	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		return
	}

	if len(peek) == 1 && peek[0] == 0x16 {
		handleTLS(conn, reader, resolver)
		return
	}
	handleHTTP(conn, reader, resolver)
}

func handleHTTP(client net.Conn, reader *bufio.Reader, resolver *pacResolver) {
	req, err := http.ReadRequest(reader)
	if err != nil {
		return
	}
	defer req.Body.Close()

	hostPort, hostOnly, err := normalizeHostPort(req.Host, "80")
	if err != nil {
		return
	}

	targetURL := requestURL(req, "http", hostOnly)
	decision := resolver.resolve(targetURL)

	var upstream net.Conn
	if decision.typ == routeHTTPProxy {
		upstream, err = net.DialTimeout("tcp", decision.proxyAddr, 10*time.Second)
	} else {
		upstream, err = net.DialTimeout("tcp", hostPort, 10*time.Second)
	}
	if err != nil {
		return
	}
	defer upstream.Close()

	req.Header.Del("Proxy-Connection")
	req.Close = true
	req.Header.Set("Connection", "close")

	if decision.typ == routeHTTPProxy {
		if !req.URL.IsAbs() {
			req.URL.Scheme = "http"
			req.URL.Host = hostOnly
		}
		err = req.WriteProxy(upstream)
	} else {
		req.URL.Scheme = ""
		req.URL.Host = ""
		req.RequestURI = req.URL.RequestURI()
		err = req.Write(upstream)
	}
	if err != nil {
		return
	}

	_, _ = io.Copy(client, upstream)
}

func handleTLS(client net.Conn, reader *bufio.Reader, resolver *pacResolver) {
	initial, sniHost, err := readTLSClientHello(reader)
	if err != nil {
		return
	}
	if sniHost == "" {
		return
	}

	targetHostPort := net.JoinHostPort(sniHost, "443")
	targetURL := "https://" + sniHost + "/"
	decision := resolver.resolve(targetURL)

	if decision.typ == routeHTTPProxy {
		handleTLSViaHTTPProxy(client, reader, initial, targetHostPort, decision.proxyAddr)
		return
	}
	handleTLSDirect(client, reader, initial, targetHostPort)
}

func handleTLSViaHTTPProxy(client net.Conn, clientReader *bufio.Reader, initial []byte, targetHostPort, proxyAddr string) {
	upstream, err := net.DialTimeout("tcp", proxyAddr, 10*time.Second)
	if err != nil {
		return
	}
	defer upstream.Close()

	proxyReader := bufio.NewReader(upstream)
	connectReq := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\nProxy-Connection: Keep-Alive\r\n\r\n", targetHostPort, targetHostPort)
	if _, err := io.WriteString(upstream, connectReq); err != nil {
		return
	}

	resp, err := http.ReadResponse(proxyReader, &http.Request{Method: http.MethodConnect})
	if err != nil {
		return
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if resp.Body != nil {
			resp.Body.Close()
		}
		return
	}
	// For CONNECT 2xx, switch to raw tunnel immediately.
	// Draining resp.Body here can block indefinitely and stall the TLS handshake.
	if resp.Body != nil {
		resp.Body.Close()
	}

	_ = tunnel(client, io.MultiReader(proxyReader, upstream), upstream, clientReader, initial)
}

func handleTLSDirect(client net.Conn, clientReader *bufio.Reader, initial []byte, targetHostPort string) {
	upstream, err := net.DialTimeout("tcp", targetHostPort, 10*time.Second)
	if err != nil {
		return
	}
	defer upstream.Close()

	_ = tunnel(client, upstream, upstream, clientReader, initial)
}

func tunnel(client net.Conn, upstreamRead io.Reader, upstreamWrite net.Conn, clientReader *bufio.Reader, initial []byte) error {
	if _, err := upstreamWrite.Write(initial); err != nil {
		return err
	}

	errCh := make(chan error, 2)
	go func() {
		_, err := io.Copy(upstreamWrite, io.MultiReader(clientReader, client))
		if cw, ok := upstreamWrite.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
		errCh <- err
	}()

	go func() {
		_, err := io.Copy(client, upstreamRead)
		if cw, ok := client.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
		errCh <- err
	}()

	<-errCh
	<-errCh
	return nil
}

func normalizeHostPort(host, defaultPort string) (hostPort string, hostOnly string, err error) {
	host = strings.TrimSpace(host)
	if host == "" {
		return "", "", errors.New("host is empty")
	}

	if strings.Contains(host, ":") {
		h, p, splitErr := net.SplitHostPort(host)
		if splitErr == nil {
			if h == "" {
				return "", "", errors.New("host is empty")
			}
			if p == "" {
				p = defaultPort
			}
			return net.JoinHostPort(h, p), h, nil
		}

		if strings.Count(host, ":") > 1 && !strings.HasPrefix(host, "[") {
			return net.JoinHostPort(host, defaultPort), host, nil
		}
	}

	return net.JoinHostPort(host, defaultPort), host, nil
}

func requestURL(req *http.Request, scheme, host string) string {
	if req.URL != nil && req.URL.IsAbs() {
		return req.URL.String()
	}
	if req.URL == nil {
		return scheme + "://" + host + "/"
	}
	u := &url.URL{
		Scheme:   scheme,
		Host:     host,
		Path:     req.URL.Path,
		RawPath:  req.URL.RawPath,
		RawQuery: req.URL.RawQuery,
	}
	if u.Path == "" {
		u.Path = "/"
	}
	return u.String()
}

func readTLSClientHello(reader *bufio.Reader) ([]byte, string, error) {
	header := make([]byte, 5)
	if _, err := io.ReadFull(reader, header); err != nil {
		return nil, "", err
	}
	if header[0] != 0x16 {
		return nil, "", errors.New("not a TLS handshake record")
	}

	recLen := int(binary.BigEndian.Uint16(header[3:5]))
	if recLen <= 0 || recLen > 64*1024 {
		return nil, "", errors.New("invalid TLS record length")
	}

	body := make([]byte, recLen)
	if _, err := io.ReadFull(reader, body); err != nil {
		return nil, "", err
	}

	sni, err := extractSNIFromClientHello(body)
	if err != nil {
		return nil, "", err
	}

	raw := make([]byte, 0, len(header)+len(body))
	raw = append(raw, header...)
	raw = append(raw, body...)
	return raw, sni, nil
}

func extractSNIFromClientHello(recordBody []byte) (string, error) {
	if len(recordBody) < 42 {
		return "", errors.New("short client hello")
	}
	if recordBody[0] != 0x01 {
		return "", errors.New("not client hello")
	}

	hsLen := int(recordBody[1])<<16 | int(recordBody[2])<<8 | int(recordBody[3])
	if hsLen+4 > len(recordBody) {
		return "", errors.New("invalid client hello size")
	}
	p := 4

	p += 2  // client version
	p += 32 // random
	if p >= len(recordBody) {
		return "", errors.New("malformed client hello")
	}

	sessionLen := int(recordBody[p])
	p++
	p += sessionLen
	if p+2 > len(recordBody) {
		return "", errors.New("malformed cipher suites")
	}

	cipherLen := int(binary.BigEndian.Uint16(recordBody[p : p+2]))
	p += 2 + cipherLen
	if p >= len(recordBody) {
		return "", errors.New("malformed compression methods")
	}

	compLen := int(recordBody[p])
	p++
	p += compLen
	if p+2 > len(recordBody) {
		return "", errors.New("missing extensions")
	}

	extLen := int(binary.BigEndian.Uint16(recordBody[p : p+2]))
	p += 2
	if p+extLen > len(recordBody) {
		return "", errors.New("invalid extensions length")
	}

	extData := recordBody[p : p+extLen]
	off := 0
	for off+4 <= len(extData) {
		extType := binary.BigEndian.Uint16(extData[off : off+2])
		extSize := int(binary.BigEndian.Uint16(extData[off+2 : off+4]))
		off += 4
		if off+extSize > len(extData) {
			break
		}
		if extType == 0x0000 { // server_name
			serverNames := extData[off : off+extSize]
			if len(serverNames) < 2 {
				break
			}
			nameListLen := int(binary.BigEndian.Uint16(serverNames[:2]))
			if 2+nameListLen > len(serverNames) {
				break
			}
			n := 2
			for n+3 <= 2+nameListLen {
				nameType := serverNames[n]
				nameLen := int(binary.BigEndian.Uint16(serverNames[n+1 : n+3]))
				n += 3
				if n+nameLen > len(serverNames) {
					break
				}
				if nameType == 0 {
					host := strings.TrimSpace(string(serverNames[n : n+nameLen]))
					if host != "" {
						return host, nil
					}
				}
				n += nameLen
			}
			break
		}
		off += extSize
	}

	return "", errors.New("sni not found")
}
