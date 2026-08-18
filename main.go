package main

import (
	"bufio"
	"encoding/binary"
	"errors"
	"flag"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/miekg/dns"
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
	cidrs, err := loadCIDRs(resolvePath(*cfgPath, cfg.CIDRFile))
	if err != nil {
		log.Fatalf("load cidr file: %v", err)
	}
	if len(cidrs) == 0 {
		log.Fatal("cidr.txt is empty")
	}

	log.Printf("cdn-pool %s socks5=%s hosts=%d cidrs=%d", version, cfg.Listen, len(cfg.Hosts), len(cidrs))
	log.Printf("cname=%s dns=%s", cfg.CNAME, cfg.DNS)
	ips, err := discoverIPs(cfg.DNS, cfg.CNAME, cidrs)
	if err != nil {
		log.Fatalf("first refresh: %v", err)
	}
	s := &server{
		pool:   newPool(ips, cfg.MaxFails, cfg.Cooldown),
		hosts:  newHostSet(cfg.Hosts),
		dialer: &net.Dialer{Timeout: cfg.DialTimeout, KeepAlive: 30 * time.Second},
	}
	log.Printf("refresh: %d ips from %d cidrs", len(ips), len(cidrs))
	go refreshLoop(cfg, cidrs, s.pool)

	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		log.Fatal(err)
	}
	for {
		c, err := ln.Accept()
		if err != nil {
			log.Printf("accept: %v", err)
			continue
		}
		go s.serve(c)
	}
}

type config struct {
	Listen      string        `yaml:"listen"`
	CIDRFile    string        `yaml:"cidr-file"`
	Hosts       []string      `yaml:"hosts"`
	CNAME       string        `yaml:"cname"`
	DNS         string        `yaml:"dns"`
	Refresh     time.Duration `yaml:"refresh"`
	MaxFails    int           `yaml:"max-fails"`
	Cooldown    time.Duration `yaml:"cooldown"`
	DialTimeout time.Duration `yaml:"dial-timeout"`
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
		cfg.Listen = "0.0.0.0:1080"
	}
	if cfg.CIDRFile == "" {
		cfg.CIDRFile = "cidr.txt"
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
	if cfg.Refresh <= 0 {
		cfg.Refresh = 10 * time.Minute
	}
	if cfg.DNS == "" {
		cfg.DNS = "223.5.5.5:53"
	} else if _, _, err := net.SplitHostPort(cfg.DNS); err != nil {
		cfg.DNS = net.JoinHostPort(cfg.DNS, "53")
	}
	if len(cfg.Hosts) == 0 {
		return nil, errors.New("hosts is required (domains that use the IP pool)")
	}
	if cfg.CNAME == "" {
		return nil, errors.New("cname is required")
	}
	return cfg, nil
}

func resolvePath(cfgPath, file string) string {
	if filepath.IsAbs(file) {
		return file
	}
	return filepath.Join(filepath.Dir(cfgPath), file)
}

func loadCIDRs(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	seen := map[string]struct{}{}
	var cidrs []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if !strings.Contains(line, "/") {
			line += "/24"
		}
		ip, _, err := net.ParseCIDR(line)
		if err != nil {
			log.Printf("skip invalid cidr: %s", line)
			continue
		}
		n := ip.To4()
		if n == nil {
			log.Printf("skip invalid cidr: %s", line)
			continue
		}
		n[3] = 0
		key := n.String()
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		cidrs = append(cidrs, key)
	}
	return cidrs, sc.Err()
}

type suffixRule struct {
	dot  string // ".example.com"
	apex string // "example.com"
}

type hostSet struct {
	exact   map[string]struct{}
	suffix  []suffixRule
	matchIP bool
}

func newHostSet(hosts []string) *hostSet {
	h := &hostSet{exact: map[string]struct{}{}}
	for _, raw := range hosts {
		n := strings.ToLower(strings.TrimSpace(raw))
		n = strings.TrimSuffix(n, ".")
		if n == "" {
			continue
		}
		if n == "*" {
			h.matchIP = true
			continue
		}
		if strings.HasPrefix(n, "*.") {
			dot := n[1:]
			h.suffix = append(h.suffix, suffixRule{dot: dot, apex: strings.TrimPrefix(dot, ".")})
			continue
		}
		h.exact[n] = struct{}{}
	}
	return h
}

func (h *hostSet) match(name string) bool {
	if _, ok := h.exact[name]; ok {
		return true
	}
	for _, suf := range h.suffix {
		if name == suf.apex || strings.HasSuffix(name, suf.dot) {
			return true
		}
	}
	return false
}

type node struct {
	ip       string
	fails    atomic.Int32
	cooldown atomic.Int64
}

type pool struct {
	mu       sync.RWMutex
	nodes    []*node
	idx      atomic.Uint64
	maxFails int32
	cool     time.Duration
}

func newPool(ips []string, maxFails int, cool time.Duration) *pool {
	p := &pool{maxFails: int32(maxFails), cool: cool}
	p.replace(ips)
	return p
}

func (p *pool) next() *node {
	p.mu.RLock()
	nodes := p.nodes
	p.mu.RUnlock()
	n := len(nodes)
	if n == 0 {
		return nil
	}
	now := time.Now().UnixNano()
	start := int(p.idx.Add(1) - 1)
	for i := 0; i < n; i++ {
		nd := nodes[(start+i)%n]
		if nd.cooldown.Load() > now {
			continue
		}
		return nd
	}
	return nodes[start%n]
}

func (p *pool) ok(nd *node) {
	nd.fails.Store(0)
	nd.cooldown.Store(0)
}

func (p *pool) fail(nd *node) {
	if nd.fails.Add(1) >= p.maxFails {
		nd.cooldown.Store(time.Now().Add(p.cool).UnixNano())
		nd.fails.Store(0)
		log.Printf("cooldown %s for %s", nd.ip, p.cool)
	}
}

func (p *pool) replace(ips []string) {
	nodes := make([]*node, len(ips))
	for i, ip := range ips {
		nodes[i] = &node{ip: ip}
	}
	p.mu.Lock()
	p.nodes = nodes
	p.idx.Store(0)
	p.mu.Unlock()
}

type server struct {
	pool   *pool
	hosts  *hostSet
	dialer *net.Dialer
}

func (s *server) serve(c net.Conn) {
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(15 * time.Second))

	host, port, err := socks5Handshake(c)
	if err != nil {
		return
	}

	up, err := s.dial(host, port)
	if err != nil {
		_ = socks5Reply(c, socks5RepHostUnreachable)
		return
	}
	if err := socks5Reply(c, socks5RepOK); err != nil {
		up.Close()
		return
	}
	_ = c.SetDeadline(time.Time{})
	pipe(c, up)
}

func (s *server) dial(host, port string) (net.Conn, error) {
	if net.ParseIP(host) != nil {
		if s.hosts.matchIP {
			return s.dialPool(port)
		}
		return s.dialer.Dial("tcp", net.JoinHostPort(host, port))
	}
	if s.hosts.match(host) {
		return s.dialPool(port)
	}
	return s.dialer.Dial("tcp", net.JoinHostPort(host, port))
}

func (s *server) dialPool(port string) (net.Conn, error) {
	nd := s.pool.next()
	if nd == nil {
		return nil, errors.New("empty pool")
	}
	c, err := s.dialer.Dial("tcp", net.JoinHostPort(nd.ip, port))
	if err != nil {
		s.pool.fail(nd)
		return nil, err
	}
	s.pool.ok(nd)
	return c, nil
}

const (
	socks5Ver                = 0x05
	socks5CmdConnect         = 0x01
	socks5AtypIPv4           = 0x01
	socks5AtypDomain         = 0x03
	socks5AtypIPv6           = 0x04
	socks5RepOK              = 0x00
	socks5RepCommand         = 0x07
	socks5RepAtyp            = 0x08
	socks5RepHostUnreachable = 0x04
)

func socks5Handshake(c net.Conn) (host, port string, err error) {
	var hdr [2]byte
	if _, err = io.ReadFull(c, hdr[:]); err != nil {
		return "", "", err
	}
	if hdr[0] != socks5Ver {
		return "", "", errors.New("not socks5")
	}
	var methods [255]byte
	if _, err = io.ReadFull(c, methods[:int(hdr[1])]); err != nil {
		return "", "", err
	}
	ack := [2]byte{socks5Ver, 0x00}
	if _, err = c.Write(ack[:]); err != nil {
		return "", "", err
	}

	var req [4]byte
	if _, err = io.ReadFull(c, req[:]); err != nil {
		return "", "", err
	}
	if req[0] != socks5Ver {
		return "", "", errors.New("bad ver")
	}
	if req[1] != socks5CmdConnect {
		_ = socks5Reply(c, socks5RepCommand)
		return "", "", errors.New("only CONNECT")
	}

	switch req[3] {
	case socks5AtypIPv4:
		var a [4]byte
		if _, err = io.ReadFull(c, a[:]); err != nil {
			return "", "", err
		}
		host = net.IP(a[:]).String()
	case socks5AtypIPv6:
		var a [16]byte
		if _, err = io.ReadFull(c, a[:]); err != nil {
			return "", "", err
		}
		host = net.IP(a[:]).String()
	case socks5AtypDomain:
		var lb [1]byte
		if _, err = io.ReadFull(c, lb[:]); err != nil {
			return "", "", err
		}
		var name [255]byte
		n := int(lb[0])
		if _, err = io.ReadFull(c, name[:n]); err != nil {
			return "", "", err
		}
		for i := 0; i < n; i++ {
			if name[i] >= 'A' && name[i] <= 'Z' {
				name[i] += 'a' - 'A'
			}
		}
		host = string(name[:n])
	default:
		_ = socks5Reply(c, socks5RepAtyp)
		return "", "", errors.New("bad atyp")
	}
	var pb [2]byte
	if _, err = io.ReadFull(c, pb[:]); err != nil {
		return "", "", err
	}
	return host, strconv.Itoa(int(binary.BigEndian.Uint16(pb[:]))), nil
}

func socks5Reply(c net.Conn, rep byte) error {
	buf := [10]byte{socks5Ver, rep, 0, socks5AtypIPv4}
	_, err := c.Write(buf[:])
	return err
}

func pipe(a, b net.Conn) {
	defer a.Close()
	defer b.Close()
	go func() {
		_, _ = io.Copy(a, b)
		a.Close()
	}()
	_, _ = io.Copy(b, a)
}

func refreshLoop(cfg *config, cidrs []string, p *pool) {
	t := time.NewTicker(cfg.Refresh)
	defer t.Stop()
	for range t.C {
		if err := refreshOnce(cfg, cidrs, p); err != nil {
			log.Printf("refresh: %v", err)
		}
	}
}

func refreshOnce(cfg *config, cidrs []string, p *pool) error {
	ips, err := discoverIPs(cfg.DNS, cfg.CNAME, cidrs)
	if err != nil {
		return err
	}
	p.replace(ips)
	log.Printf("refresh: %d ips from %d cidrs", len(ips), len(cidrs))
	return nil
}

func discoverIPs(server, name string, cidrs []string) ([]string, error) {
	cli := &dns.Client{Timeout: 5 * time.Second, UDPSize: 1232}
	sem := make(chan struct{}, 16)
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		seen = map[string]struct{}{}
		out  []string
	)
	for _, subnet := range cidrs {
		wg.Add(1)
		sem <- struct{}{}
		go func(subnet string) {
			defer wg.Done()
			defer func() { <-sem }()
			ips, err := lookupAWithECS(cli, server, name, subnet)
			if err != nil {
				log.Printf("refresh %s: %v", subnet, err)
				return
			}
			mu.Lock()
			for _, ip := range ips {
				if _, dup := seen[ip]; dup {
					continue
				}
				seen[ip] = struct{}{}
				out = append(out, ip)
			}
			mu.Unlock()
		}(subnet)
	}
	wg.Wait()
	if len(out) == 0 {
		return nil, errors.New("all ecs queries failed")
	}
	return out, nil
}

func lookupAWithECS(c *dns.Client, server, name, ecsIP string) ([]string, error) {
	ip := net.ParseIP(ecsIP).To4()
	if ip == nil {
		return nil, errors.New("bad ecs ip")
	}
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(name), dns.TypeA)
	m.SetEdns0(1232, false)
	m.IsEdns0().Option = append(m.IsEdns0().Option, &dns.EDNS0_SUBNET{
		Code:          dns.EDNS0SUBNET,
		Family:        1,
		SourceNetmask: 24,
		Address:       ip,
	})

	r, _, err := c.Exchange(m, server)
	if err != nil {
		return nil, err
	}
	if r.Truncated {
		tcp := *c
		tcp.Net = "tcp"
		r, _, err = tcp.Exchange(m, server)
		if err != nil {
			return nil, err
		}
	}
	var ips []string
	for _, rr := range r.Answer {
		if a, ok := rr.(*dns.A); ok {
			ips = append(ips, a.A.String())
		}
	}
	return ips, nil
}
