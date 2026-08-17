package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gopkg.in/yaml.v3"
)

var version = "dev"

func main() {
	cfgPath := flag.String("c", "config.yaml", "config file")
	flag.Parse()

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	ips, err := loadIPs(resolvePath(*cfgPath, cfg.IPFile))
	if err != nil {
		log.Fatalf("load ip file: %v", err)
	}
	if len(ips) == 0 {
		log.Fatal("ip.txt is empty")
	}

	p := newProxy(cfg, newPool(ips, cfg.MaxFails, cfg.Cooldown))
	log.Printf("cdn-pool %s listen=%s host=%s ips=%d", version, cfg.Listen, cfg.CDNHost, len(ips))

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           p,
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       90 * time.Second,
	}
	log.Fatal(srv.ListenAndServe())
}

type config struct {
	Listen      string            `yaml:"listen"`
	CDNHost     string            `yaml:"cdn-host"`
	IPFile      string            `yaml:"ip-file"`
	MaxFails    int               `yaml:"max-fails"`
	Cooldown    time.Duration     `yaml:"cooldown"`
	DialTimeout time.Duration     `yaml:"dial-timeout"`
	Headers     map[string]string `yaml:"headers"`
}

func loadConfig(path string) (*config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg := &config{}
	if err := yaml.Unmarshal(b, cfg); err != nil {
		return nil, err
	}
	if cfg.Listen == "" {
		cfg.Listen = "127.0.0.1:18080"
	}
	if cfg.IPFile == "" {
		cfg.IPFile = "ip.txt"
	}
	if cfg.MaxFails <= 0 {
		cfg.MaxFails = 3
	}
	if cfg.Cooldown <= 0 {
		cfg.Cooldown = 30 * time.Second
	}
	if cfg.DialTimeout <= 0 {
		cfg.DialTimeout = 8 * time.Second
	}
	if cfg.Headers == nil {
		cfg.Headers = map[string]string{}
	}
	if cfg.CDNHost == "" {
		return nil, errors.New("cdn-host is required")
	}
	return cfg, nil
}

func resolvePath(cfgPath, ipFile string) string {
	if filepath.IsAbs(ipFile) {
		return ipFile
	}
	return filepath.Join(filepath.Dir(cfgPath), ipFile)
}

func loadIPs(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	seen := map[string]struct{}{}
	var ips []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		host, port, err := net.SplitHostPort(line)
		if err != nil {
			host, port = line, "443"
			line = net.JoinHostPort(host, port)
		}
		if net.ParseIP(host) == nil {
			log.Printf("skip invalid ip: %s", host)
			continue
		}
		if _, ok := seen[line]; ok {
			continue
		}
		seen[line] = struct{}{}
		ips = append(ips, line)
	}
	return ips, sc.Err()
}

type node struct {
	addr     string
	fails    atomic.Int32
	cooldown atomic.Int64
	tr       *http.Transport
}

type pool struct {
	nodes    []*node
	idx      atomic.Uint64
	maxFails int32
	cool     time.Duration
}

func newPool(addrs []string, maxFails int, cool time.Duration) *pool {
	p := &pool{maxFails: int32(maxFails), cool: cool, nodes: make([]*node, 0, len(addrs))}
	for _, a := range addrs {
		p.nodes = append(p.nodes, &node{addr: a})
	}
	return p
}

func (p *pool) next() *node {
	n := len(p.nodes)
	if n == 0 {
		return nil
	}
	now := time.Now().UnixNano()
	start := int(p.idx.Add(1) - 1)
	for i := 0; i < n; i++ {
		nd := p.nodes[(start+i)%n]
		if nd.cooldown.Load() > now {
			continue
		}
		return nd
	}
	return p.nodes[start%n]
}

func (p *pool) ok(nd *node) {
	nd.fails.Store(0)
	nd.cooldown.Store(0)
}

func (p *pool) fail(nd *node) {
	if nd.fails.Add(1) >= p.maxFails {
		nd.cooldown.Store(time.Now().Add(p.cool).UnixNano())
		nd.fails.Store(0)
		log.Printf("cooldown %s for %s", nd.addr, p.cool)
	}
}

type nodeStat struct {
	Addr     string `json:"addr"`
	Fails    int32  `json:"fails"`
	Cooldown string `json:"cooldown"`
}

type statsResp struct {
	CDNHost string     `json:"cdnHost"`
	Listen  string     `json:"listen"`
	Total   int        `json:"total"`
	Nodes   []nodeStat `json:"nodes"`
}

func (p *pool) stats() []nodeStat {
	now := time.Now().UnixNano()
	out := make([]nodeStat, len(p.nodes))
	for i, nd := range p.nodes {
		until := nd.cooldown.Load()
		left := time.Duration(0)
		if until > now {
			left = time.Duration(until - now)
		}
		out[i] = nodeStat{
			Addr:     nd.addr,
			Fails:    nd.fails.Load(),
			Cooldown: left.Round(time.Millisecond).String(),
		}
	}
	return out
}

type proxy struct {
	cfg    *config
	pool   *pool
	dialer *net.Dialer
}

func newProxy(cfg *config, pool *pool) *proxy {
	dialer := &net.Dialer{
		Timeout:   cfg.DialTimeout,
		KeepAlive: 30 * time.Second,
	}
	tlsCfg := &tls.Config{
		ServerName: cfg.CDNHost,
		MinVersion: tls.VersionTLS12,
		NextProtos: []string{"h2", "http/1.1"},
	}
	for _, nd := range pool.nodes {
		addr := nd.addr
		nd.tr = &http.Transport{
			TLSClientConfig:     tlsCfg,
			ForceAttemptHTTP2:   true,
			DisableCompression:  true,
			IdleConnTimeout:     90 * time.Second,
			MaxIdleConns:        8,
			MaxIdleConnsPerHost: 8,
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return dialer.DialContext(ctx, "tcp", addr)
			},
		}
	}
	return &proxy{cfg: cfg, pool: pool, dialer: dialer}
}

func (p *proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodConnect:
		p.handleCONNECT(w, r)
	case r.URL.Path == "/_pool/stats" && r.URL.Host == "":
		p.handleStats(w, r)
	default:
		p.handleHTTP(w, r)
	}
}

func (p *proxy) handleStats(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(statsResp{
		CDNHost: p.cfg.CDNHost,
		Listen:  p.cfg.Listen,
		Total:   len(p.pool.nodes),
		Nodes:   p.pool.stats(),
	})
}

func (p *proxy) handleHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Host == "" && r.Host == "" {
		http.Error(w, "not a proxy request (need absolute URL or Host)", http.StatusBadRequest)
		return
	}

	nd := p.pool.next()
	if nd == nil {
		http.Error(w, "empty pool", http.StatusBadGateway)
		return
	}

	u := *r.URL
	u.Scheme = "https"
	u.Host = p.cfg.CDNHost
	outReq := &http.Request{
		Method:        r.Method,
		URL:           &u,
		Proto:         r.Proto,
		ProtoMajor:    r.ProtoMajor,
		ProtoMinor:    r.ProtoMinor,
		Header:        r.Header.Clone(),
		Body:          r.Body,
		ContentLength: r.ContentLength,
		Host:          p.cfg.CDNHost,
	}
	if outReq.Header == nil {
		outReq.Header = make(http.Header)
	}
	applyHeaders(outReq.Header, p.cfg.Headers)
	stripHopHeaders(outReq.Header)
	if outReq.Body == http.NoBody {
		outReq.Body = nil
	}

	resp, err := nd.tr.RoundTrip(outReq.WithContext(r.Context()))
	if err != nil {
		p.pool.fail(nd)
		log.Printf("http %s via %s: %v", r.URL.Path, nd.addr, err)
		http.Error(w, "upstream: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	p.pool.ok(nd)

	copyHeader(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func (p *proxy) handleCONNECT(w http.ResponseWriter, r *http.Request) {
	nd := p.pool.next()
	if nd == nil {
		http.Error(w, "empty pool", http.StatusBadGateway)
		return
	}

	up, err := p.dialer.DialContext(r.Context(), "tcp", nd.addr)
	if err != nil {
		p.pool.fail(nd)
		http.Error(w, "dial: "+err.Error(), http.StatusBadGateway)
		return
	}

	hj, ok := w.(http.Hijacker)
	if !ok {
		up.Close()
		http.Error(w, "hijack not supported", http.StatusInternalServerError)
		return
	}
	client, bufrw, err := hj.Hijack()
	if err != nil {
		up.Close()
		http.Error(w, "hijack: "+err.Error(), http.StatusInternalServerError)
		return
	}

	if _, err := io.WriteString(client, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		up.Close()
		client.Close()
		return
	}
	if err := bufrw.Flush(); err != nil {
		up.Close()
		client.Close()
		return
	}

	p.pool.ok(nd)
	// Transparent TCP tunnel. Client does its own TLS; extra headers only
	// apply on the HTTP-proxy path.
	left := struct {
		io.Reader
		io.Writer
		io.Closer
	}{io.MultiReader(bufrw.Reader, client), client, client}
	pipe(left, up)
}

func applyHeaders(h http.Header, extra map[string]string) {
	for k, v := range extra {
		if v == "" {
			h.Del(k)
			continue
		}
		h.Set(k, v)
	}
}

var hopHeaders = []string{
	"Connection", "Proxy-Connection", "Keep-Alive",
	"Proxy-Authenticate", "Proxy-Authorization",
	"Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

func stripHopHeaders(h http.Header) {
	for _, k := range hopHeaders {
		h.Del(k)
	}
}

func copyHeader(dst, src http.Header) {
	for k, vs := range src {
		dst[k] = vs
	}
	stripHopHeaders(dst)
}

func pipe(a, b io.ReadWriteCloser) {
	var once sync.Once
	closeBoth := func() {
		once.Do(func() {
			a.Close()
			b.Close()
		})
	}
	cp := func(dst, src io.ReadWriteCloser) {
		_, _ = io.Copy(dst, src)
		if c, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = c.CloseWrite()
		}
		closeBoth()
	}
	go cp(a, b)
	cp(b, a)
}
