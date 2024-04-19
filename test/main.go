package main

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	utils "github.com/johnietre/utils/go"
)

var (
	thisDir, binFile          string
	addr, proxyAddr, srvrAddr string

	// TODO: Do better
	proxyCmd, tunnelCmd *exec.Cmd
	procsWg             sync.WaitGroup

	start   = utils.NewAValue(time.Now())
	failing atomic.Bool
)

func init() {
	_, thisFile, _, _ := runtime.Caller(0)
	thisDir = filepath.Dir(thisFile)
	dir := filepath.Join(filepath.Dir(thisDir))
	binFile = filepath.Join(dir, "bin", "tunnelit")
	checkBinFile(dir)

	addr = envOr("ADDR", "127.0.0.1:17390")
	proxyAddr = envOr("PROXY_ADDR", "127.0.0.1:17391")
	srvrAddr = envOr("PROXY_ADDR", "127.0.0.1:17392")
}

func main() {
	log.SetFlags(0)
	os.Setenv("TUNNELIT_PASSWORD", "thisisatestpassword")
	go runProxy()
	go runServer()

	// Wait for spin up
	time.Sleep(time.Second * 3)
	if failing.Load() {
		// Spin until the program exits
		for {
		}
	}
	go runTunnel()

	// Wait for spin up
	time.Sleep(time.Second * 3)
	if failing.Load() {
		// Spin until the program exits
		for {
		}
	}
	start.Store(time.Now())
	runTests()
	dur := time.Since(start.Load())
	failing.Store(true)
	killAndWait()
	log.Printf("OK: finished in %f seconds", dur.Seconds())
}

func runProxy() {
	procsWg.Add(1)
	cmd := exec.Command(
		binFile, "proxy",
		"--addr", addr,
		"--paddr", proxyAddr,
		"--log", filepath.Join(thisDir, "proxy.log"),
		"--idle-conns", "10",
	)

	buf := bytes.NewBuffer(nil)
	cmd.Stderr = buf

	log.Printf("Starting proxy with --addr=%s and --paddr=%s", addr, proxyAddr)
	if err := cmd.Start(); err != nil {
		procsWg.Done()
		cleanupAndExit("Error starting proxy: ", err)
	}
	proxyCmd = cmd

	if err := cmd.Wait(); err != nil && !failing.Load() {
		log.Printf("Proxy output:\n%s", buf.Bytes())
		procsWg.Done()
		cleanupAndExit("Error running proxy: ", err)
	}
	procsWg.Done()
}

func runTunnel() {
	procsWg.Add(1)
	cmd := exec.Command(
		binFile, "tunnel",
		"--paddr", proxyAddr,
		"--saddr", srvrAddr,
		"--log", filepath.Join(thisDir, "tunnel.log"),
		"--idle-conns", "10",
	)

	buf := bytes.NewBuffer(nil)
	cmd.Stderr = buf

	log.Printf(
		"Starting tunnel with --paddr=%s and --saddr=%s",
		proxyAddr, srvrAddr,
	)
	if err := cmd.Start(); err != nil {
		procsWg.Done()
		cleanupAndExit("Error starting tunnel: ", err)
	}
	tunnelCmd = cmd

	if err := cmd.Wait(); err != nil && !failing.Load() {
		log.Printf("Tunnel output:\n%s", buf.Bytes())
		procsWg.Done()
		cleanupAndExit("Error running tunnel: ", err)
	}
	procsWg.Done()
}

func runServer() {
	log.Print("Starting server on ", srvrAddr)
	ln, err := net.Listen("tcp", srvrAddr)
	if err != nil {
		cleanupAndExit("Error running server: ", err)
	}
	defer ln.Close()
	for {
		conn, err := ln.Accept()
		if err != nil {
			cleanupAndExit("Error running server: ", err)
		}
		go func(conn net.Conn) {
			defer conn.Close()
			var buf [1024]byte
			for {
				n, err := conn.Read(buf[:])
				if err != nil {
					if err != io.EOF {
						cleanupAndExit("Error reading from conn: ", err)
					}
					break
				}
				reverse(buf[:n])
				if _, err := conn.Write(buf[:n]); err != nil {
					cleanupAndExit("Error writing to conn: ", err)
					break
				}
			}
		}(conn)
	}
}

func runTests() {
	log.Print("Starting tests")
	var wg sync.WaitGroup
	ready := make(chan struct{}, 0)
	for i := 0; i < 1_000; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = <-ready
			conn, err := net.Dial("tcp", addr)
			if err != nil {
				cleanupAndExit("Error connecting client to proxy: ", err)
			}
			defer conn.Close()
			var buf [1024]byte
			for i := 1; i <= 1_000; i++ {
				msg := bytes.Repeat([]byte(strconv.Itoa(i*i)), i%100+1)
				if _, err := conn.Write([]byte(msg)); err != nil {
					cleanupAndExit("Error sending client message: ", err)
				}
				want := reverse(clone(msg))
				if n, err := conn.Read(buf[:]); err != nil {
					cleanupAndExit(
						fmt.Sprintf("Error reading (%d) from proxy: %v", i, err),
					)
				} else if !bytes.Equal(buf[:n], want) {
					cleanupAndExit(fmt.Sprintf("Expected %s, got %s", want, buf[:n]))
				}
			}
		}()
	}
	close(ready)
	wg.Wait()
}

func envOr(name, def string) string {
	val := os.Getenv(name)
	if val == "" {
		val = def
	}
	return val
}

func reverse(b []byte) []byte {
	for i, l := 0, len(b); i < l/2; i++ {
		b[i], b[l-i-1] = b[l-i-1], b[i]
	}
	return b
}

func clone(b []byte) []byte {
	buf := make([]byte, len(b))
	copy(buf, b)
	return b
}

func cleanupAndExit(args ...any) {
	if failing.Swap(true) {
		time.Sleep(time.Minute)
	}
	dur := time.Since(start.Load())
	log.Print(args...)
	killAndWait()
	log.Printf("FAIL: finished in %f seconds", dur.Seconds())
	time.Sleep(time.Second)
	os.Exit(1)
}

func killAndWait() {
	var toWait []*exec.Cmd
	if proxyCmd != nil && proxyCmd.Process != nil {
		proxyCmd.Process.Signal(os.Kill)
		toWait = append(toWait, proxyCmd)
	}
	if tunnelCmd != nil && tunnelCmd.Process != nil {
		tunnelCmd.Process.Signal(os.Kill)
		toWait = append(toWait, tunnelCmd)
	}
	procsWg.Wait()
}

func checkBinFile(dir string) {
	for i := 0; i < 2; i++ {
		if _, err := os.Stat(binFile); err == nil {
			return
		}
		if i == 0 {
			cmd := exec.Command("go", "build", "-o", binFile, dir)
			out, err := cmd.CombinedOutput()
			if err != nil {
				log.Print("go build output:\n", out)
				cleanupAndExit("Error building bin: ", err)
			}
		}
	}
	cleanupAndExit("No bin file")
}
