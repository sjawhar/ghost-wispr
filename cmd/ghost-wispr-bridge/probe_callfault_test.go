//go:build callfault

package main

import (
	"bytes"
	"crypto/x509"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// faultAudience is the Pi's public URL in the probe's tests.
const faultAudience = "https://pi.example.com"

// faultToken is the fake metadata server's ID token for audience.
func faultToken(audience string) string { return "token-for:" + audience }

// faultPi is a fake Pi's Serve on TLS: /call opens for the bridge's own
// token alone (401 for none or another), and /api/sessions answers 403 to
// that token and 401 to anything else, as the Pi's table does. It keeps the
// headers of each sessions request.
type faultPi struct {
	srv      *httptest.Server
	mu       sync.Mutex
	sessions []http.Header
}

func faultStartPi(t *testing.T) *faultPi {
	t.Helper()
	p := &faultPi{}
	own := "Bearer " + faultToken(faultAudience)
	mux := http.NewServeMux()
	mux.HandleFunc("/call", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != own {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		up := websocket.Upgrader{}
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	})
	mux.HandleFunc("/api/sessions", func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		p.sessions = append(p.sessions, r.Header.Clone())
		p.mu.Unlock()
		if r.Header.Get("Authorization") == own {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	})
	p.srv = httptest.NewTLSServer(mux)
	t.Cleanup(p.srv.Close)
	return p
}

// faultMetadata is a fake metadata server: it mints an ID token only for a
// request with Metadata-Flavor: Google and format=full, and keeps each
// audience asked for.
type faultMetadata struct {
	srv       *httptest.Server
	mu        sync.Mutex
	audiences []string
}

func faultStartMetadata(t *testing.T) *faultMetadata {
	t.Helper()
	m := &faultMetadata{}
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Metadata-Flavor", "Google")
		q := r.URL.Query()
		if r.URL.Path != "/computeMetadata/v1/instance/service-accounts/default/identity" ||
			r.Header.Get("Metadata-Flavor") != "Google" || q.Get("format") != "full" || q.Get("audience") == "" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		m.mu.Lock()
		m.audiences = append(m.audiences, q.Get("audience"))
		m.mu.Unlock()
		_, _ = w.Write([]byte(faultToken(q.Get("audience"))))
	}))
	t.Cleanup(m.srv.Close)
	return m
}

// faultDNS is a fake DNS server on UDP: it answers each query with its own
// bytes as a reply, and keeps each query.
type faultDNS struct {
	conn    net.PacketConn
	mu      sync.Mutex
	queries [][]byte
}

func faultStartDNS(t *testing.T, answer bool) *faultDNS {
	t.Helper()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen UDP: %v", err)
	}
	d := &faultDNS{conn: conn}
	t.Cleanup(func() { _ = conn.Close() })
	go func() {
		buf := make([]byte, 1500)
		for {
			n, from, err := conn.ReadFrom(buf)
			if err != nil {
				return
			}
			q := slices.Clone(buf[:n])
			d.mu.Lock()
			d.queries = append(d.queries, q)
			d.mu.Unlock()
			if answer {
				_, _ = conn.WriteTo(faultDNSAnswer(q), from)
			}
		}
	}()
	return d
}

func (d *faultDNS) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.queries)
}

// faultDNSAnswer answers a query of one question with one A record,
// 10.0.0.1; it drops the query's additional records (Go's EDNS(0) record).
func faultDNSAnswer(q []byte) []byte {
	end := 12
	for end < len(q) && q[end] != 0 {
		end += int(q[end]) + 1
	}
	end += 5 // the root label, then the type and the class
	if end > len(q) {
		return nil
	}
	a := slices.Clone(q[:end])
	a[2], a[3] = 0x81, 0x80 // a response, recursion desired and available, no error
	a[6], a[7] = 0, 1       // one answer
	a[8], a[9], a[10], a[11] = 0, 0, 0, 0
	return append(a, 0xc0, 0x0c, 0, 1, 0, 1, 0, 0, 0, 60, 0, 4, 10, 0, 0, 1)
}

// faultProbeSettings are the settings and environment of a probe of p, with
// md as the metadata server, forwarder as BRIDGE_PROBE_DNS_ADDRESS (nil:
// unset) and public as the public DNS address.
func faultProbeSettings(t *testing.T, p *faultPi, md *faultMetadata, piName string, forwarder, public *faultDNS) (settings, faultEnv) {
	t.Helper()
	roots := x509.NewCertPool()
	roots.AddCert(p.srv.Certificate())
	u, err := url.Parse(p.srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	s := settings{
		PiAddress:   u.Host,
		PiName:      piName,
		PiAudience:  faultAudience,
		OrgDomains:  []string{"example.org"},
		RunSpecFile: faultWriteSpec(t, faultSpec(faultStart.Add(time.Hour), "probe_network")),
		MetadataURL: md.srv.URL,
	}
	if forwarder != nil {
		s.ProbeDNSAddress = forwarder.conn.LocalAddr().String()
	}
	return s, faultEnv{publicDNS: public.conn.LocalAddr().String(), rootCAs: roots}
}

// faultProbeLines waits for the probe's ten lines and gives their messages.
func faultProbeLines(t *testing.T, r *faultRun) []string {
	t.Helper()
	var got []string
	for _, line := range r.log.waitLines(t, "PROBE ", 10) {
		got = append(got, line.Message)
	}
	return got
}

// probe_network makes each probe of C8's list against the Pi that the
// settings name, with ID tokens from the metadata server, and logs each
// result (c-bridge-reach).
func TestProbeNetworkLogsEachResult(t *testing.T) {
	pi := faultStartPi(t)
	md := faultStartMetadata(t)
	forwarder := faultStartDNS(t, true)
	public := faultStartDNS(t, true)
	// httptest's certificate names example.com.
	s, env := faultProbeSettings(t, pi, md, "example.com", forwarder, public)

	r := faultApply(t, &s, env)
	faultAccepted(t, r)

	want := []string{
		"PROBE name=tcp_443 result=ok",
		"PROBE name=tls_name result=ok",
		"PROBE name=call_no_token result=401",
		"PROBE name=call_other_audience result=401",
		"PROBE name=call_own_token result=101",
		"PROBE name=sessions_own_token result=403",
		"PROBE name=sessions_no_token result=401",
		"PROBE name=sessions_forged_headers result=401",
		"PROBE name=dns result=ok",
		"PROBE name=udp_public_dns result=sent",
	}
	if got := faultProbeLines(t, r); !slices.Equal(got, want) {
		t.Fatalf("probe lines:\n got %q\nwant %q", got, want)
	}

	md.mu.Lock()
	audiences := slices.Clone(md.audiences)
	md.mu.Unlock()
	slices.Sort(audiences)
	if wantAud := []string{faultAudience, faultAudience + "/another-audience"}; !slices.Equal(audiences, wantAud) {
		t.Fatalf("metadata audiences = %q, want %q", audiences, wantAud)
	}

	pi.mu.Lock()
	sessions := slices.Clone(pi.sessions)
	pi.mu.Unlock()
	if len(sessions) != 3 {
		t.Fatalf("%d sessions requests, want 3", len(sessions))
	}
	forged := sessions[2]
	if forged.Get("Authorization") != "" || forged.Get("X-Forwarded-For") != "127.0.0.1" ||
		len(forged["Tailscale-User-Login"]) != 1 || forged["Tailscale-User-Login"][0] != "probe@example.org" ||
		forged.Get("Tailscale_User_Login") != "probe@example.org" {
		t.Fatalf("the forged request's headers = %v, want no token, X-Forwarded-For 127.0.0.1, and both login forms naming probe@example.org", forged)
	}
	if sessions[0].Get("Authorization") != "Bearer "+faultToken(faultAudience) || sessions[1].Get("Authorization") != "" {
		t.Fatalf("the sessions requests' tokens = %q, %q; want the bridge's own, then none",
			sessions[0].Get("Authorization"), sessions[1].Get("Authorization"))
	}

	// The query goes on the wire: a resolver that answers from /etc/hosts,
	// which holds this name on Google's machines, sends nothing.
	if n := forwarder.count(); n != 1 {
		t.Fatalf("the forwarding address got %d queries, want 1", n)
	}
	forwarder.mu.Lock()
	query := forwarder.queries[0]
	forwarder.mu.Unlock()
	if !bytes.Contains(query, []byte("\x08metadata\x06google\x08internal\x00\x00\x01\x00\x01")) {
		t.Fatalf("the forwarding address got % x, want an A query for metadata.google.internal", query)
	}
	if n := public.count(); n != 100 {
		t.Fatalf("the public DNS address got %d packets, want 100", n)
	}
	lines := r.log.waitLines(t, "PROBE name=udp_public_dns", 1)
	if !faultHasAttr(lines[0].Raw, "packets", 100) || !faultHasAttr(lines[0].Raw, "replies", 100) {
		t.Fatalf("the UDP line = %s, want packets 100 and replies 100", lines[0].Raw)
	}
	faultNoMeetURL(t, r.log)
}

// The TLS probe checks the Pi's name: a certificate that does not name
// BRIDGE_PI_NAME fails the handshake and each request after it, so a
// passing handshake is the name's check and not any certificate's.
func TestProbeNetworkRefusesACertificateForAnotherName(t *testing.T) {
	pi := faultStartPi(t)
	md := faultStartMetadata(t)
	forwarder := faultStartDNS(t, true)
	public := faultStartDNS(t, false)
	s, env := faultProbeSettings(t, pi, md, "pi.example.org", forwarder, public)

	r := faultApply(t, &s, env)
	faultAccepted(t, r)

	want := []string{
		"PROBE name=tcp_443 result=ok",
		"PROBE name=tls_name result=failed",
		"PROBE name=call_no_token result=failed",
		"PROBE name=call_other_audience result=failed",
		"PROBE name=call_own_token result=failed",
		"PROBE name=sessions_own_token result=failed",
		"PROBE name=sessions_no_token result=failed",
		"PROBE name=sessions_forged_headers result=failed",
		"PROBE name=dns result=ok",
		"PROBE name=udp_public_dns result=sent",
	}
	if got := faultProbeLines(t, r); !slices.Equal(got, want) {
		t.Fatalf("probe lines:\n got %q\nwant %q", got, want)
	}
	pi.mu.Lock()
	n := len(pi.sessions)
	pi.mu.Unlock()
	if n != 0 {
		t.Fatalf("the Pi got %d sessions requests over a handshake that failed its name", n)
	}
}

// With BRIDGE_PROBE_DNS_ADDRESS unset, the DNS probe cannot run: it says
// UNKNOWN and why, never ok (contract A4), and the other probes still run.
func TestProbeNetworkReportsTheDNSQueryUnknownWithNoAddress(t *testing.T) {
	pi := faultStartPi(t)
	md := faultStartMetadata(t)
	public := faultStartDNS(t, true)
	s, env := faultProbeSettings(t, pi, md, "example.com", nil, public)

	r := faultApply(t, &s, env)
	faultAccepted(t, r)

	got := faultProbeLines(t, r)
	if got[8] != "PROBE name=dns result=UNKNOWN" || got[4] != "PROBE name=call_own_token result=101" {
		t.Fatalf("probe lines = %q, want the DNS probe UNKNOWN and the others run", got)
	}
	line := r.log.waitLines(t, "PROBE name=dns ", 1)[0]
	if !strings.Contains(line.Raw, `"reason":"BRIDGE_PROBE_DNS_ADDRESS is not set"`) {
		t.Fatalf("the DNS line = %s, want the reason", line.Raw)
	}
}

// faultHasAttr reports whether the JSON log line raw has key with the
// number n.
func faultHasAttr(raw, key string, n int) bool {
	var attrs map[string]any
	if err := json.Unmarshal([]byte(raw), &attrs); err != nil {
		return false
	}
	v, ok := attrs[key].(float64)
	return ok && int(v) == n
}

// C9: no PROBE line holds an error's text. A probe that failed gives its
// error's class, and one that could not run its reason and class; here the
// metadata server's URL carries the sentinel, which each of Go's HTTP errors
// repeats.
func TestProbeNetworkLogsNoErrorText(t *testing.T) {
	const sentinel = "zebra-sentinel-6603"
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed := ln.Addr().String()
	_ = ln.Close()
	log := &faultLog{}
	p := probeTargets{piAddr: closed, piName: "pi.example.com", audience: faultAudience,
		metadataURL: "http://" + closed + "/" + sentinel, forgedLogin: "probe@example.com",
		dnsAddr: closed, publicDNS: closed}
	probeNetwork(t.Context(), &p, log.logger())
	lines := log.lines(t)
	if len(lines) != 10 {
		t.Fatalf("probe lines = %v, want one for each of the 10 probes", lines)
	}
	for _, line := range lines {
		if strings.Contains(line.Raw, sentinel) {
			t.Errorf("a PROBE line holds the error's text: %s", line.Raw)
		}
	}
}
