package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Client struct {
	conn    net.Conn
	id      string
	swarmID string
}

type FileRecord struct {
	Size    int64
	Hash    string
	OwnerID string
}

type Swarm struct {
	Clients map[string]*Client
	Index   map[string]FileRecord
	Mu      sync.RWMutex
}

// ── Metrics ──────────────────────────────────────────────────────────────────

type metrics struct {
	BytesReceived  atomic.Int64
	BytesSent      atomic.Int64
	FilesRelayed   atomic.Int64
	ActiveConns    atomic.Int64
	TotalConns     atomic.Int64
	ConflictsTotal atomic.Int64
	startTime      time.Time
}

var stats = &metrics{
	startTime: time.Now(),
}

type metricsSnapshot struct {
	UptimeSeconds   float64 `json:"uptime_seconds"`
	Goroutines      int     `json:"goroutines"`
	ActiveConns     int64   `json:"active_connections"`
	TotalConns      int64   `json:"total_connections"`
	ActiveSwarms    int     `json:"active_swarms"`
	TotalPeers      int     `json:"total_peers"`
	BytesReceived   int64   `json:"bytes_received"`
	BytesSent       int64   `json:"bytes_sent"`
	BytesReceivedMB float64 `json:"bytes_received_mb"`
	BytesSentMB     float64 `json:"bytes_sent_mb"`
	FilesRelayed    int64   `json:"files_relayed"`
	ConflictsTotal  int64   `json:"conflicts_total"`
}

func collectSnapshot() metricsSnapshot {
	globalMu.RLock()
	swarmCount := len(swarms)
	peerCount := 0
	for _, s := range swarms {
		s.Mu.RLock()
		peerCount += len(s.Clients)
		s.Mu.RUnlock()
	}
	globalMu.RUnlock()

	recv := stats.BytesReceived.Load()
	sent := stats.BytesSent.Load()

	return metricsSnapshot{
		UptimeSeconds:   time.Since(stats.startTime).Seconds(),
		Goroutines:      runtime.NumGoroutine(),
		ActiveConns:     stats.ActiveConns.Load(),
		TotalConns:      stats.TotalConns.Load(),
		ActiveSwarms:    swarmCount,
		TotalPeers:      peerCount,
		BytesReceived:   recv,
		BytesSent:       sent,
		BytesReceivedMB: float64(recv) / (1024 * 1024),
		BytesSentMB:     float64(sent) / (1024 * 1024),
		FilesRelayed:    stats.FilesRelayed.Load(),
		ConflictsTotal:  stats.ConflictsTotal.Load(),
	}
}

func startMetricsServer() {
	mux := http.NewServeMux()

	// JSON endpoint — for programmatic access / thesis tooling
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(collectSnapshot())
	})

	// Human-readable dashboard — open in browser at http://localhost:9001/
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		s := collectSnapshot()
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, `<!DOCTYPE html>
<html>
<head>
<meta charset="utf-8">
<meta http-equiv="refresh" content="3">
<title>Relay Metrics</title>
<style>
  body { font-family: monospace; background: #0d1117; color: #e6edf3; padding: 2rem; }
  h1   { color: #388bfd; margin-bottom: 0.25rem; }
  .sub { color: #6e7681; font-size: 0.85rem; margin-bottom: 2rem; }
  .grid { display: grid; grid-template-columns: repeat(auto-fill, minmax(220px, 1fr)); gap: 1rem; }
  .card { background: #161b22; border: 1px solid #30363d; border-radius: 8px; padding: 1.25rem; }
  .card .val { font-size: 2rem; font-weight: bold; color: #388bfd; }
  .card .lbl { font-size: 0.75rem; color: #6e7681; margin-top: 0.25rem; text-transform: uppercase; letter-spacing: 0.05em; }
  .green { color: #23c55e; }
  .amber { color: #d29922; }
  table { width: 100%%; border-collapse: collapse; margin-top: 2rem; }
  th { text-align: left; color: #6e7681; font-size: 0.75rem; padding: 0.5rem; border-bottom: 1px solid #30363d; }
  td { padding: 0.5rem; border-bottom: 1px solid #21262d; }
  .badge { display:inline-block; padding:0.15rem 0.5rem; border-radius:4px; font-size:0.75rem; }
  .badge-blue { background:#1f3a5f; color:#388bfd; }
  .badge-green { background:#1a3a2a; color:#23c55e; }
</style>
</head>
<body>
<h1>&#9646; Relay Server Metrics</h1>
<div class="sub">Uptime: %.0fs &nbsp;|&nbsp; Auto-refreshes every 3s &nbsp;|&nbsp; <a href="/metrics" style="color:#388bfd">JSON</a></div>
<div class="grid">
  <div class="card"><div class="val green">%d</div><div class="lbl">Active Connections</div></div>
  <div class="card"><div class="val">%d</div><div class="lbl">Total Connections</div></div>
  <div class="card"><div class="val green">%d</div><div class="lbl">Active Swarms</div></div>
  <div class="card"><div class="val green">%d</div><div class="lbl">Total Peers Online</div></div>
  <div class="card"><div class="val">%d</div><div class="lbl">Goroutines</div></div>
  <div class="card"><div class="val">%d</div><div class="lbl">Files Relayed</div></div>
  <div class="card"><div class="val">%.2f MB</div><div class="lbl">Data Received</div></div>
  <div class="card"><div class="val">%.2f MB</div><div class="lbl">Data Sent</div></div>
  <div class="card"><div class="val amber">%d</div><div class="lbl">Conflicts Detected</div></div>
</div>
</body>
</html>`,
			s.UptimeSeconds,
			s.ActiveConns, s.TotalConns,
			s.ActiveSwarms, s.TotalPeers,
			s.Goroutines,
			s.FilesRelayed,
			s.BytesReceivedMB, s.BytesSentMB,
			s.ConflictsTotal,
		)
	})

	fmt.Println("[METRICS] Dashboard at http://localhost:9001/")
	http.ListenAndServe(":9001", mux)
}

var (
	swarms   = make(map[string]*Swarm)
	globalMu sync.RWMutex
)

const MAX_FILE_SIZE = 15 * 1024 * 1024 * 1024

func main() {
	tlsConfig, err := getTLSConfig()
	if err != nil {
		fmt.Printf("[FATAL] TLS config failed: %v\n", err)
		return
	}
	listener, err := tls.Listen("tcp", ":9000", tlsConfig)
	if err != nil {
		fmt.Println("[FATAL] Listener failed:", err)
		return
	}
	defer listener.Close()
	fmt.Println("[SECURE] Relay running on :9000 (TLS 1.3)")

	go startMetricsServer()

	for {
		conn, err := listener.Accept()
		if err != nil {
			continue
		}
		conn.SetDeadline(time.Now().Add(10 * time.Second))
		go handleClient(conn)
	}
}

func getTLSConfig() (*tls.Config, error) {
	if _, err := os.Stat("server.crt"); os.IsNotExist(err) {
		fmt.Println("[SECURE] Generating self-signed cert...")
		if err := generateSelfSignedCert("server.crt", "server.key"); err != nil {
			return nil, err
		}
	}
	cert, err := tls.LoadX509KeyPair("server.crt", "server.key")
	if err != nil {
		return nil, err
	}
	return &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13}, nil
}

func generateSelfSignedCert(certPath, keyPath string) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	tmpl := x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{Organization: []string{"P2P Sync Relay"}, CommonName: "localhost"},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		return err
	}
	cf, _ := os.Create(certPath)
	pem.Encode(cf, &pem.Block{Type: "CERTIFICATE", Bytes: der})
	cf.Close()

	kf, _ := os.OpenFile(keyPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	kb, _ := x509.MarshalECPrivateKey(key)
	pem.Encode(kf, &pem.Block{Type: "EC PRIVATE KEY", Bytes: kb})
	kf.Close()
	return nil
}

func hashSwarmKey(key string) string {
	h := sha256.Sum256([]byte(key))
	return fmt.Sprintf("%x", h)
}

func handleClient(conn net.Conn) {
	defer conn.Close()

	hs, err := readLine(conn)
	if err != nil || !strings.HasPrefix(hs, "ID|") {
		return
	}
	parts := strings.Split(strings.TrimSpace(hs), "|")
	if len(parts) != 3 {
		fmt.Println("[SECURE] Rejected: bad handshake")
		return
	}

	id := parts[1]
	swarmID := hashSwarmKey(parts[2])

	conn.SetDeadline(time.Time{})
	client := &Client{conn: conn, id: id, swarmID: swarmID}

	globalMu.Lock()
	swarm, exists := swarms[swarmID]
	if !exists {
		swarm = &Swarm{
			Clients: make(map[string]*Client),
			Index:   make(map[string]FileRecord),
		}
		swarms[swarmID] = swarm
	}
	globalMu.Unlock()

	swarm.Mu.Lock()
	swarm.Clients[id] = client

	stats.ActiveConns.Add(1)
	stats.TotalConns.Add(1)

	for tid, c := range swarm.Clients {
		if tid != id {
			fmt.Fprintf(c.conn, "PEER|%s|1\n", id)
			fmt.Fprintf(conn, "PEER|%s|1\n", tid)
		}
	}

	swarm.Mu.Unlock()
	fmt.Printf("[SECURE] %s joined swarm %s...\n", id, swarmID[:8])

	defer func() {
		swarm.Mu.Lock()
		delete(swarm.Clients, id)

		stats.ActiveConns.Add(-1)

		for tid, c := range swarm.Clients {
			if tid != id {
				fmt.Fprintf(c.conn, "PEER|%s|0\n", id)
			}
		}

		swarm.Mu.Unlock()
		fmt.Printf("[SECURE] %s left.\n", id)
	}()

	clientIndex := make(map[string]FileRecord)
	indexingDone := false

	for {
		conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		line, err := readLine(conn)
		if err != nil {
			fmt.Printf("[NETWORK] %s disconnected.\n", id)
			break
		}

		cmdParts := strings.SplitN(line, "|", 5)
		cmd := cmdParts[0]

		switch cmd {

		case "PING":
			fmt.Fprintf(conn, "PONG\n")

		case "INDEX":
			if len(cmdParts) < 4 {
				continue
			}
			name := sanitizePath(cmdParts[1])
			size, _ := strconv.ParseInt(cmdParts[2], 10, 64)
			hash := cmdParts[3]
			clientIndex[name] = FileRecord{Size: size, Hash: hash, OwnerID: id}

		case "INDEX_DONE":
			if indexingDone {
				continue
			}
			indexingDone = true

			swarm.Mu.Lock()
			pushMap := make(map[string][]string)

			for name, swarmRecord := range swarm.Index {
				clientRecord, clientHas := clientIndex[name]
				if !clientHas || clientRecord.Hash != swarmRecord.Hash {
					if swarmRecord.OwnerID != id {
						pushMap[swarmRecord.OwnerID] = append(pushMap[swarmRecord.OwnerID], name)
					}
				}
			}
			// Merge this client's index into the swarm index
			for name, rec := range clientIndex {
				if _, exists := swarm.Index[name]; !exists {
					swarm.Index[name] = rec
				}
			}
			type pushJob struct {
				conn  net.Conn
				files []string
			}
			var jobs []pushJob
			for ownerID, files := range pushMap {
				if ownerClient, ok := swarm.Clients[ownerID]; ok {
					jobs = append(jobs, pushJob{conn: ownerClient.conn, files: files})
				}
			}
			swarm.Mu.Unlock()

			for _, job := range jobs {
				for _, fileName := range job.files {
					fmt.Fprintf(job.conn, "PUSH|%s\n", fileName)
					fmt.Printf("[INDEX] Requesting '%s' from owner for new peer.\n", fileName)
				}
			}

			fmt.Fprintf(conn, "INDEX_ACK|\n")

		case "SYNC":
			if len(cmdParts) < 4 {
				continue
			}
			name := sanitizePath(cmdParts[1])
			size, _ := strconv.ParseInt(cmdParts[2], 10, 64)
			hash := cmdParts[3]

			if size > MAX_FILE_SIZE {
				fmt.Printf("[WARN] %s tried to send oversized file.\n", id)
				return
			}

			swarm.Mu.RLock()
			existing, fileKnown := swarm.Index[name]
			swarm.Mu.RUnlock()

			isConflict := fileKnown && existing.Hash != hash && existing.OwnerID != id

			conn.SetReadDeadline(time.Time{})
			spooledPath, err := spoolFile(name, size, conn)
			if err != nil {
				fmt.Printf("[ERROR] Spool failed for %s: %v\n", name, err)
				return
			}

			swarm.Mu.Lock()
			swarm.Index[name] = FileRecord{Size: size, Hash: hash, OwnerID: id}
			swarm.Mu.Unlock()

			swarm.Mu.RLock()
			var targets []*Client
			for tid, c := range swarm.Clients {
				if tid != id {
					targets = append(targets, c)
				}
			}
			senderClient := swarm.Clients[id]
			swarm.Mu.RUnlock()

			if isConflict {
				stats.ConflictsTotal.Add(1)

				fmt.Printf("[CONFLICT] %s — existing=%s... incoming=%s...\n", name, existing.Hash[:8], hash[:8])
				for _, t := range targets {
					fmt.Fprintf(t.conn, "CONFLICT|%s|%s|%s\n", name, existing.Hash, hash)
				}
				if senderClient != nil {
					fmt.Fprintf(senderClient.conn, "CONFLICT|%s|%s|%s\n", name, existing.Hash, hash)
				}
			}

			broadcastFile(targets, name, size, hash, spooledPath)
			os.Remove(spooledPath)

		case "DELETE":
			if len(cmdParts) < 2 {
				continue
			}
			name := sanitizePath(cmdParts[1])

			swarm.Mu.Lock()
			delete(swarm.Index, name)
			swarm.Mu.Unlock()

			swarm.Mu.RLock()
			var targets []net.Conn
			for tid, c := range swarm.Clients {
				if tid != id {
					targets = append(targets, c.conn)
				}
			}
			swarm.Mu.RUnlock()

			for _, t := range targets {
				fmt.Fprintf(t, "DELETE|%s\n", name)
			}
		}
	}
}

func spoolFile(name string, size int64, src net.Conn) (string, error) {
	tmp, err := os.CreateTemp("", "relay-*.tmp")
	if err != nil {
		io.CopyN(io.Discard, src, size)

		return "", err
	}
	path := tmp.Name()

	start := time.Now()
	fmt.Printf("[SERVER] Spooling '%s' (%.2f MB)...\n", name, float64(size)/(1024*1024))
	_, err = io.CopyN(tmp, src, size)
	tmp.Close()

	if err != nil {
		os.Remove(path)
		return "", err
	}

	stats.BytesReceived.Add(size)

	dur := time.Since(start).Seconds()
	if dur > 0 {
		fmt.Printf("[METRIC] Spool %.2fs @ %.2f MB/s\n", dur, (float64(size)/1024/1024)/dur)
	}
	return path, nil
}

func broadcastFile(targets []*Client, fileName string, size int64, hash string, spooledPath string) {
	if len(targets) == 0 {
		fmt.Println("[SERVER] No peers to broadcast to.")
		return
	}

	fmt.Printf("[SERVER] Broadcasting '%s' to %d peer(s)...\n", fileName, len(targets))
	start := time.Now()
	var wg sync.WaitGroup

	for _, target := range targets {
		wg.Add(1)
		go func(c *Client) {
			defer wg.Done()
			f, err := os.Open(spooledPath)
			if err != nil {
				return
			}
			defer f.Close()
			fmt.Fprintf(c.conn, "SYNC|%s|%d|%s\n", fileName, size, hash)
			io.Copy(c.conn, f)
		}(target)
	}

	wg.Wait()

	stats.BytesSent.Add(size * int64(len(targets)))
	stats.FilesRelayed.Add(1)

	dur := time.Since(start).Seconds()
	if dur > 0 {
		fmt.Printf("[METRIC] Fan-out %.2fs @ %.2f MB/s/node\n", dur, (float64(size)/1024/1024)/dur)
	}
}

func sanitizePath(path string) string {
	path = strings.ReplaceAll(path, "..", "")
	path = strings.ReplaceAll(path, "/", "")
	path = strings.ReplaceAll(path, "\\", "")
	return strings.TrimSpace(path)
}

func readLine(conn net.Conn) (string, error) {
	var buf []byte
	one := make([]byte, 1)
	for {
		_, err := conn.Read(one)
		if err != nil {
			return "", err
		}
		if one[0] == '\n' {
			break
		}
		buf = append(buf, one[0])
		if len(buf) > 2048 {
			return "", fmt.Errorf("line too long")
		}
	}
	return string(buf), nil
}
