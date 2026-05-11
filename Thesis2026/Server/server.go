package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
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

type Swarm struct {
	Clients map[string]*Client
	Mu      sync.RWMutex
}

var (
	swarms   = make(map[string]*Swarm)
	globalMu sync.RWMutex
)

const MAX_FILE_SIZE = 15 * 1024 * 1024 * 1024 // Current aim: 15gb

func main() {
	tlsConfig, err := getTLSConfig()
	if err != nil {
		fmt.Printf("[FATAL] Failed to configure TLS: %v\n", err)
		return
	}

	listener, err := tls.Listen("tcp", ":9000", tlsConfig)
	if err != nil {
		fmt.Println("[FATAL] Failed to start listener:", err)
		return
	}
	defer listener.Close()

	fmt.Println("[SECURE] Encrypted Relay running on port 9000")

	for {
		conn, err := listener.Accept()
		if err != nil {
			continue
		}
		conn.SetDeadline(time.Now().Add(10 * time.Second))
		go handleClient(conn)
	}
}

// TLS Cert gen
func getTLSConfig() (*tls.Config, error) {
	certFile := "server.crt"
	keyFile := "server.key"

	if _, err := os.Stat(certFile); os.IsNotExist(err) {
		fmt.Println("[SECURE] No certificates found. Generating new self-signed pair...")
		if err := generateSelfSignedCert(certFile, keyFile); err != nil {
			return nil, err
		}
	}

	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, err
	}

	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS13,
	}, nil
}

func generateSelfSignedCert(certOutPath, keyOutPath string) error {
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}

	serialNumber, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	template := x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			Organization: []string{"P2P Sync Engine Relay"},
			CommonName:   "localhost",
		},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{"localhost"},
	}

	derBytes, err := x509.CreateCertificate(rand.Reader, &template, &template, &privateKey.PublicKey, privateKey)
	if err != nil {
		return err
	}

	certOut, err := os.Create(certOutPath)
	if err != nil {
		return err
	}
	pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: derBytes})
	certOut.Close()

	keyOut, err := os.OpenFile(keyOutPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	privBytes, _ := x509.MarshalECPrivateKey(privateKey)
	pem.Encode(keyOut, &pem.Block{Type: "EC PRIVATE KEY", Bytes: privBytes})
	keyOut.Close()

	return nil
}

func handleClient(conn net.Conn) {
	defer conn.Close()

	handshake, err := readUntilNewline(conn)
	if err != nil || !strings.HasPrefix(handshake, "ID|") {
		return
	}

	parts := strings.Split(strings.TrimSpace(handshake), "|")
	if len(parts) != 3 {
		fmt.Println("[SECURE] Rejected connection: Invalid handshake format.")
		return
	}

	id, swarmID := parts[1], parts[2]

	conn.SetDeadline(time.Time{})
	clientObj := &Client{conn: conn, id: id, swarmID: swarmID}

	globalMu.Lock()
	swarm, exists := swarms[swarmID]
	if !exists {
		swarm = &Swarm{Clients: make(map[string]*Client)}
		swarms[swarmID] = swarm
	}
	globalMu.Unlock()

	swarm.Mu.Lock()
	swarm.Clients[id] = clientObj
	swarm.Mu.Unlock()

	fmt.Printf("[SECURE] %s joined swarm '%s'.\n", id, swarmID)

	defer func() {
		swarm.Mu.Lock()
		delete(swarm.Clients, id)
		swarm.Mu.Unlock()
		fmt.Printf("[SECURE] %s left swarm '%s'.\n", id, swarmID)
	}()

	for {
		// 60sec timeout
		conn.SetReadDeadline(time.Now().Add(60 * time.Second))

		cmdLine, err := readUntilNewline(conn)
		if err != nil {
			fmt.Printf("[NETWORK] %s disconnected or timed out.\n", id)
			break
		}

		cmdParts := strings.Split(cmdLine, "|")

		if cmdParts[0] == "PING" {
			fmt.Fprintf(conn, "PONG\n")
			continue
		}

		if cmdParts[0] == "SYNC" && len(cmdParts) == 3 {
			fileName := sanitizePath(cmdParts[1])
			fileSize, _ := strconv.ParseInt(cmdParts[2], 10, 64)

			if fileSize > MAX_FILE_SIZE {
				fmt.Printf("[WARN] %s tried to send too large a file.\n", id)
				return
			}

			conn.SetReadDeadline(time.Time{})
			broadcastFile(swarmID, id, fileName, fileSize, conn)
		}
	}
}

func sanitizePath(path string) string {
	path = strings.ReplaceAll(path, "..", "")
	path = strings.ReplaceAll(path, "/", "")
	path = strings.ReplaceAll(path, "\\", "")
	return path
}

func broadcastFile(swarmID string, senderID string, fileName string, size int64, senderConn net.Conn) {
	tempFile, err := os.CreateTemp("", "relay-*.tmp")
	if err != nil {
		fmt.Println("[ERROR] Cannot create temp spool file:", err)
		io.CopyN(io.Discard, senderConn, size)
		return
	}
	tempFileName := tempFile.Name()
	defer os.Remove(tempFileName)

	// Spooling time check
	spoolStart := time.Now()
	fmt.Printf("[SERVER] Spooling '%s' (%.2f MB) to disk...\n", fileName, float64(size)/(1024*1024))

	_, err = io.CopyN(tempFile, senderConn, size)
	tempFile.Close()

	spoolDuration := time.Since(spoolStart).Seconds()
	if err != nil {
		fmt.Printf("[ERROR] Sender %s dropped connection during upload: %v\n", senderID, err)
		return
	}

	spoolSpeed := (float64(size) / (1024 * 1024)) / spoolDuration
	fmt.Printf("[METRIC] Spool Complete: %.2fs at %.2f MB/s\n", spoolDuration, spoolSpeed)

	globalMu.RLock()
	swarm, exists := swarms[swarmID]
	globalMu.RUnlock()

	if !exists {
		return
	}

	swarm.Mu.RLock()
	var targets []net.Conn
	for id, c := range swarm.Clients {
		if id != senderID {
			targets = append(targets, c.conn)
		}
	}
	swarm.Mu.RUnlock()

	if len(targets) == 0 {
		fmt.Println("[SERVER] No peers available. File dropped.")
		return
	}

	// Metric Logs
	fmt.Printf("[SERVER] Broadcasting '%s' to %d peers...\n", fileName, len(targets))
	fanOutStart := time.Now()
	var wg sync.WaitGroup

	for _, targetConn := range targets {
		wg.Add(1)
		go func(conn net.Conn) {
			defer wg.Done()
			f, err := os.Open(tempFileName)
			if err != nil {
				return
			}
			defer f.Close()

			fmt.Fprintf(conn, "SYNC|%s|%d\n", fileName, size)
			io.Copy(conn, f)
		}(targetConn)
	}

	wg.Wait()
	fanOutDuration := time.Since(fanOutStart).Seconds()
	fanOutSpeed := (float64(size) / (1024 * 1024)) / fanOutDuration
	fmt.Printf("[METRIC] Fan-Out Complete: %.2fs at %.2f MB/s per node.\n", fanOutDuration, fanOutSpeed)
}

func readUntilNewline(conn net.Conn) (string, error) {
	var result []byte
	one := make([]byte, 1)
	for {
		_, err := conn.Read(one)
		if err != nil {
			return "", err
		}
		if one[0] == '\n' {
			break
		}
		result = append(result, one[0])
		if len(result) > 1024 {
			return "", fmt.Errorf("line too long")
		}
	}
	return string(result), nil
}
