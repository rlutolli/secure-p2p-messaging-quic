package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/csv"
	"encoding/pem"
	"fmt"
	"io"
	"math"
	"math/big"
	"net"
	"os"
	"time"

	"github.com/quic-go/quic-go"
)

// --- Configuration ---

const numIterations = 10 // connections per mode for statistical averaging

var clientSessionCache = tls.NewLRUClientSessionCache(100)

// --- CLI entry point ---

func main() {
	if len(os.Args) < 2 {
		fmt.Println("Usage: go run quic_0rtt.go [server|client] [addr] [port]")
		fmt.Println("  server <port>       — starts an echo server with 0-RTT enabled")
		fmt.Println("  client <addr> <port> — runs 1-RTT and 0-RTT benchmarks against the server")
		return
	}
	mode := os.Args[1]
	port := "9002"
	if mode == "server" {
		if len(os.Args) >= 3 {
			port = os.Args[2]
		}
		runServer(port)
	} else {
		addr := "172.20.0.10"
		if len(os.Args) >= 3 {
			addr = os.Args[2]
		}
		if len(os.Args) >= 4 {
			port = os.Args[3]
		}
		runClient(addr, port)
	}
}

// --- Server ---

func runServer(port string) {
	tlsConf := generateTLSConfig()
	quicConf := &quic.Config{Allow0RTT: true} // enable 0-RTT acceptance

	l, err := quic.ListenAddr(":"+port, tlsConf, quicConf)
	if err != nil {
		panic(err)
	}
	fmt.Printf("QUIC 0-RTT Echo Server on :%s\n", port)

	for {
		conn, err := l.Accept(context.Background())
		if err != nil {
			continue
		}

		go func(c *quic.Conn) {
			is0RTT := c.ConnectionState().TLS.DidResume
			if is0RTT {
				fmt.Println(">> Server accepted 0-RTT (Resumed) connection")
			} else {
				fmt.Println(">> Server accepted 1-RTT (New) connection")
			}

			// Accept single stream and echo the message back to the client.
			s, err := c.AcceptStream(context.Background())
			if err != nil {
				return
			}
			echoStream(s)
			c.CloseWithError(0, "")
		}(conn)
	}
}

// echoStream reads all data from the stream (until the client half-closes)
// and writes it back, allowing the client to measure round-trip timing.
func echoStream(s *quic.Stream) {
	data, err := io.ReadAll(s)
	if err != nil {
		return
	}
	if len(data) > 0 {
		s.Write(data)
	}
	s.Close()
}

// --- Client ---

func runClient(addr, port string) {
	tlsConf := &tls.Config{
		InsecureSkipVerify: true,
		ClientSessionCache: clientSessionCache,
		NextProtos:         []string{"bench"},
	}

	dest := net.JoinHostPort(addr, port)

	// ------------------------------------------------------------------
	// Warm-up: one full 1-RTT connection to populate the session cache.
	// Without this, the first DialAddrEarly has nothing to resume from
	// and falls back to a full handshake.
	// ------------------------------------------------------------------
	fmt.Println("--- Warm-up (establishing session) ---")
	warmupConn, err := quic.DialAddr(context.Background(), dest, tlsConf, nil)
	if err != nil {
		fmt.Printf("Warm-up dial failed: %v\n", err)
		return
	}
	<-warmupConn.HandshakeComplete()
	{
		s, _ := warmupConn.OpenStreamSync(context.Background())
		s.Write([]byte("ping"))
		s.Close()
		io.ReadAll(s) // drain echo
	}
	warmupConn.CloseWithError(0, "")
	time.Sleep(200 * time.Millisecond)

	// ------------------------------------------------------------------
	// 1-RTT measurements (DialAddr — full handshake every time)
	// ------------------------------------------------------------------
	fmt.Printf("\n=== Running %d × 1-RTT connections (DialAddr) ===\n", numIterations)
	rtt1 := run1RTTLoop(dest, tlsConf)

	// ------------------------------------------------------------------
	// 0-RTT measurements (DialAddrEarly — session resumption)
	// ------------------------------------------------------------------
	fmt.Printf("\n=== Running %d × 0-RTT connections (DialAddrEarly) ===\n", numIterations)
	rtt0 := run0RTTLoop(dest, tlsConf)

	// ------------------------------------------------------------------
	// Report
	// ------------------------------------------------------------------
	printResultsTable(rtt1, rtt0)

	// CSV output: one row per (proto, metric), aggregated across all iterations.
	csvPath := "0rtt_results.csv"
	if err := writeCSV(csvPath, rtt1, rtt0); err != nil {
		fmt.Printf("warning: could not write CSV: %v\n", err)
	} else {
		fmt.Printf("\nResults written to %s\n", csvPath)
	}
}

type measurement struct {
	dialReturn time.Duration // local time for DialAddr/DialAddrEarly to return
	handshake  time.Duration // <-HandshakeComplete() — server-side handshake done
	dataSent   time.Duration // stream open + write + close
	firstResp  time.Duration // first byte of echo received from server
	used0RTT   bool          // whether 0-RTT was actually accepted (client-side)
}

// run1RTTLoop opens numIterations connections using DialAddr.
// DialAddr blocks until the server has completed its handshake, so the
// handshake-complete channel fires essentially immediately.
func run1RTTLoop(dest string, tlsConf *tls.Config) []measurement {
	results := make([]measurement, 0, numIterations)

	for i := 0; i < numIterations; i++ {
		t0 := time.Now()

		// ---- dial (blocks until handshake complete in quic-go) ----
		conn, err := quic.DialAddr(context.Background(), dest, tlsConf, nil)
		tDial := time.Since(t0)
		if err != nil {
			fmt.Printf("  [1-RTT %d] dial failed: %v\n", i, err)
			continue
		}

		// ---- wait for handshake-complete signal (fires immediately) ----
		select {
		case <-conn.HandshakeComplete():
			// ok
		case <-time.After(5 * time.Second):
			fmt.Printf("  [1-RTT %d] handshake timeout\n", i)
			conn.CloseWithError(0, "")
			continue
		}
		tHandshake := time.Since(t0)

		// ---- open stream, write payload, half-close write side ----
		stream, err := conn.OpenStreamSync(context.Background())
		if err != nil {
			fmt.Printf("  [1-RTT %d] open stream failed: %v\n", i, err)
			conn.CloseWithError(0, "")
			continue
		}
		stream.Write([]byte("Hello"))
		stream.Close() // half-close: server sees EOF after reading payload
		tDataSent := time.Since(t0)

		// ---- read echo (server echoes back after processing) ----
		respBuf := make([]byte, 1024)
		_, err = stream.Read(respBuf)
		tFirstResp := time.Since(t0)
		if err != nil && err != io.EOF {
			fmt.Printf("  [1-RTT %d] read echo failed: %v\n", i, err)
		}

		conn.CloseWithError(0, "")

		results = append(results, measurement{
			dialReturn: tDial,
			handshake:  tHandshake,
			dataSent:   tDataSent,
			firstResp:  tFirstResp,
		})
	}
	return results
}

// run0RTTLoop opens numIterations connections using DialAddrEarly.
// DialAddrEarly returns before the handshake completes, allowing 0-RTT
// application data to be sent in the first UDP datagram.  The server-side
// handshake still takes ~1 RTT; the only saving is in the client→server
// data direction.
func run0RTTLoop(dest string, tlsConf *tls.Config) []measurement {
	results := make([]measurement, 0, numIterations)

	for i := 0; i < numIterations; i++ {
		t0 := time.Now()

		// ---- dial early (returns immediately, before handshake) ----
		conn, err := quic.DialAddrEarly(context.Background(), dest, tlsConf, nil)
		tDial := time.Since(t0)
		if err != nil {
			fmt.Printf("  [0-RTT %d] dial failed: %v\n", i, err)
			continue
		}

		was0RTT := conn.ConnectionState().Used0RTT

		// ---- open stream while handshake is still in progress ----
		// If 0-RTT is accepted, this stream carries 0-RTT data.
		// If rejected, data is queued and sent after handshake completes.
		stream, err := conn.OpenStreamSync(context.Background())
		if err != nil {
			fmt.Printf("  [0-RTT %d] open stream failed: %v\n", i, err)
			conn.CloseWithError(0, "")
			continue
		}
		stream.Write([]byte("Hello"))
		stream.Close() // half-close; if 0-RTT is active, FIN goes in 0-RTT packet
		tDataSent := time.Since(t0)

		// ---- wait for handshake complete ----
		// This is the honest measurement: the server's handshake is not
		// accelerated by 0-RTT in quic-go's current implementation.
		select {
		case <-conn.HandshakeComplete():
			// ok
		case <-time.After(5 * time.Second):
			fmt.Printf("  [0-RTT %d] handshake timeout\n", i)
			conn.CloseWithError(0, "")
			continue
		}
		tHandshake := time.Since(t0)

		// ---- read echo (server can only process 0-RTT data after its
		//      handshake completes) ----
		respBuf := make([]byte, 1024)
		_, err = stream.Read(respBuf)
		tFirstResp := time.Since(t0)
		if err != nil && err != io.EOF {
			fmt.Printf("  [0-RTT %d] read echo failed: %v\n", i, err)
		}

		conn.CloseWithError(0, "")

		results = append(results, measurement{
			dialReturn: tDial,
			handshake:  tHandshake,
			dataSent:   tDataSent,
			firstResp:  tFirstResp,
			used0RTT:   was0RTT,
		})
	}
	return results
}

// --- Statistics helpers ---

// stats returns the mean and the 95%-confidence-interval margin from a slice
// of durations.  Entries ≤ 0 are treated as missing and filtered out.
func stats(durations []time.Duration) (mean, ciMargin time.Duration) {
	var valid []float64
	for _, d := range durations {
		if d > 0 {
			valid = append(valid, float64(d))
		}
	}
	n := len(valid)
	if n == 0 {
		return 0, 0
	}

	// mean
	var sum float64
	for _, v := range valid {
		sum += v
	}
	meanF := sum / float64(n)
	mean = time.Duration(meanF)

	if n == 1 {
		return mean, 0
	}

	// sample standard deviation
	var sqDiff float64
	for _, v := range valid {
		d := v - meanF
		sqDiff += d * d
	}
	stddev := math.Sqrt(sqDiff / float64(n-1))

	// Student's t critical value for 95% CI (two-sided)
	// We use a lookup table for common sample sizes.
	tCrit := 2.262 // n=10
	switch {
	case n <= 2:
		tCrit = 12.706
	case n == 3:
		tCrit = 4.303
	case n == 4:
		tCrit = 3.182
	case n == 5:
		tCrit = 2.776
	case n <= 7:
		tCrit = 2.447
	case n <= 9:
		tCrit = 2.306
	default:
		tCrit = 2.262
	}
	ciMargin = time.Duration(tCrit * stddev / math.Sqrt(float64(n)))
	return mean, ciMargin
}

// fmtDuration produces a compact, readable duration string.
func fmtDuration(d time.Duration) string {
	if d <= 0 {
		return "   N/A    "
	}
	switch {
	case d < 10*time.Microsecond:
		return fmt.Sprintf("%.1f µs", float64(d.Nanoseconds())/1000.0)
	case d < time.Millisecond:
		return fmt.Sprintf("%.0f µs", float64(d.Nanoseconds())/1000.0)
	case d < time.Second:
		return fmt.Sprintf("%.1f ms", float64(d.Microseconds())/1000.0)
	default:
		return fmt.Sprintf("%.2f s", d.Seconds())
	}
}

// printResultsTable renders a formatted comparison table with means and 95% CI.
func printResultsTable(rtt1, rtt0 []measurement) {
	// build per-metric slices
	dial1 := make([]time.Duration, len(rtt1))
	dial0 := make([]time.Duration, len(rtt0))
	hs1 := make([]time.Duration, len(rtt1))
	hs0 := make([]time.Duration, len(rtt0))
	data1 := make([]time.Duration, len(rtt1))
	data0 := make([]time.Duration, len(rtt0))
	resp1 := make([]time.Duration, len(rtt1))
	resp0 := make([]time.Duration, len(rtt0))

	for i, m := range rtt1 {
		dial1[i] = m.dialReturn
		hs1[i] = m.handshake
		data1[i] = m.dataSent
		resp1[i] = m.firstResp
	}
	for i, m := range rtt0 {
		dial0[i] = m.dialReturn
		hs0[i] = m.handshake
		data0[i] = m.dataSent
		resp0[i] = m.firstResp
	}

	fmt.Println()
	fmt.Println("╔════════════════════════════════════════════════════════════════════════════╗")
	fmt.Println("║            QUIC 1-RTT vs 0-RTT Connection Timing Benchmarks               ║")
	fmt.Println("╠════════════════════════════════════════════════════════════════════════════╣")
	fmt.Printf("║ %-38s │ %14s │ %14s ║\n", "Metric (mean ± 95% CI)", "1-RTT", "0-RTT")
	fmt.Println("╠════════════════════════════════════════════════════════════════════════════╣")

	printRow("local dial return", dial1, dial0)
	printRow("handshake complete", hs1, hs0)
	printRow("first app-data sent", data1, data0)
	printRow("first response byte", resp1, resp0)

	fmt.Println("╚════════════════════════════════════════════════════════════════════════════╝")
	fmt.Println()
	fmt.Println("Interpretation:")
	fmt.Println("  • 'handshake complete' measures the time until <-conn.HandshakeComplete()")
	fmt.Println("    fires on the client — i.e. the time until the server has finished its")
	fmt.Println("    TLS handshake.  quic-go's 0-RTT does NOT accelerate the server-side")
	fmt.Println("    handshake; the only difference is that DialAddrEarly returns before")
	fmt.Println("    the handshake completes, allowing the client to queue 0-RTT data.")
	fmt.Println()
	fmt.Println("  • 'first app-data sent' shows the client-side advantage: with 0-RTT the")
	fmt.Println("    client can write and close a stream before the handshake finishes.")
	fmt.Println("    With 1-RTT, DialAddr first waits for the handshake.")
	fmt.Println()
	fmt.Println("  • 'first response byte' is the end-to-end user-facing metric.  The server")
	fmt.Println("    cannot deliver 0-RTT data to the application until its own handshake")
	fmt.Println("    completes, so the echo response arrives at roughly the same time for")
	fmt.Println("    both modes.  quic-go's current implementation buffers 0-RTT data until")
	fmt.Println("    the server handshake is done.")
	fmt.Println()

	// report 0-RTT acceptance rate
	accepted := 0
	for _, m := range rtt0 {
		if m.used0RTT {
			accepted++
		}
	}
	fmt.Printf("0-RTT accepted by server: %d / %d connections\n", accepted, len(rtt0))
}

// printRow computes stats for two sets of measurements and prints a table row.
func printRow(name string, rtt1, rtt0 []time.Duration) {
	m1, c1 := stats(rtt1)
	m0, c0 := stats(rtt0)

	c1Str := "        "
	c0Str := "        "
	if c1 > 0 {
		c1Str = "±" + fmtDuration(c1)
	}
	if c0 > 0 {
		c0Str = "±" + fmtDuration(c0)
	}

	fmt.Printf("║ %-38s │ %10s %-8s │ %10s %-8s ║\n",
		name,
		fmtDuration(m1), c1Str,
		fmtDuration(m0), c0Str,
	)
}

// --- CSV output ---

// t-critical for 95% CI, df=9 ≈ 2.262
const tCrit95df9 = 2.262

// meanAndCI computes the mean and 95% CI half-width for a slice of int64 samples
// (in microseconds) using Student's t-distribution.
func meanAndCI(samples []int64) (mean, ci float64) {
	if len(samples) == 0 {
		return 0, 0
	}
	var sum float64
	for _, s := range samples {
		sum += float64(s)
	}
	mean = sum / float64(len(samples))
	if len(samples) < 2 {
		return mean, 0
	}
	var sq float64
	for _, s := range samples {
		d := float64(s) - mean
		sq += d * d
	}
	variance := sq / float64(len(samples)-1)
	sd := math.Sqrt(variance)
	ci = sd * tCrit95df9 / math.Sqrt(float64(len(samples)))
	return mean, ci
}

// writeCSV outputs aggregated per-metric means and 95% CIs in a fixed schema.
// Schema (header line):
//
//	proto,metric,mean_us,ci95_halfwidth_us,n,unit
//
// unit is "us" (microseconds) for all metrics — the chart converts to ms.
func writeCSV(path string, rtt1, rtt0 []measurement) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := csv.NewWriter(f)
	defer w.Flush()

	if err := w.Write([]string{"proto", "metric", "mean_us", "ci95_halfwidth_us", "n", "unit"}); err != nil {
		return err
	}

	// Metric names: must match exactly what fig_0rtt() in chart_repeats.py parses
	metricNames := []string{"local_dial_return", "handshake_complete", "first_app_data_sent", "first_response_byte"}

	for _, proto := range []string{"1-RTT", "0-RTT"} {
		var rtt []measurement
		if proto == "1-RTT" {
			rtt = rtt1
		} else {
			rtt = rtt0
		}
		// Compute mean and CI for each of the 4 metrics
		for i, name := range metricNames {
			var samples []int64
			for _, m := range rtt {
				switch i {
				case 0:
					samples = append(samples, m.dialReturn.Microseconds())
				case 1:
					samples = append(samples, m.handshake.Microseconds())
				case 2:
					samples = append(samples, m.dataSent.Microseconds())
				case 3:
					samples = append(samples, m.firstResp.Microseconds())
				}
			}
			if len(samples) == 0 {
				continue
			}
			mean, ci := meanAndCI(samples)
			if err := w.Write([]string{
				proto, name,
				fmt.Sprintf("%.2f", mean),
				fmt.Sprintf("%.2f", ci),
				fmt.Sprintf("%d", len(samples)),
				"us",
			}); err != nil {
				return err
			}
		}
	}
	return nil
}

// --- TLS certificate (unchanged) ---

func generateTLSConfig() *tls.Config {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{Organization: []string{"Bench"}},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("172.20.0.10"), net.ParseIP("172.20.0.11")},
	}
	certDER, _ := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	keyDER, _ := x509.MarshalECPrivateKey(key)
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	cert, _ := tls.X509KeyPair(certPEM, keyPEM)
	return &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{"bench"}}
}
