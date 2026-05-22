// DHT bootstrap that resolves node addresses without trusting the system DNS
// resolver, which some networks hijack for BitTorrent-related domains.
package engine

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/anacrolix/dht/v2"
)

// bootstrapHostPorts are the standard Mainline DHT bootstrap nodes. The DHT
// cannot find peers until it reaches one of these to seed its routing table.
var bootstrapHostPorts = []string{
	"router.utorrent.com:6881",
	"router.bittorrent.com:6881",
	"dht.transmissionbt.com:6881",
	"dht.aelitis.com:6881",
	"dht.libtorrent.org:25401",
}

// bootstrapFallbackIPs are known-good IP:port pairs for the hosts above,
// resolved over DoH on 2026-05-22. They let the DHT bootstrap even when both
// the system DNS and DoH are unavailable.
var bootstrapFallbackIPs = []string{
	"67.215.246.10:6881",    // router.bittorrent.com
	"82.221.103.244:6881",   // router.utorrent.com
	"87.98.162.88:6881",     // dht.transmissionbt.com
	"212.129.33.59:6881",    // dht.transmissionbt.com
	"34.203.221.232:6881",   // dht.aelitis.com
	"185.157.221.247:25401", // dht.libtorrent.org
}

// dohEndpoints are DNS-over-HTTPS resolvers. DoH runs over TLS, so an in-path
// resolver cannot forge answers the way it can for plaintext DNS.
var dohEndpoints = []string{
	"https://1.1.1.1/dns-query",
	"https://dns.google/resolve",
}

var (
	bootstrapOnce  sync.Once
	bootstrapAddrs []dht.Addr
)

// bootstrapNodes returns DHT bootstrap addresses, resolved over DoH with a
// hardcoded IP fallback. The result is computed once and cached.
func bootstrapNodes() []dht.Addr {
	bootstrapOnce.Do(func() {
		bootstrapAddrs = resolveBootstrapNodes(http.DefaultClient)
	})
	return bootstrapAddrs
}

// resolveBootstrapNodes resolves the bootstrap hosts over DoH, falling back to
// the hardcoded IPs when DoH yields nothing. It always returns a non-empty
// slice. The HTTP client is a parameter so tests can inject one.
func resolveBootstrapNodes(client *http.Client) []dht.Addr {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	var addrs []dht.Addr
	for _, hp := range bootstrapHostPorts {
		host, port, err := net.SplitHostPort(hp)
		if err != nil {
			continue
		}
		for _, ip := range dohLookup(ctx, client, host) {
			if a := udpAddr(ip, port); a != nil {
				addrs = append(addrs, dht.NewAddr(a))
			}
		}
	}
	if len(addrs) > 0 {
		return addrs
	}
	return fallbackNodes()
}

// fallbackNodes builds dht.Addrs from the hardcoded bootstrap IPs.
func fallbackNodes() []dht.Addr {
	var addrs []dht.Addr
	for _, ipPort := range bootstrapFallbackIPs {
		host, port, err := net.SplitHostPort(ipPort)
		if err != nil {
			continue
		}
		if a := udpAddr(host, port); a != nil {
			addrs = append(addrs, dht.NewAddr(a))
		}
	}
	return addrs
}

// udpAddr builds a *net.UDPAddr from an IP string and numeric port string, or
// returns nil if either is malformed.
func udpAddr(ip, port string) *net.UDPAddr {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return nil
	}
	p, err := strconv.Atoi(port)
	if err != nil {
		return nil
	}
	return &net.UDPAddr{IP: parsed, Port: p}
}

// dohLookup resolves host's A records via DoH, trying each endpoint until one
// answers. It returns nil when every endpoint fails.
func dohLookup(ctx context.Context, client *http.Client, host string) []string {
	for _, ep := range dohEndpoints {
		if ips := dohQuery(ctx, client, ep, host); len(ips) > 0 {
			return ips
		}
	}
	return nil
}

// dohQuery performs one DoH JSON query for host's A records at endpoint ep.
func dohQuery(ctx context.Context, client *http.Client, ep, host string) []string {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ep, nil)
	if err != nil {
		return nil
	}
	q := req.URL.Query()
	q.Set("name", host)
	q.Set("type", "A")
	req.URL.RawQuery = q.Encode()
	req.Header.Set("accept", "application/dns-json")

	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	var body struct {
		Answer []struct {
			Type int    `json:"type"`
			Data string `json:"data"`
		} `json:"Answer"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil
	}
	var ips []string
	for _, a := range body.Answer {
		data := strings.TrimSpace(a.Data)
		if a.Type == 1 && net.ParseIP(data) != nil { // type 1 = A record
			ips = append(ips, data)
		}
	}
	return ips
}
