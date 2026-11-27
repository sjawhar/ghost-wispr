//go:build callfault

package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

// probeStepTimeout bounds each probe.
const probeStepTimeout = 10 * time.Second

// probeUDPPackets is how many DNS queries probe_network sends to the public
// DNS address, and probeReplyWait how long it then waits for their replies.
const (
	probeUDPPackets = 100
	probeReplyWait  = 2 * time.Second
)

// probeDNSName is the name of the DNS query to the inbound forwarding
// address: Google's metadata server, which every VPC network resolves.
// probe_network sends the query itself: Go's resolver would answer this name
// from /etc/hosts, which on Google's machines holds it, and send nothing.
const probeDNSName = "metadata.google.internal."

// probePublicDNSName is the name of the 100 queries to the public DNS
// address.
const probePublicDNSName = "example.com."

// probeTargets is what probe_network reaches: the Pi's Serve (piAddr, with
// its port), the name its certificate must carry, the audience of the
// bridge's ID token, the metadata server that mints it, the login the
// forged headers name, the inbound forwarding address of the network's DNS
// (empty: unset), and the public DNS address.
type probeTargets struct {
	piAddr, piName, audience string
	metadataURL              string
	forgedLogin              string
	dnsAddr, publicDNS       string
	rootCAs                  *x509.CertPool
}

// probeTargetsFor reads the probe's targets from the bridge's settings.
// BRIDGE_PI_ADDRESS names the Pi's tailnet address, on Serve's port 443
// unless it names a port; BRIDGE_PROBE_DNS_ADDRESS names the forwarding
// address, on port 53 unless it names a port.
func probeTargetsFor(s *settings, env faultEnv) probeTargets {
	domain := "example.com"
	if len(s.OrgDomains) > 0 {
		domain = s.OrgDomains[0]
	}
	t := probeTargets{
		piAddr:      withPort(s.PiAddress, "443"),
		piName:      s.PiName,
		audience:    s.PiAudience,
		metadataURL: s.MetadataURL,
		forgedLogin: "probe@" + domain,
		publicDNS:   env.publicDNS,
		rootCAs:     env.rootCAs,
	}
	if s.ProbeDNSAddress != "" {
		t.dnsAddr = withPort(s.ProbeDNSAddress, "53")
	}
	return t
}

// withPort is addr with port when addr names none.
func withPort(addr, port string) string {
	if _, _, err := net.SplitHostPort(addr); err == nil {
		return addr
	}
	return net.JoinHostPort(addr, port)
}

func (p *probeTargets) tlsConfig() *tls.Config {
	return &tls.Config{ServerName: p.piName, RootCAs: p.rootCAs, MinVersion: tls.VersionTLS12}
}

// probeNetwork runs probe_network's list (plan C8) once, in order, and logs
// each result as PROBE name=<n> result=<r>: an HTTP status, 101 for a call
// socket that opened, ok, sent, failed, or UNKNOWN when a probe could not
// run. It logs no response body, and a failure by its error's class alone.
func probeNetwork(ctx context.Context, p *probeTargets, log *slog.Logger) {
	report := func(name, result string, attrs ...any) {
		log.Info("PROBE name="+name+" result="+result, attrs...)
	}
	failed := func(name string, err error) { report(name, "failed", "error", errorClass(err)) }

	step := func(f func(ctx context.Context)) {
		sctx, cancel := context.WithTimeout(ctx, probeStepTimeout)
		defer cancel()
		f(sctx)
	}
	var d net.Dialer

	step(func(ctx context.Context) {
		conn, err := d.DialContext(ctx, "tcp", p.piAddr)
		if err != nil {
			failed("tcp_443", err)
			return
		}
		_ = conn.Close()
		report("tcp_443", "ok")
	})
	step(func(ctx context.Context) {
		conn, err := (&tls.Dialer{NetDialer: &d, Config: p.tlsConfig()}).DialContext(ctx, "tcp", p.piAddr)
		if err != nil {
			failed("tls_name", err)
			return
		}
		version := tls.VersionName(conn.(*tls.Conn).ConnectionState().Version)
		_ = conn.Close()
		report("tls_name", "ok", "version", version)
	})

	var own, other string
	var ownErr, otherErr error
	step(func(ctx context.Context) { other, otherErr = p.idToken(ctx, p.audience+"/another-audience") })
	step(func(ctx context.Context) { own, ownErr = p.idToken(ctx, p.audience) })

	// statusProbe reports the HTTP status a probe gets, or UNKNOWN when the
	// ID token it needs did not come.
	statusProbe := func(name string, tokenErr error, probe func(ctx context.Context) (int, error)) {
		if tokenErr != nil {
			report(name, "UNKNOWN", "reason", "no_id_token", "error", errorClass(tokenErr))
			return
		}
		step(func(ctx context.Context) {
			status, err := probe(ctx)
			if err != nil {
				failed(name, err)
				return
			}
			report(name, strconv.Itoa(status))
		})
	}
	callProbe := func(name, token string, tokenErr error) {
		statusProbe(name, tokenErr, func(ctx context.Context) (int, error) { return p.call(ctx, token) })
	}
	callProbe("call_no_token", "", nil)
	callProbe("call_other_audience", other, otherErr)
	callProbe("call_own_token", own, ownErr)

	client := &http.Client{
		Transport: &http.Transport{TLSClientConfig: p.tlsConfig(), DialContext: d.DialContext},
		// The first answer is the probe's: a redirect is not followed.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	defer client.CloseIdleConnections()
	sessionsProbe := func(name string, header http.Header, tokenErr error) {
		statusProbe(name, tokenErr, func(ctx context.Context) (int, error) { return p.sessions(ctx, client, header) })
	}
	sessionsProbe("sessions_own_token", http.Header{"Authorization": {"Bearer " + own}}, ownErr)
	sessionsProbe("sessions_no_token", http.Header{}, nil)
	// Go writes these keys as given; Tailscale_User_Login keeps its
	// underscores, the form some proxies turn into Tailscale-User-Login.
	sessionsProbe("sessions_forged_headers", http.Header{
		"Tailscale-User-Login": {p.forgedLogin},
		"Tailscale_User_Login": {p.forgedLogin},
		"X-Forwarded-For":      {"127.0.0.1"},
	}, nil)

	if p.dnsAddr == "" {
		report("dns", "UNKNOWN", "reason", "BRIDGE_PROBE_DNS_ADDRESS is not set")
	} else {
		step(func(ctx context.Context) {
			rcode, answers, err := p.forwarderQuery(ctx, &d)
			switch {
			case err != nil:
				failed("dns", err)
			case rcode == 0:
				report("dns", "ok", "answers", answers)
			case rcode == 3:
				report("dns", "not_found")
			default:
				report("dns", "failed", "rcode", rcode)
			}
		})
	}

	step(func(ctx context.Context) {
		sent, replies, err := p.publicDNSPackets(ctx, &d)
		if err != nil && sent == 0 {
			failed("udp_public_dns", err)
			return
		}
		report("udp_public_dns", "sent", "packets", sent, "replies", replies)
	})
}

// idToken mints the bridge's Google ID token for audience from the
// metadata server (format=full, as the Pi's check needs the email).
func (p *probeTargets) idToken(ctx context.Context, audience string) (string, error) {
	u := p.metadataURL + "/computeMetadata/v1/instance/service-accounts/default/identity?" +
		url.Values{"audience": {audience}, "format": {"full"}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, http.NoBody)
	if err != nil {
		return "", err
	}
	req.Header.Set("Metadata-Flavor", "Google")
	// No proxy: the metadata server is reached directly, never through one.
	resp, err := (&http.Client{Transport: &http.Transport{}}).Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("the metadata server answered %d", resp.StatusCode)
	}
	return strings.TrimSpace(string(body)), nil
}

// call opens /call with token (none when empty) and gives the handshake's
// status: 101 when the socket opened, which the probe closes at once.
func (p *probeTargets) call(ctx context.Context, token string) (int, error) {
	dialer := websocket.Dialer{
		NetDialContext:   (&net.Dialer{}).DialContext,
		TLSClientConfig:  p.tlsConfig(),
		HandshakeTimeout: probeStepTimeout,
	}
	header := http.Header{}
	if token != "" {
		header.Set("Authorization", "Bearer "+token)
	}
	conn, resp, err := dialer.DialContext(ctx, "wss://"+p.piAddr+"/call", header)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err == nil {
		_ = conn.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
		_ = conn.Close()
		return http.StatusSwitchingProtocols, nil
	}
	if resp != nil {
		return resp.StatusCode, nil
	}
	return 0, err
}

// sessions sends GET /api/sessions with header and gives the status. The
// body is read and dropped: it may list the Pi's sessions.
func (p *probeTargets) sessions(ctx context.Context, client *http.Client, header http.Header) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+p.piAddr+"/api/sessions", http.NoBody)
	if err != nil {
		return 0, err
	}
	for k, v := range header {
		req.Header[k] = v
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	_ = resp.Body.Close()
	return resp.StatusCode, nil
}

// forwarderQuery sends one DNS query for probeDNSName to the inbound
// forwarding address and gives the reply's response code and its count of
// answers.
func (p *probeTargets) forwarderQuery(ctx context.Context, d *net.Dialer) (rcode, answers int, err error) {
	conn, err := d.DialContext(ctx, "udp", p.dnsAddr)
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = conn.Close() }()
	const id = 0x6770
	if _, err := conn.Write(probeDNSQuery(id, probeDNSName)); err != nil {
		return 0, 0, err
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetReadDeadline(deadline)
	}
	buf := make([]byte, 1500)
	for {
		n, err := conn.Read(buf)
		if err != nil {
			return 0, 0, err
		}
		// A reply to this query: its id, and the response bit set.
		if n >= 12 && buf[0] == id>>8 && buf[1] == id&0xff && buf[2]&0x80 != 0 {
			return int(buf[3] & 0x0f), int(buf[6])<<8 | int(buf[7]), nil
		}
	}
}

// publicDNSPackets sends probeUDPPackets DNS queries to the public DNS
// address, then counts the replies that come within probeReplyWait.
func (p *probeTargets) publicDNSPackets(ctx context.Context, d *net.Dialer) (sent, replies int, err error) {
	conn, err := d.DialContext(ctx, "udp", p.publicDNS)
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = conn.Close() }()
	for i := range probeUDPPackets {
		if _, err := conn.Write(probeDNSQuery(uint16(i), probePublicDNSName)); err != nil {
			return sent, 0, err
		}
		sent++
	}
	_ = conn.SetReadDeadline(time.Now().Add(probeReplyWait))
	buf := make([]byte, 512)
	for replies < sent {
		if _, err := conn.Read(buf); err != nil {
			break
		}
		replies++
	}
	return sent, replies, nil
}

// probeDNSQuery is a DNS query with id, recursion desired, for the A record
// of name, a fully qualified name.
func probeDNSQuery(id uint16, name string) []byte {
	q := []byte{byte(id >> 8), byte(id), 0x01, 0x00, 0, 1, 0, 0, 0, 0, 0, 0}
	for label := range strings.SplitSeq(strings.TrimSuffix(name, "."), ".") {
		q = append(q, byte(len(label)))
		q = append(q, label...)
	}
	return append(q, 0, 0, 1, 0, 1)
}
