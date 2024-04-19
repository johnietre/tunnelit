package main

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"time"

	"github.com/johnietre/utils/go"
	"github.com/spf13/cobra"
)

var (
	maxIdleConns uint = 10
	passwordHash [sha256.Size]byte
	logFile      string
)

const passwordEnvName = "TUNNELIT_PASSWORD"

const (
	connReady       byte = 1
	passwordInvalid byte = 10
	passwordOk      byte = 11
)

func main() {
	log.SetFlags(0)

	rootCmd := &cobra.Command{
		Use:   "tunnelit",
		Short: "A tunnel/proxy useful for proxying from a server with a static address to one without",
		Long: `A tunnel/proxy program. This is most useful for when it is desired to proxy from a static IP to a non-static IP.
This acts as the intermediary between some machine with a static IP and a server running on a machine without a static IP.
When starting either the tunnel or proxy, a password is sent/checked for each new tunnel connection.
The password can be set using the ` + passwordEnvName + ` environment variable.`,
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			if maxIdleConns == 0 {
				return fmt.Errorf("iddle-conns must be greater than 0")
			}
			readyCh = make(chan utils.Unit, maxIdleConns)
			for i := 0; i < int(maxIdleConns); i++ {
				readyCh <- utils.Unit{}
			}
			if logFile != "" {
				f, err := utils.OpenAppend(logFile)
				if err != nil {
					return err
				}
				log.SetOutput(f)
			}
			pwd := os.Getenv(passwordEnvName)
			passwordHash = sha256.Sum256([]byte(pwd))
			return nil
		},
	}
	rootCmd.PersistentFlags().UintVar(
		&maxIdleConns, "idle-conns", 10,
		"Maximum number of idle conns (must be greater than 0)",
	)
	rootCmd.PersistentFlags().StringVar(
		&logFile, "log", "", "File to log to (blank means stderr)",
	)

	proxyCmd := &cobra.Command{
		Use:   "proxy",
		Short: "Start proxy server that clients connect to",
		Long: `Start the proxy server that clients and tunneling servers can connect to.
This is usually be run on the machine with the static IP. The addresses passed to the "addr" and "paddr" flags are usually bound to static addresses.`,
		Run: RunProxy,
	}
	proxyCmd.Flags().String("addr", "", "Address to listen for clients on")
	proxyCmd.Flags().String("paddr", "", "Address to listen for tunnels on")
	proxyCmd.MarkFlagRequired("addr")
	proxyCmd.MarkFlagRequired("paddr")

	tunnelCmd := &cobra.Command{
		Use:   "tunnel",
		Short: "Connect to remote proxy server and pipe to another server",
		Long: `Connect to a remote tunnelit proxy server and pipe connections from the proxy server clients to the other given server.
This is usually run on the machine without a static IP. The address passed to the "saddr" is usually a local IP.`,
		Run: RunTunnel,
	}
	tunnelCmd.Flags().String(
		"paddr", "",
		"Address of tunnelit server to tunnel to",
	)
	tunnelCmd.Flags().String("saddr", "", "Address of server to pipe to")
	tunnelCmd.MarkFlagRequired("paddr")
	tunnelCmd.MarkFlagRequired("saddr")

	rootCmd.AddCommand(proxyCmd, tunnelCmd)

	cobra.CheckErr(rootCmd.Execute())
}

var (
	idleConns chan net.Conn
)

func RunProxy(cmd *cobra.Command, args []string) {
	addr := must(cmd.Flags().GetString("addr"))
	proxyAddr := must(cmd.Flags().GetString("paddr"))

	if addr == "" || proxyAddr == "" {
		log.Fatal(`Must provide "addr" and "paddr"`)
	}

	idleConns = make(chan net.Conn, maxIdleConns)

	log.Printf("Listening for clients on %s and tunnels on %s", addr, proxyAddr)
	go listenProxy(proxyAddr)

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatal("Error listening: ", err)
	}
	for {
		conn, err := ln.Accept()
		if err != nil {
			log.Fatal("Error accepting: ", err)
		}
		go handleClientConn(conn)
	}
}

func listenProxy(proxyAddr string) {
	ln, err := net.Listen("tcp", proxyAddr)
	if err != nil {
		log.Fatal("Error starting proxy listener: ", err)
	}
	for range readyCh {
		conn, err := ln.Accept()
		if err != nil {
			log.Fatal("Error accepting proxy conn: ", err)
		}
		go handleProxyConn(conn)
	}
}

var (
	idleTimeout = time.Second * 10
)

func handleClientConn(clientConn net.Conn) {
	closeClientConn := utils.NewT(true)
	defer deferredClose(clientConn, closeClientConn)

	// Wait for idle conn
	var proxyConn net.Conn
	timer := time.NewTimer(idleTimeout)
	select {
	case <-timer.C:
		return
	case proxyConn = <-idleConns:
	}
	if !timer.Stop() {
		<-timer.C
	}
	closeProxyConn := utils.NewT(true)
	defer deferredClose(clientConn, closeProxyConn)

	// Signal that another idle conn can be accepted
	readyCh <- utils.Unit{}

	// Notify the proxy conn that it's ready and wait for ready status
	if _, err := proxyConn.Write([]byte{connReady}); err != nil {
		return
	}
	// TODO: Timeout?
	b := []byte{0}
	if _, err := proxyConn.Read(b); err != nil {
		return
	} else if b[0] != connReady {
		log.Printf(
			"Received unexpected response from tunnel, expected %d, got %d",
			connReady, b[0],
		)
		return
	}
	*closeClientConn, *closeProxyConn = false, false

	go pipe(clientConn, proxyConn)
	pipe(proxyConn, clientConn)
}

func handleProxyConn(conn net.Conn) {
	conn.SetDeadline(time.Now().Add(idleTimeout))
	var b [sha256.Size]byte
	if _, err := io.ReadFull(conn, b[:]); err != nil {
		conn.Close()
		return
	} else if !bytes.Equal(b[:], passwordHash[:]) {
		conn.Write([]byte{passwordInvalid})
		conn.Close()
		return
	}
	if _, err := conn.Write([]byte{passwordOk}); err != nil {
		conn.Close()
		return
	}
	idleConns <- conn
	conn.SetDeadline(time.Time{})
}

var (
	readyCh chan utils.Unit
)

func RunTunnel(cmd *cobra.Command, args []string) {
	proxyAddr := must(cmd.Flags().GetString("paddr"))
	srvrAddr := must(cmd.Flags().GetString("saddr"))

	if proxyAddr == "" || srvrAddr == "" {
		log.Fatal(`Must provide "paddr" and "saddr"`)
	}

	log.Printf("Tunneling to %s and piping to %s", proxyAddr, srvrAddr)
	for range readyCh {
		conn, err := net.Dial("tcp", proxyAddr)
		if err != nil {
			log.Print("Error connecting to proxy: ", err)
			continue
		}
		go pipeProxySrvr(conn, srvrAddr)
	}
}

func pipeProxySrvr(proxyConn net.Conn, srvrAddr string) {
	closeProxyConn := utils.NewT(true)
	defer deferredClose(proxyConn, closeProxyConn)
	// Send password and wait for response
	if _, err := utils.WriteAll(proxyConn, passwordHash[:]); err != nil {
		log.Print("Error writing password to proxy: ", err)
		return
	}
	b := []byte{0}
	if _, err := proxyConn.Read(b); err != nil {
		return
	} else if b[0] == passwordInvalid {
		log.Print("Invalid password for proxy")
		return
	} else if b[0] != passwordOk {
		log.Print("Unexpected byte from proxy: ", b[0])
		return
	}

	// Wait for ready
	if _, err := proxyConn.Read(b); err != nil {
		return
	} else if b[0] != connReady {
		log.Printf(
			"Received unexpected response from proxy tunnel, expected %d, got %d",
			connReady, b[0],
		)
		return
	}

	// Signal that another conn is ready to be connected
	readyCh <- utils.Unit{}

	// Connect to server and send ready response
	srvrConn, err := net.Dial("tcp", srvrAddr)
	if err != nil {
		log.Printf("Error connecting to server (%s): %v", srvrAddr, err)
		return
	}
	if _, err := proxyConn.Write([]byte{connReady}); err != nil {
		srvrConn.Close()
		return
	}
	*closeProxyConn = false

	go pipe(proxyConn, srvrConn)
	pipe(srvrConn, proxyConn)
}

func pipe(rconn, wconn net.Conn) {
	io.Copy(wconn, rconn)
	rconn.Close()
	wconn.Close()
}

func deferredClose(conn net.Conn, shouldClose *bool) {
	if *shouldClose {
		conn.Close()
	}
}

func must[T any](t T, err error) T {
	if err != nil {
		log.Fatal("Error: ", err)
	}
	return t
}
