// Package discover finds Arduino Linux boards (UNO Q and its siblings) on the local network.
//
// The boards' avahi-daemon advertises an `_arduino._tcp` DNS-SD service named after the board's
// hostname, with TXT records such as board=unoq and serial_number=…. Browse sends an mDNS query
// from an ordinary UDP port on every multicast-capable IPv4 interface; responders answer such
// "legacy unicast" queries directly to that port (RFC 6762 §6.7), so nothing has to bind port 5353.
package discover

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/dns/dnsmessage"
	"golang.org/x/net/ipv4"
)

// Service is the DNS-SD service type the boards advertise.
const Service = "_arduino._tcp.local."

// LinuxBoards are the `board` TXT values of boards that run Arduino App CLI (Quay can manage them).
var LinuxBoards = map[string]string{"unoq": "Arduino UNO Q", "ventunoq": "Arduino VENTUNO Q"}

// Board is one board found on the network.
type Board struct {
	Name    string            `json:"name"`    // instance name: the board's hostname, e.g. "duck"
	Host    string            `json:"host"`    // mDNS host name, e.g. "duck.local"
	IPs     []string          `json:"ips"`     // IPv4 addresses
	Board   string            `json:"board"`   // TXT board=, e.g. "unoq"
	Model   string            `json:"model"`   // human name for Board, "" if unknown
	Serial  string            `json:"serial"`  // TXT serial_number=
	Version string            `json:"version"` // TXT distro_version=
	TXT     map[string]string `json:"txt"`
}

// Address is what to connect to: the mDNS name when there is one, else the first IP.
func (b Board) Address() string {
	if b.Host != "" {
		return strings.TrimSuffix(b.Host, ".")
	}
	if len(b.IPs) > 0 {
		return b.IPs[0]
	}
	return ""
}

var mdnsGroup = &net.UDPAddr{IP: net.IPv4(224, 0, 0, 251), Port: 5353}

// Browse looks for boards for up to timeout (about 2 s is plenty on a home network) and returns
// the Linux boards Quay can manage, sorted by name.
func Browse(ctx context.Context, timeout time.Duration) ([]Board, error) {
	query, err := Query(Service)
	if err != nil {
		return nil, err
	}
	c := newCollector()
	err = exchange(ctx, query, timeout, func(pkt []byte, from net.IP) bool {
		c.add(pkt, from)
		return false
	})
	if err != nil {
		return nil, err
	}
	return c.boards(), nil
}

// Find looks for one board, the first that match accepts, for up to timeout, and returns as soon
// as it answers; ok is false when no such board did.
func Find(ctx context.Context, timeout time.Duration, match func(Board) bool) (b Board, ok bool, err error) {
	query, err := Query(Service)
	if err != nil {
		return Board{}, false, err
	}
	c := newCollector()
	err = exchange(ctx, query, timeout, func(pkt []byte, from net.IP) bool {
		c.add(pkt, from)
		for _, x := range c.boards() {
			if match(x) {
				b, ok = x, true
				return true
			}
		}
		return false
	})
	return b, ok, err
}

// LookupHost resolves a NAME.local host name with an mDNS query, for systems whose resolver does
// not (Windows without Bonjour, minimal Linux, builds without cgo).
func LookupHost(ctx context.Context, host string, timeout time.Duration) ([]string, error) {
	fqdn := strings.TrimSuffix(host, ".") + "."
	name, err := dnsmessage.NewName(fqdn)
	if err != nil {
		return nil, err
	}
	msg := dnsmessage.Message{Questions: []dnsmessage.Question{{Name: name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}}}
	query, err := msg.Pack()
	if err != nil {
		return nil, err
	}
	var ips []string
	err = exchange(ctx, query, timeout, func(pkt []byte, _ net.IP) bool {
		var m dnsmessage.Message
		if m.Unpack(pkt) != nil || !m.Header.Response {
			return false
		}
		for _, r := range append(m.Answers, m.Additionals...) {
			if a, ok := r.Body.(*dnsmessage.AResource); ok && strings.EqualFold(r.Header.Name.String(), fqdn) {
				if ip := net.IP(a.A[:]).String(); !contains(ips, ip) {
					ips = append(ips, ip)
				}
			}
		}
		return len(ips) > 0
	})
	if err != nil {
		return nil, err
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("no answer for %s on the local network", host)
	}
	return ips, nil
}

// exchange multicasts query on every IPv4 interface (three times, the first packet on a sleeping
// Wi-Fi link is often lost) and passes each reply to handle until it returns true or timeout.
func exchange(ctx context.Context, query []byte, timeout time.Duration, handle func(pkt []byte, from net.IP) (done bool)) error {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{})
	if err != nil {
		return err
	}
	defer conn.Close()
	pc := ipv4.NewPacketConn(conn)
	_ = pc.SetMulticastTTL(255)
	_ = pc.SetMulticastLoopback(true)
	ifaces := multicastInterfaces()
	send := func() {
		if len(ifaces) == 0 {
			_, _ = conn.WriteToUDP(query, mdnsGroup)
			return
		}
		for i := range ifaces {
			if pc.SetMulticastInterface(&ifaces[i]) == nil {
				_, _ = pc.WriteTo(query, nil, mdnsGroup)
			}
		}
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	go func() {
		for _, d := range []time.Duration{0, 300 * time.Millisecond, 600 * time.Millisecond} {
			select {
			case <-ctx.Done():
				return
			case <-time.After(d):
				send()
			}
		}
	}()
	go func() { <-ctx.Done(); _ = conn.SetReadDeadline(time.Now()) }()

	buf := make([]byte, 9000)
	for {
		n, from, err := conn.ReadFromUDP(buf)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				return nil
			}
			return err
		}
		if handle(buf[:n], from.IP) {
			return nil
		}
	}
}

// Query builds an mDNS PTR question for a service type.
func Query(service string) ([]byte, error) {
	name, err := dnsmessage.NewName(service)
	if err != nil {
		return nil, err
	}
	msg := dnsmessage.Message{Questions: []dnsmessage.Question{{Name: name, Type: dnsmessage.TypePTR, Class: dnsmessage.ClassINET}}}
	return msg.Pack()
}

func multicastInterfaces() []net.Interface {
	all, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []net.Interface
	for _, ifc := range all {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagMulticast == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil {
				out = append(out, ifc)
				break
			}
		}
	}
	return out
}

// collector merges records from any number of responses (an answer may arrive split, or twice
// over several interfaces).
type collector struct {
	mu        sync.Mutex
	instances map[string]*instance // by full instance name, lower-case
	addrs     map[string][]string  // host name (lower-case) → IPv4s
}

type instance struct {
	name, host string
	txt        map[string]string
	from       []string
}

func newCollector() *collector {
	return &collector{instances: map[string]*instance{}, addrs: map[string][]string{}}
}

func (c *collector) inst(full string) *instance {
	key := strings.ToLower(full)
	in := c.instances[key]
	if in == nil {
		in = &instance{name: instanceLabel(full), txt: map[string]string{}}
		c.instances[key] = in
	}
	return in
}

// add parses one mDNS response. Malformed packets are ignored.
func (c *collector) add(pkt []byte, from net.IP) {
	var m dnsmessage.Message
	if err := m.Unpack(pkt); err != nil || !m.Header.Response {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	svc := strings.ToLower(Service)
	records := append(append(append([]dnsmessage.Resource{}, m.Answers...), m.Authorities...), m.Additionals...)
	var touched []*instance
	for _, r := range records {
		owner := r.Header.Name.String()
		switch b := r.Body.(type) {
		case *dnsmessage.PTRResource:
			if strings.ToLower(owner) == svc {
				touched = append(touched, c.inst(b.PTR.String()))
			}
		case *dnsmessage.SRVResource:
			if strings.HasSuffix(strings.ToLower(owner), "."+svc) {
				in := c.inst(owner)
				in.host = b.Target.String()
				touched = append(touched, in)
			}
		case *dnsmessage.TXTResource:
			if strings.HasSuffix(strings.ToLower(owner), "."+svc) {
				in := c.inst(owner)
				for _, kv := range b.TXT {
					k, v, _ := strings.Cut(kv, "=")
					in.txt[strings.ToLower(k)] = v
				}
				touched = append(touched, in)
			}
		case *dnsmessage.AResource:
			ip := net.IP(b.A[:]).String()
			host := strings.ToLower(owner)
			if !contains(c.addrs[host], ip) {
				c.addrs[host] = append(c.addrs[host], ip)
			}
		}
	}
	if from4 := from.To4(); from4 != nil {
		for _, in := range touched {
			if !contains(in.from, from4.String()) {
				in.from = append(in.from, from4.String())
			}
		}
	}
}

func (c *collector) boards() []Board {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []Board
	for _, in := range c.instances {
		kind := in.txt["board"]
		model, ok := LinuxBoards[kind]
		if !ok {
			continue // an Arduino board without Linux (e.g. an OTA-capable ESP32)
		}
		b := Board{Name: in.name, Host: strings.TrimSuffix(in.host, "."), Board: kind, Model: model,
			Serial: in.txt["serial_number"], Version: in.txt["distro_version"], TXT: in.txt}
		b.IPs = append(b.IPs, c.addrs[strings.ToLower(in.host)]...)
		for _, ip := range in.from {
			if !contains(b.IPs, ip) {
				b.IPs = append(b.IPs, ip)
			}
		}
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	return out
}

// instanceLabel turns `duck._arduino._tcp.local.` into `duck` (DNS-SD instance names may contain
// escaped dots and spaces).
func instanceLabel(full string) string {
	l := full
	if i := strings.Index(strings.ToLower(l), "."+strings.ToLower(Service)); i >= 0 {
		l = l[:i]
	}
	r := strings.NewReplacer(`\.`, ".", `\ `, " ", `\\`, `\`)
	return r.Replace(l)
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
