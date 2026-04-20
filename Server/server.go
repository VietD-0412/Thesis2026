package main

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
)

var (
	clients = make(map[string]net.Conn)
	mu      sync.Mutex
)

func main() {
	port := "9000"
	listener, err := net.Listen("tcp", ":"+port)
	if err != nil {
		fmt.Println("[SERVER] Failed to start:", err)
		return
	}
	defer listener.Close()

	fmt.Printf("[SERVER] Relay running on port %s. Waiting for clients...\n", port)

	for {
		conn, err := listener.Accept()
		if err != nil {
			continue
		}
		go handleClient(conn)
	}
}

func handleClient(conn net.Conn) {
	reader := bufio.NewReader(conn)

	idMsg, err := reader.ReadString('\n')
	if err != nil {
		conn.Close()
		return
	}

	idParts := strings.Split(strings.TrimSpace(idMsg), "|")
	if len(idParts) != 2 || idParts[0] != "ID" {
		conn.Close()
		return
	}
	clientID := idParts[1]

	mu.Lock()
	clients[clientID] = conn
	mu.Unlock()
	fmt.Printf("[SERVER] %s connected. Total machines: %d\n", clientID, len(clients))

	defer func() {
		mu.Lock()
		delete(clients, clientID)
		mu.Unlock()
		conn.Close()
		fmt.Printf("[SERVER] %s disconnected.\n", clientID)
	}()

	for {
		header, err := reader.ReadString('\n')
		if err != nil {
			return
		}

		parts := strings.Split(strings.TrimSpace(header), "|")
		if parts[0] == "DELETE" && len(parts) == 2 {
			fileName := parts[1]
			fmt.Printf("[SERVER] Received DELETE for '%s' from %s. Broadcasting...\n", fileName, clientID)

			mu.Lock()
			for targetID, targetConn := range clients {
				if targetID != clientID {
					fmt.Fprintf(targetConn, "DELETE|%s\n", fileName)
				}
			}
			mu.Unlock()
			continue
		}

		if len(parts) != 3 || parts[0] != "SYNC" {
			continue
		}

		fileName := parts[1]
		fileSize, _ := strconv.ParseInt(parts[2], 10, 64)

		fmt.Printf("[SERVER] Received '%s' from %s. Broadcasting...\n", fileName, clientID)

		fileData := make([]byte, fileSize)
		_, err = io.ReadFull(reader, fileData)
		if err != nil {
			fmt.Println("[SERVER] Error reading file data:", err)
			continue
		}

		mu.Lock()
		for targetID, targetConn := range clients {
			if targetID != clientID {
				// Header
				fmt.Fprintf(targetConn, "SYNC|%s|%d\n", fileName, fileSize)
				// Data
				targetConn.Write(fileData)
			}
		}
		mu.Unlock()
	}
}
