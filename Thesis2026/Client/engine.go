package main

import (
	"bufio"
	"crypto/tls"
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
		progress := float64(pr.Readed) / float64(pr.Total)
		pr.Callback(progress) // Fyne progress bars expect a float between 0.0 and 1.0
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
			logMsg("[SYSTEM] Heartbeat failed, connection likely dead.\n")
			return
		}
	}
}

func StartEngine(id, folder, serverIP, swarmKey string) error {
	engineMutex.Lock()
	defer engineMutex.Unlock()

	os.MkdirAll(folder, os.ModePerm)

	// TLS Setup
	tlsConfig := &tls.Config{
		InsecureSkipVerify: true, //Only for testing, comment when production
		VerifyConnection: func(cs tls.ConnectionState) error {
			// Production
			// hash := sha256.Sum256(cs.PeerCertificates[0].Raw)
			// if hex.EncodeToString(hash[:]) != EXPECTED_HASH { return errors.New("cert mismatch") }
			return nil
		},
	}

	conn, err := tls.Dial("tcp", serverIP, tlsConfig)
	if err != nil {
		logMsg("[CLIENT] Cannot connect to server: %v\n", err)
		return err
	}

	activeConn = conn
	logMsg("[%s] Connected to Relay Server!\nMonitoring folder: %s\n", id, folder)
	fmt.Fprintf(conn, "ID|%s|%s\n", id, swarmKey)

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		logMsg("Watcher error: %v\n", err)
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
	logMsg("\n[SYSTEM] Sync engine completely stopped.\n")
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
				if strings.Contains(fileName, "~") {
					continue
				}
				if _, recentlyDownloaded := downloadCache.Load(fileName); recentlyDownloaded {
					downloadCache.Delete(fileName)
					continue
				}
				logMsg("\n[WATCHER] Local change detected: %s", fileName)
				sendFile(conn, event.Name)
			}
			if event.Op&fsnotify.Remove == fsnotify.Remove {
				fileName := filepath.Base(event.Name)
				logMsg("\n[WATCHER] Local deletion detected: %s", fileName)
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
		logMsg("[NETWORK] Skipped: File locked by OS.\n")
		return
	}
	defer file.Close()

	header := fmt.Sprintf("SYNC|%s|%d\n", info.Name(), info.Size())
	fmt.Fprintf(conn, "%s", header)

	pr := &ProgressReader{
		Reader:   file,
		Total:    info.Size(),
		Callback: UIProgressCallback,
	}
	io.Copy(conn, pr)

	if UIProgressCallback != nil {
		UIProgressCallback(0)
	}

	logMsg("[NETWORK] File pushed to Relay Server!\n")
}

func receiveLoop(conn net.Conn, saveFolder string) {
	reader := bufio.NewReader(conn)
	for {
		header, err := reader.ReadString('\n')
		if err != nil {
			logMsg("\n[NETWORK] Disconnected from server.\n")
			if UIDisconnectCallback != nil {
				UIDisconnectCallback()
			}
			return
		}

		parts := strings.Split(strings.TrimSpace(header), "|")
		if parts[0] == "PONG" {
			continue
		}
		if parts[0] == "DELETE" && len(parts) == 2 {
			fileName := parts[1]
			deletePath := filepath.Join(saveFolder, fileName)
			logMsg("\n[NETWORK] Network delete request for: %s", fileName)
			if err := os.Remove(deletePath); err == nil {
				logMsg("[NETWORK] File successfully deleted locally.\n")
			}
			continue
		}

		if len(parts) != 3 || parts[0] != "SYNC" {
			continue
		}

		fileName := parts[1]
		fileSize, _ := strconv.ParseInt(parts[2], 10, 64)
		logMsg("\n[NETWORK] Downloading incoming file: %s...", fileName)

		downloadCache.Store(fileName, true)
		outPath := filepath.Join(saveFolder, fileName)
		outFile, err := os.Create(outPath)
		if err != nil {
			logMsg("[NETWORK] Error creating file: %v\n", err)
			continue
		}

		start := time.Now()

		pr := &ProgressReader{
			Reader:   reader,
			Total:    fileSize,
			Callback: UIProgressCallback,
		}
		io.CopyN(outFile, pr, fileSize)

		if UIProgressCallback != nil {
			UIProgressCallback(0)
		}

		duration := time.Since(start).Seconds()
		if duration > 0 {
			speed := (float64(fileSize) / (1024 * 1024)) / duration
			logMsg("[NETWORK] Synced in %.2fs (Speed: %.2f MB/s)\n", duration, speed)
		}
		outFile.Close()
		logMsg("[NETWORK] File successfully synced!\n")
	}
}
