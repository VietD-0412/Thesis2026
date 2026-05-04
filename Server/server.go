package main

import (
	"crypto/tls"
	"fmt"
	"io"
	"net"
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

const MAX_FILE_SIZE = 10 * 1024 * 1024 * 1024

func main() {
	cert, err := tls.LoadX509KeyPair("server.crt", "server.key")
	if err != nil {
		fmt.Printf("[SECURE] Failed to load certificates: %v\n", err)
		return
	}

	config := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS13,
	}

	listener, err := tls.Listen("tcp", ":9000", config)
	if err != nil {
		fmt.Println("[SECURE] Failed to start:", err)
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

	id := parts[1]
	swarmID := parts[2]

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
		conn.SetReadDeadline(time.Now().Add(5 * time.Minute))

		cmdLine, err := readUntilNewline(conn)
		if err != nil {
			break
		}

		cmdParts := strings.Split(cmdLine, "|")
		if cmdParts[0] == "SYNC" && len(cmdParts) == 3 {
			fileName := sanitizePath(cmdParts[1])
			fileSize, _ := strconv.ParseInt(cmdParts[2], 10, 64)

			if fileSize > MAX_FILE_SIZE {
				fmt.Printf("[WARN] %s tried to send too large a file.\n", id)
				return
			}

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
	globalMu.RLock()
	swarm, exists := swarms[swarmID]
	globalMu.RUnlock()

	if !exists {
		io.CopyN(io.Discard, senderConn, size)
		return
	}

	swarm.Mu.RLock()
	var targets []io.Writer
	for id, c := range swarm.Clients {
		if id != senderID {
			fmt.Fprintf(c.conn, "SYNC|%s|%d\n", fileName, size)
			targets = append(targets, c.conn)
		}
	}
	swarm.Mu.RUnlock()

	if len(targets) > 0 {
		mw := io.MultiWriter(targets...)
		io.CopyN(mw, senderConn, size)
	} else {
		io.CopyN(io.Discard, senderConn, size)
	}
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
