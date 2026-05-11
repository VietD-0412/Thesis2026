package main

import (
	"bufio"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

var downloadCache sync.Map
var localIndex sync.Map

var (
	activeConn    net.Conn
	activeWatcher *fsnotify.Watcher
	engineMutex   sync.Mutex
)

var UIProgressCallback func(float64)
var UILogCallback func(string)
var UIDisconnectCallback func()

type ProgressReader struct {
	Reader   io.Reader
	Total    int64
	Readed   int64
	Callback func(float64)
}

func (pr *ProgressReader) Read(p []byte) (int, error) {
	n, err := pr.Reader.Read(p)
	pr.Readed += int64(n)
	if pr.Total > 0 && pr.Callback != nil {
		pr.Callback(float64(pr.Readed) / float64(pr.Total))
	}
	return n, err
}

func logMsg(format string, a ...interface{}) {
	msg := fmt.Sprintf(format, a...)
	fmt.Println(msg)
	if UILogCallback != nil {
		UILogCallback(msg)
	}
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func sendHeartbeats() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		<-ticker.C
		engineMutex.Lock()
		if activeConn == nil {
			engineMutex.Unlock()
			return
		}
		_, err := fmt.Fprintf(activeConn, "PING\n")
		engineMutex.Unlock()
		if err != nil {
			logMsg("[SYSTEM] Heartbeat failed.\n")
			return
		}
	}
}

func buildAndSendIndex(conn net.Conn, folderPath string) {
	entries, err := os.ReadDir(folderPath)
	if err != nil {
		logMsg("[INDEX] Cannot read folder: %v\n", err)
		return
	}
	for _, e := range entries {
		if e.IsDir() || strings.Contains(e.Name(), "~") {
			continue
		}
		fullPath := filepath.Join(folderPath, e.Name())
		info, err := e.Info()
		if err != nil {
			continue
		}
		hash, err := hashFile(fullPath)
		if err != nil {
			continue
		}
		localIndex.Store(e.Name(), hash)
		fmt.Fprintf(conn, "INDEX|%s|%d|%s\n", e.Name(), info.Size(), hash)
	}
	fmt.Fprintf(conn, "INDEX_DONE\n")
	logMsg("[INDEX] Local index sent to relay.\n")
}

func StartEngine(id, folder, serverIP, swarmKey string) error {
	engineMutex.Lock()
	defer engineMutex.Unlock()

	os.MkdirAll(folder, os.ModePerm)

	tlsConfig := &tls.Config{
		InsecureSkipVerify: true, // IMPORTATANT!!!:cert hash when production, not this
	}

	conn, err := tls.Dial("tcp", serverIP, tlsConfig)
	if err != nil {
		logMsg("[CLIENT] Cannot connect: %v\n", err)
		return err
	}

	activeConn = conn
	logMsg("[%s] Connected!\nMonitoring: %s\n", id, folder)
	fmt.Fprintf(conn, "ID|%s|%s\n", id, swarmKey)

	go buildAndSendIndex(conn, folder)

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		conn.Close()
		return err
	}
	activeWatcher = watcher

	go receiveLoop(conn, folder)
	go watchAndSend(conn, folder, watcher)
	go sendHeartbeats()

	return nil
}

func StopEngine() {
	engineMutex.Lock()
	defer engineMutex.Unlock()
	if activeConn != nil {
		activeConn.Close()
		activeConn = nil
	}
	if activeWatcher != nil {
		activeWatcher.Close()
		activeWatcher = nil
	}
	logMsg("\n[SYSTEM] Stopped.\n")
}

func watchAndSend(conn net.Conn, folderPath string, watcher *fsnotify.Watcher) {
	watcher.Add(folderPath)
	for {
		select {
		case event, ok := <-watcher.Events:
			if !ok {
				return
			}
			if event.Op&fsnotify.Write == fsnotify.Write || event.Op&fsnotify.Create == fsnotify.Create {
				fileName := filepath.Base(event.Name)
				if strings.Contains(fileName, "~") || strings.HasSuffix(fileName, ".tmp") {
					continue
				}
				if _, skip := downloadCache.Load(fileName); skip {
					downloadCache.Delete(fileName)
					continue
				}
				logMsg("\n[WATCHER] Change detected: %s", fileName)
				sendFile(conn, event.Name)
			}
			if event.Op&fsnotify.Remove == fsnotify.Remove {
				fileName := filepath.Base(event.Name)
				localIndex.Delete(fileName)
				logMsg("\n[WATCHER] Deleted locally: %s", fileName)
				fmt.Fprintf(conn, "DELETE|%s\n", fileName)
			}
		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			logMsg("Watcher error: %v\n", err)
		}
	}
}

func sendFile(conn net.Conn, filePath string) {
	info, err := os.Stat(filePath)
	if err != nil || info.IsDir() {
		return
	}

	var file *os.File
	for i := 0; i < 5; i++ {
		file, err = os.Open(filePath)
		if err == nil {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if err != nil {
		logMsg("[NETWORK] Skipped: file locked.\n")
		return
	}
	defer file.Close()

	hash, err := hashFile(filePath)
	if err != nil {
		logMsg("[NETWORK] Hash failed: %v\n", err)
		return
	}

	if stored, ok := localIndex.Load(info.Name()); ok && stored.(string) == hash {
		return
	}
	localIndex.Store(info.Name(), hash)

	fmt.Fprintf(conn, "SYNC|%s|%d|%s\n", info.Name(), info.Size(), hash)

	pr := &ProgressReader{Reader: file, Total: info.Size(), Callback: UIProgressCallback}
	io.Copy(conn, pr)

	if UIProgressCallback != nil {
		UIProgressCallback(0)
	}
	logMsg("[NETWORK] File pushed: %s\n", info.Name())
}

func receiveLoop(conn net.Conn, saveFolder string) {
	reader := bufio.NewReader(conn)
	for {
		header, err := reader.ReadString('\n')
		if err != nil {
			logMsg("\n[NETWORK] Disconnected.\n")
			if UIDisconnectCallback != nil {
				UIDisconnectCallback()
			}
			return
		}

		parts := strings.Split(strings.TrimSpace(header), "|")
		cmd := parts[0]

		switch cmd {
		case "PONG":
			continue

		case "DELETE":
			if len(parts) < 2 {
				continue
			}
			target := filepath.Join(saveFolder, parts[1])
			logMsg("\n[NETWORK] Remote delete: %s", parts[1])
			localIndex.Delete(parts[1])
			os.Remove(target)

		case "CONFLICT":
			// CONFLICT|filename|localHash|remoteHash
			if len(parts) < 4 {
				continue
			}
			logMsg("\n[CONFLICT] %s — local=%s… remote=%s…\n", parts[1], parts[2][:8], parts[3][:8])
			logMsg("[CONFLICT] Remote version incoming; local copy renamed to *.conflict\n")
			existing := filepath.Join(saveFolder, parts[1])
			conflictCopy := existing + ".conflict"
			os.Rename(existing, conflictCopy)

		case "SYNC":
			// SYNC|filename|size|hash
			if len(parts) < 4 {
				continue
			}
			fileName := parts[1]
			fileSize, _ := strconv.ParseInt(parts[2], 10, 64)
			remoteHash := parts[3]

			if stored, ok := localIndex.Load(fileName); ok && stored.(string) == remoteHash {
				logMsg("[NETWORK] Already have %s, skipping.\n", fileName)
				io.CopyN(io.Discard, reader, fileSize)
				continue
			}

			logMsg("\n[NETWORK] Receiving: %s (%d bytes)", fileName, fileSize)
			downloadCache.Store(fileName, true)

			// If temp file {rename}
			tmpPath := filepath.Join(saveFolder, fileName+".tmp")
			outFile, err := os.Create(tmpPath)
			if err != nil {
				logMsg("[NETWORK] Cannot create temp file: %v\n", err)
				io.CopyN(io.Discard, reader, fileSize)
				continue
			}

			start := time.Now()
			pr := &ProgressReader{Reader: reader, Total: fileSize, Callback: UIProgressCallback}
			written, err := io.CopyN(outFile, pr, fileSize)
			outFile.Close()

			if err != nil || written != fileSize {
				logMsg("[NETWORK] Incomplete receive, discarding.\n")
				os.Remove(tmpPath)
				continue
			}

			// Verify integrity
			receivedHash, _ := hashFile(tmpPath)
			if receivedHash != remoteHash {
				logMsg("[NETWORK] Hash mismatch! File corrupted, discarding.\n")
				os.Remove(tmpPath)
				continue
			}

			finalPath := filepath.Join(saveFolder, fileName)
			os.Rename(tmpPath, finalPath)
			localIndex.Store(fileName, remoteHash)

			if UIProgressCallback != nil {
				UIProgressCallback(0)
			}

			dur := time.Since(start).Seconds()
			if dur > 0 {
				speed := (float64(fileSize) / (1024 * 1024)) / dur
				logMsg("[NETWORK] Synced %s in %.2fs (%.2f MB/s)\n", fileName, dur, speed)
			}

		case "INDEX_ACK":
			// INDEX_ACK|filename1,filename2,...
			if len(parts) < 2 || parts[1] == "" {
				continue
			}
			needed := strings.Split(parts[1], ",")
			logMsg("[INDEX] Remote peers need %d file(s) from us.\n", len(needed))
			go func() {
				for _, name := range needed {
					engineMutex.Lock()
					c := activeConn
					engineMutex.Unlock()
					if c == nil {
						return
					}
					fullPath := filepath.Join(saveFolder, strings.TrimSpace(name))
					sendFile(c, fullPath)
				}
			}()
		}
	}
}
