package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
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
	OwnerID string // last edited client
}

type Swarm struct {
	Clients map[string]*Client
	Index   map[string]FileRecord // filename == latest known version
	Mu      sync.RWMutex
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
	swarm.Mu.Unlock()

	fmt.Printf("[SECURE] %s joined swarm %s…\n", id, swarmID[:8])

	defer func() {
		swarm.Mu.Lock()
		delete(swarm.Clients, id)
		swarm.Mu.Unlock()
		fmt.Printf("[SECURE] %s left.\n", id)
	}()

	// INDEX msgs Tracking(until this is INDEX_DONE)
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
			// INDEX|filename|size|hash
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

			// Client's and Swarm's Index merged
			swarm.Mu.Lock()
			var clientNeeds []string
			for name, swarmRecord := range swarm.Index {
				clientRecord, clientHas := clientIndex[name]
				if !clientHas || clientRecord.Hash != swarmRecord.Hash {
					clientNeeds = append(clientNeeds, name)
				}
			}

			for name, rec := range clientIndex {
				swarm.Index[name] = rec
			}
			swarm.Mu.Unlock()

			// Tell Client which file is needed to send
			if len(clientNeeds) == 0 {
				fmt.Fprintf(conn, "INDEX_ACK|\n")
				fmt.Printf("[INDEX] %s is up to date.\n", id)
			} else {
				fmt.Fprintf(conn, "INDEX_ACK|%s\n", strings.Join(clientNeeds, ","))
				fmt.Printf("[INDEX] %s needs %d file(s) from swarm.\n", id, len(clientNeeds))
			}

		case "SYNC":
			// SYNC|filename|size|hash
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

			// Conflict detection(swarm has file w/ a diff hash +
			// sender dont have current version)
			existing, fileKnown := swarm.Index[name]
			swarm.Mu.RUnlock()

			isConflict := fileKnown && existing.Hash != hash && existing.OwnerID != id

			conn.SetReadDeadline(time.Time{})
			spooledPath, err := spoolFile(name, size, conn)
			if err != nil {
				fmt.Printf("[ERROR] Spool failed for %s: %v\n", name, err)
				return
			}

			// Update swarm index
			swarm.Mu.Lock()
			swarm.Index[name] = FileRecord{Size: size, Hash: hash, OwnerID: id}
			swarm.Mu.Unlock()

			if isConflict {
				fmt.Printf("[CONFLICT] %s — existing=%s… incoming=%s…\n", name, existing.Hash[:8], hash[:8])
				broadcastConflict(swarm, id, name, existing.Hash, hash)
			}

			broadcastFile(swarm, id, name, size, hash, spooledPath)
			os.Remove(spooledPath)

		case "DELETE":
			if len(cmdParts) < 2 {
				continue
			}
			name := sanitizePath(cmdParts[1])
			swarm.Mu.Lock()
			delete(swarm.Index, name)
			swarm.Mu.Unlock()
			broadcastDelete(swarm, id, name)
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
	// Spooling metrics
	dur := time.Since(start).Seconds()
	fmt.Printf("[METRIC] Spool %.2fs @ %.2f MB/s\n", dur, (float64(size)/1024/1024)/dur)
	return path, nil
}

func broadcastFile(swarm *Swarm, senderID, fileName string, size int64, hash string, spooledPath string) {
	swarm.Mu.RLock()
	var targets []net.Conn
	for id, c := range swarm.Clients {
		if id != senderID {
			targets = append(targets, c.conn)
		}
	}
	swarm.Mu.RUnlock()

	if len(targets) == 0 {
		fmt.Println("[SERVER] No peers to broadcast to.")
		return
	}

	fmt.Printf("[SERVER] Broadcasting '%s' to %d peer(s)...\n", fileName, len(targets))
	start := time.Now()
	var wg sync.WaitGroup

	for _, target := range targets {
		wg.Add(1)
		go func(conn net.Conn) {
			defer wg.Done()
			f, err := os.Open(spooledPath)
			if err != nil {
				return
			}
			defer f.Close()
			// Hash included in SYNC(verify both side)
			fmt.Fprintf(conn, "SYNC|%s|%d|%s\n", fileName, size, hash)
			io.Copy(conn, f)
		}(target)
	}

	wg.Wait()
	dur := time.Since(start).Seconds()
	fmt.Printf("[METRIC] Fan-out %.2fs @ %.2f MB/s/node\n", dur, (float64(size)/1024/1024)/dur)
}

func broadcastConflict(swarm *Swarm, senderID, fileName, existingHash, incomingHash string) {
	swarm.Mu.RLock()
	defer swarm.Mu.RUnlock()
	for id, c := range swarm.Clients {
		if id != senderID {
			fmt.Fprintf(c.conn, "CONFLICT|%s|%s|%s\n", fileName, existingHash, incomingHash)
		}
	}
	if sender, ok := swarm.Clients[senderID]; ok {
		fmt.Fprintf(sender.conn, "CONFLICT|%s|%s|%s\n", fileName, existingHash, incomingHash)
	}
}

func broadcastDelete(swarm *Swarm, senderID, fileName string) {
	swarm.Mu.RLock()
	defer swarm.Mu.RUnlock()
	for id, c := range swarm.Clients {
		if id != senderID {
			fmt.Fprintf(c.conn, "DELETE|%s\n", fileName)
		}
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
