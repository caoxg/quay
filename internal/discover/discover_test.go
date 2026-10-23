package discover

import (
	"net"
	"testing"

	"golang.org/x/net/dns/dnsmessage"
)

// response builds an mDNS answer like the one an UNO Q's avahi-daemon sends.
func response(t *testing.T, instance, host string, ip [4]byte, txt []string, withAddr bool) []byte {
	t.Helper()
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{Response: true, Authoritative: true})
	b.EnableCompression()
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(b.StartAnswers())
	svc := dnsmessage.MustNewName(Service)
	full := dnsmessage.MustNewName(instance + "." + Service)
	target := dnsmessage.MustNewName(host)
	hdr := func(n dnsmessage.Name) dnsmessage.ResourceHeader {
		return dnsmessage.ResourceHeader{Name: n, Class: dnsmessage.ClassINET, TTL: 120}
	}
	must(b.PTRResource(hdr(svc), dnsmessage.PTRResource{PTR: full}))
	must(b.StartAdditionals())
	must(b.SRVResource(hdr(full), dnsmessage.SRVResource{Port: 80, Target: target}))
	must(b.TXTResource(hdr(full), dnsmessage.TXTResource{TXT: txt}))
	if withAddr {
		must(b.AResource(hdr(target), dnsmessage.AResource{A: ip}))
	}
	pkt, err := b.Finish()
	must(err)
	return pkt
}

func TestCollector(t *testing.T) {
	c := newCollector()
	unoq := []string{"board=unoq", "vid=0x2341", "pid=0x0078", "distro_version=0.1", "serial_number=2c3e91a7"}
	c.add(response(t, "duck", "duck.local.", [4]byte{192, 168, 1, 42}, unoq, true), net.IPv4(192, 168, 1, 42))
	// The same answer again over a second interface must not duplicate anything.
	c.add(response(t, "duck", "duck.local.", [4]byte{192, 168, 1, 42}, unoq, true), net.IPv4(192, 168, 1, 42))
	// A board whose answer has no address record: the sender's address is used.
	c.add(response(t, "lab bench", "bench.local.", [4]byte{}, []string{"board=ventunoq", "serial_number=42"}, false), net.IPv4(10, 0, 0, 7))
	// An OTA-capable microcontroller also advertises _arduino._tcp, but it is not a Linux board.
	c.add(response(t, "esp32-1", "esp32-1.local.", [4]byte{10, 0, 0, 9}, []string{"board=esp32"}, true), net.IPv4(10, 0, 0, 9))
	c.add([]byte("not dns"), net.IPv4(10, 0, 0, 10))

	got := c.boards()
	if len(got) != 2 {
		t.Fatalf("want 2 Linux boards, got %+v", got)
	}
	duck, bench := got[1], got[0] // sorted by name: "duck" < "lab bench"
	if bench.Name != "lab bench" {
		duck, bench = got[0], got[1]
	}
	if duck.Name != "duck" || duck.Host != "duck.local" || duck.Serial != "2c3e91a7" || duck.Model != "Arduino UNO Q" ||
		duck.Version != "0.1" || len(duck.IPs) != 1 || duck.IPs[0] != "192.168.1.42" || duck.Address() != "duck.local" {
		t.Errorf("duck: %+v", duck)
	}
	if bench.Name != "lab bench" || bench.Model != "Arduino VENTUNO Q" || len(bench.IPs) != 1 || bench.IPs[0] != "10.0.0.7" {
		t.Errorf("bench: %+v", bench)
	}
}

func TestQuery(t *testing.T) {
	pkt, err := Query(Service)
	if err != nil {
		t.Fatal(err)
	}
	var m dnsmessage.Message
	if err := m.Unpack(pkt); err != nil || len(m.Questions) != 1 || m.Questions[0].Type != dnsmessage.TypePTR || m.Questions[0].Name.String() != Service {
		t.Fatalf("query: %+v %v", m.Questions, err)
	}
}

func TestInstanceLabel(t *testing.T) {
	for in, want := range map[string]string{
		"duck._arduino._tcp.local.":      "duck",
		`My\ Board._arduino._tcp.local.`: "My Board",
		`v1\.2._arduino._TCP.local.`:     "v1.2",
	} {
		if got := instanceLabel(in); got != want {
			t.Errorf("instanceLabel(%q) = %q, want %q", in, got, want)
		}
	}
}
