package main

import (
	"bufio"
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

var UILogCallback func(string)
var UIDisconnectCallback func()

func logMsg(format string, a ...interface{}) {
	msg := fmt.Sprintf(format, a...)
	fmt.Println(msg)
	if UILogCallback != nil {
		UILogCallback(msg)
	}
}

func StartEngine(id, folder, serverIP string) error {
	engineMutex.Lock()
	defer engineMutex.Unlock()

	os.MkdirAll(folder, os.ModePerm)

	conn, err := net.Dial("tcp", serverIP)
	if err != nil {
		logMsg("[CLIENT] Cannot connect to server: %v\n", err)
		return err
	}

	activeConn = conn
	logMsg("[%s] Connected to Relay Server!\nMonitoring folder: %s\n", id, folder)
	fmt.Fprintf(conn, "ID|%s\n", id)

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		logMsg("Watcher error: %v\n", err)
		conn.Close()
		return err
	}
	activeWatcher = watcher

	go receiveLoop(conn, folder)
	go watchAndSend(conn, folder, watcher)

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
	io.Copy(conn, file)
	logMsg("[NETWORK] File pushed to Relay Server!\n")
}

func receiveLoop(conn net.Conn, saveFolder string) {
	reader := bufio.NewReader(conn)
	for {
		header, err := reader.ReadString('\n')
		if err != nil {
			logMsg("\n[NETWORK] Disconnected from server.\n")
			// Trigger UI reset if the server kicks us
			if UIDisconnectCallback != nil {
				UIDisconnectCallback()
			}
			return
		}

		parts := strings.Split(strings.TrimSpace(header), "|")
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
		io.CopyN(outFile, reader, fileSize)
		duration := time.Since(start).Seconds()
		if duration > 0 {
			speed := (float64(fileSize) / (1024 * 1024)) / duration
			logMsg("[NETWORK] Synced in %.2fs (Speed: %.2f MB/s)\n", duration, speed)
		}
		outFile.Close()
		logMsg("[NETWORK] File successfully synced!\n")
	}
}
