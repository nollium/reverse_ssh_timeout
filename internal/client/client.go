package client

import (
	"bytes"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"net"
	"net/url"
	"os"
	"os/user"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/NHAS/reverse_ssh/internal"
	"github.com/NHAS/reverse_ssh/internal/client/connection"
	"github.com/NHAS/reverse_ssh/internal/client/handlers"
	"github.com/NHAS/reverse_ssh/internal/client/keys"
	"github.com/NHAS/reverse_ssh/pkg/logger"
	"golang.org/x/crypto/ssh"
	socks "golang.org/x/net/proxy"
	"golang.org/x/net/websocket"
)

func WriteHTTPReq(lines []string, conn net.Conn) error {
	lines = append(lines, "") // Add an empty line for completing the HTTP request
	for _, line := range lines {

		n, err := conn.Write([]byte(line + "\r\n"))
		if err != nil {
			return err
		}

		if len(line+"\r\n") < n {
			return io.ErrShortWrite
		}
	}
	return nil
}

// https://cs.opensource.google/go/x/net/+/refs/tags/v0.19.0:http/httpproxy/proxy.go;l=27
// Due to this code not having the compatiblity promise of golang 1.x Im moving this in here just in case something changes rather than using the library
func GetProxyDetails(proxy string) (string, error) {
	if proxy == "" {
		return "", nil
	}

	proxyURL, err := url.Parse(proxy)
	if err != nil ||
		(proxyURL.Scheme != "http" &&
			proxyURL.Scheme != "https" &&
			proxyURL.Scheme != "socks" &&
			proxyURL.Scheme != "socks5" &&
			proxyURL.Scheme != "socks4") {
		// proxy was bogus. Try prepending "http://" to it and
		// see if that parses correctly. If not, we fall
		// through and complain about the original one.
		proxyURL, err = url.Parse("http://" + proxy)
	}

	if err != nil {
		return "", fmt.Errorf("invalid proxy address %q: %v", proxy, err)
	}

	// If there is no port set we need to add a default for the tcp connection
	// Yes most of these are not supported LACHLAN, and thats fine. Im lazy
	port := proxyURL.Port()
	if port == "" {
		switch proxyURL.Scheme {
		case "socks5", "socks", "socks4":
			proxyURL.Host += ":1080"
		case "https":
			proxyURL.Host += ":443"
		case "http":
			proxyURL.Host += ":80"
		}
	}
	return proxyURL.Scheme + "://" + proxyURL.Host, nil
}

func Connect(addr, proxy string, timeout time.Duration, winauth bool) (conn net.Conn, err error) {
	timeout = 0
	if len(proxy) != 0 {
		log.Println("Setting HTTP proxy address as: ", proxy)
		proxyURL, _ := url.Parse(proxy) // Already parsed

		if proxyURL.Scheme == "http" || proxyURL.Scheme == "https" {

			var (
				proxyCon net.Conn
				err      error
			)
			switch proxyURL.Scheme {
			case "http":
				proxyCon, err = net.Dial("tcp", proxyURL.Host) // Infinite timeout
			case "https":
				proxyCon, err = tls.Dial("tcp", proxyURL.Host, &tls.Config{
					InsecureSkipVerify: true,
				}) // Infinite timeout
			}
			if err != nil {
				return nil, err
			}

			if tcpC, ok := proxyCon.(*net.TCPConn); ok {
				tcpC.SetKeepAlivePeriod(2 * time.Hour)
			}

			// First attempt without auth
			req := []string{
				fmt.Sprintf("CONNECT %s HTTP/1.1", addr),
				fmt.Sprintf("Host: %s", addr),
			}

			err = WriteHTTPReq(req, proxyCon)
			if err != nil {
				return nil, fmt.Errorf("unable to connect proxy %s", proxy)
			}

			var responseStatus []byte
			for {
				b := make([]byte, 1)
				_, err := proxyCon.Read(b)
				if err != nil {
					return conn, fmt.Errorf("reading from proxy failed")
				}
				responseStatus = append(responseStatus, b...)

				if len(responseStatus) > 4 && bytes.Equal(responseStatus[len(responseStatus)-4:], []byte("\r\n\r\n")) {
					break
				}
			}

			// If we get a 407 Proxy Authentication Required
			if bytes.Contains(bytes.ToLower(responseStatus), []byte("407")) {
				// Check if NTLM is supported
				if bytes.Contains(bytes.ToLower(responseStatus), []byte("proxy-authenticate: ntlm")) {
					if ntlmProxyCreds != "" {
						// Start NTLM negotiation
						ntlmHeader, err := getNTLMAuthHeader(nil)
						if err != nil {
							return nil, fmt.Errorf("NTLM negotiation failed: %v", err)
						}

						// Send Type 1 message
						req = []string{
							fmt.Sprintf("CONNECT %s HTTP/1.1", addr),
							fmt.Sprintf("Host: %s", addr),
							fmt.Sprintf("Proxy-Authorization: %s", ntlmHeader),
						}

						err = WriteHTTPReq(req, proxyCon)
						if err != nil {
							return nil, fmt.Errorf("unable to send NTLM negotiate message: %s", err)
						}

						// Read challenge response
						responseStatus = []byte{}
						for {
							b := make([]byte, 1)
							_, err := proxyCon.Read(b)
							if err != nil {
								return conn, fmt.Errorf("reading NTLM challenge failed")
							}
							responseStatus = append(responseStatus, b...)

							if len(responseStatus) > 4 && bytes.Equal(responseStatus[len(responseStatus)-4:], []byte("\r\n\r\n")) {
								break
							}
						}

						// Extract Type 2 message
						ntlmParts := strings.SplitN(string(responseStatus), NTLM, 2)
						if len(ntlmParts) != 2 {
							return nil, fmt.Errorf("no NTLM challenge received")
						}

						challengeStr := strings.SplitN(ntlmParts[1], "\r\n", 2)[0]
						challenge, err := base64.StdEncoding.DecodeString(challengeStr)
						if err != nil {
							return nil, fmt.Errorf("invalid NTLM challenge: %v", err)
						}

						// Generate Type 3 message
						ntlmHeader, err = getNTLMAuthHeader(challenge)
						if err != nil {
							return nil, fmt.Errorf("NTLM authentication failed: %v", err)
						}

						// Send Type 3 message
						req = []string{
							fmt.Sprintf("CONNECT %s HTTP/1.1", addr),
							fmt.Sprintf("Host: %s", addr),
							fmt.Sprintf("Proxy-Authorization: %s", ntlmHeader),
						}

						err = WriteHTTPReq(req, proxyCon)
						if err != nil {
							return nil, fmt.Errorf("unable to send NTLM authenticate message: %v", err)
						}

						// Read final response
						responseStatus = []byte{}
						for {
							b := make([]byte, 1)
							_, err := proxyCon.Read(b)
							if err != nil {
								return conn, fmt.Errorf("reading final response failed")
							}
							responseStatus = append(responseStatus, b...)

							if len(responseStatus) > 4 && bytes.Equal(responseStatus[len(responseStatus)-4:], []byte("\r\n\r\n")) {
								break
							}
						}
					} else if winauth {
						req = additionalHeaders(proxy, req)
						err = WriteHTTPReq(req, proxyCon)
						if err != nil {
							return nil, fmt.Errorf("unable to connect proxy %s", proxy)
						}

						responseStatus = []byte{}
						for {
							b := make([]byte, 1)
							_, err := proxyCon.Read(b)
							if err != nil {
								return conn, fmt.Errorf("reading from proxy failed")
							}
							responseStatus = append(responseStatus, b...)

							if len(responseStatus) > 4 && bytes.Equal(responseStatus[len(responseStatus)-4:], []byte("\r\n\r\n")) {
								break
							}
						}
					}
				}
			}

			if !(bytes.Contains(bytes.ToLower(responseStatus), []byte("200"))) {
				parts := bytes.Split(responseStatus, []byte("\r\n"))
				if len(parts) > 1 {
					return nil, fmt.Errorf("failed to proxy: %q", parts[0])
				}
			}

			log.Println("Proxy accepted CONNECT request, connection set up!")

			return proxyCon, nil
		}
		if proxyURL.Scheme == "socks" || proxyURL.Scheme == "socks5" {
			dial, err := socks.SOCKS5("tcp", proxyURL.Host, nil, nil)
			if err != nil {
				return nil, fmt.Errorf("reading from socks failed: %s", err)
			}
			proxyCon, err := dial.Dial("tcp", addr)
			if err != nil {
				return nil, fmt.Errorf("failed to dial socks: %s", err)
			}

			log.Println("SOCKS Proxy accepted dial, connection set up!")

			return proxyCon, nil
		}
	}

	conn, err = net.Dial("tcp", addr) // Infinite timeout
	if err != nil {
		return nil, fmt.Errorf("failed to connect: %s", err)
	}

	if tcpC, ok := conn.(*net.TCPConn); ok {
		tcpC.SetKeepAlivePeriod(2 * time.Hour)
	}

	return
}

func getCaseInsensitiveEnv(envs ...string) (ret []string) {

	lower := map[string]bool{}

	for _, env := range envs {
		lower[strings.ToLower(env)] = true
	}

	for _, e := range os.Environ() {

		part := strings.SplitN(e, "=", 2)
		if len(part) > 1 && lower[strings.ToLower(part[0])] {
			ret = append(ret, part[1])
		}
	}
	return ret
}

func Run(addr, fingerprint, proxyAddr, sni string, winauth bool) {

	sshPriv, sysinfoError := keys.GetPrivateKey()
	if sysinfoError != nil {
		log.Fatal("Getting private key failed: ", sysinfoError)
	}

	l := logger.NewLog("client")

	var err error
	proxyAddr, err = GetProxyDetails(proxyAddr)
	if err != nil {
		log.Fatal("Invalid proxy details", proxyAddr, ":", err)
	}

	var username string
	userInfo, sysinfoError := user.Current()
	if sysinfoError != nil {
		l.Warning("Couldnt get username: %s", sysinfoError.Error())
		username = "Unknown"
	} else {
		username = userInfo.Username
	}

	hostname, sysinfoError := os.Hostname()
	if sysinfoError != nil {
		hostname = "Unknown Hostname"
		l.Warning("Couldnt get host name: %s", sysinfoError)
	}

	config := &ssh.ClientConfig{
		User: fmt.Sprintf("%s.%s", username, hostname),
		Auth: []ssh.AuthMethod{
			ssh.PublicKeys(sshPriv),
		},
		HostKeyCallback: func(hostname string, remote net.Addr, key ssh.PublicKey) error {
			if fingerprint == "" { // If a server key isnt supplied, fail open. Potentially should change this for more paranoid people
				l.Warning("No server key specified, allowing connection to %s", addr)
				return nil
			}

			if internal.FingerprintSHA256Hex(key) != fingerprint {
				return fmt.Errorf("server public key invalid, expected: %s, got: %s", fingerprint, internal.FingerprintSHA256Hex(key))
			}

			return nil
		},
		ClientVersion: "SSH-" + internal.Version + "-" + runtime.GOOS + "_" + runtime.GOARCH,
	}

	realAddr, scheme := determineConnectionType(addr)

	// fetch the environment variables, but the first proxy is done from the supplied proxyAddr arg
	potentialProxies := getCaseInsensitiveEnv("http_proxy", "https_proxy")
	var triedProxyIndex int
	var reconnectDelay = time.Second * 5
	const maxReconnectDelay = time.Minute * 5 // Cap at 5 minutes
	initialProxyAddr := proxyAddr
	for {
		var conn net.Conn
		if scheme != "stdio" {
			log.Println("Connecting to", addr)
			// First create raw TCP connection
			conn, err = Connect(realAddr, proxyAddr, config.Timeout, winauth)
			if err != nil {

				if errMsg := err.Error(); strings.Contains(errMsg, "missing port in address") {
					log.Fatalf("Unable to connect to TCP invalid address: '%s', %s", addr, errMsg)
				}

				log.Printf("Unable to connect directly TCP: %s\n", err)

				if len(potentialProxies) > 0 {
					if len(potentialProxies) <= triedProxyIndex {
						log.Printf("Unable to connect via proxies (from env), retrying with proxy as %q: %v", potentialProxies, initialProxyAddr)
						triedProxyIndex = 0
						proxyAddr = initialProxyAddr
						continue
					}
					proxy := potentialProxies[triedProxyIndex]
					triedProxyIndex++

					log.Println("Trying to proxy via env variable (", proxy, ")")

					proxyAddr, err = GetProxyDetails(proxy)
					if err != nil {
						log.Println("Could not parse the env proxy value: ", proxy)
					}
					// dont wait 10 seconds, just immediately try each proxy
					continue
				}

				<-time.After(10 * time.Second)
				continue
			}

			// Add on transports as we go
			if scheme == "tls" || scheme == "wss" || scheme == "https" {

				sniServerName := sni
				if len(sni) == 0 {
					sniServerName = realAddr
					parts := strings.Split(realAddr, ":")
					if len(parts) == 2 {
						sniServerName = parts[0]
					}
				}

				clientTlsConn := tls.Client(conn, &tls.Config{
					InsecureSkipVerify: true,
					ServerName:         sniServerName,
				})
				err = clientTlsConn.Handshake()
				if err != nil {
					log.Printf("Unable to connect TLS: %s\n", err)
					<-time.After(10 * time.Second)
					continue
				}

				conn = clientTlsConn
			}

			switch scheme {
			case "wss", "ws":
				// Use the correct protocol scheme for WebSocket configuration
				wsScheme := "ws"
				if scheme == "wss" {
					wsScheme = "wss"
				}
				
				c, err := websocket.NewConfig(wsScheme+"://"+realAddr+"/ws", wsScheme+"://"+realAddr)
				if err != nil {
					log.Println("Could not create websockets configuration: ", err)
					<-time.After(10 * time.Second)

					continue
				}

				wsConn, err := websocket.NewClient(c, conn)
				if err != nil {
					log.Printf("Unable to connect WS: %s\n", err)
					<-time.After(10 * time.Second)
					continue

				}
				// Pain and suffering https://github.com/golang/go/issues/7350
				wsConn.PayloadType = websocket.BinaryFrame

				conn = wsConn
			case "http", "https":

				conn, err = NewHTTPConn(scheme+"://"+realAddr, func() (net.Conn, error) {
					return Connect(realAddr, proxyAddr, config.Timeout, winauth)
				})

				if err != nil {
					log.Printf("Unable to connect HTTP: %s\n", err)
					<-time.After(10 * time.Second)
					continue
				}

			}

		} else {
			conn = &InetdConn{}
		}

		// Make initial timeout quite long so folks who type their ssh public key can actually do it
		// After this the timeout gets updated by the server
		realConn := &internal.TimeoutConn{Conn: conn, Timeout: 0} // Infinite timeout

		log.Printf("DEBUG: Creating SSH client connection to %s", addr)
		sshConn, chans, reqs, err := ssh.NewClientConn(realConn, addr, config)
		if err != nil {
			realConn.Close()

			log.Printf("Unable to start a new client connection: %s\n", err)

			if scheme == "stdio" {
				// If we are in stdin/stdout mode (https://github.com/NHAS/reverse_ssh/issues/149), and something happens to our socket, just die. As we cant recover the connection (its for the harness to do)
				return
			}

			// Use same exponential backoff for initial connection failures
			log.Printf("Retrying connection in %v...", reconnectDelay)
			<-time.After(reconnectDelay)
			
			reconnectDelay *= 2
			if reconnectDelay > maxReconnectDelay {
				reconnectDelay = maxReconnectDelay
			}
			continue
		}

		if len(potentialProxies) > 0 {
			// reset proxy counter after success, so we always check the avaliable proxies
			triedProxyIndex = 0
		}

		// Reset reconnection delay on successful connection
		reconnectDelay = time.Second * 5

		log.Println("Successfully connnected", addr)
		log.Printf("DEBUG: SSH connection established - RemoteAddr: %s, ClientVersion: %s", sshConn.RemoteAddr(), sshConn.ClientVersion())

		go func() {
			log.Printf("DEBUG: Starting request handler goroutine")
			requestCount := 0
			lastRequestTime := time.Now()

			for req := range reqs {
				requestCount++
				now := time.Now()
				timeSinceLastRequest := now.Sub(lastRequestTime)
				lastRequestTime = now
				
				log.Printf("DEBUG: Processing request #%d: %s (gap: %v)", requestCount, req.Type, timeSinceLastRequest)

				switch req.Type {

				case "kill":
					log.Println("Got kill command, goodbye")
					<-time.After(5 * time.Second)
					os.Exit(0)

				case "keepalive-rssh@golang.org":
					log.Printf("DEBUG: Replying to keepalive...")
					req.Reply(false, nil)
					log.Printf("DEBUG: Keepalive reply sent")
					
					timeout, err := strconv.Atoi(string(req.Payload))
					if err != nil {
						log.Printf("DEBUG: Invalid keepalive timeout from server: %v", err)
						continue
					}

					log.Printf("DEBUG: Server requested timeout %d seconds, but keeping infinite timeout", timeout)
					// Don't set any timeout - keep infinite
					// realConn.Timeout = time.Duration(timeout*2) * time.Second

				case "log-level":
					log.Printf("DEBUG: Processing log-level request...")
					u, err := logger.StrToUrgency(string(req.Payload))
					if err != nil {
						log.Printf("server sent invalid log level: %q", string(req.Payload))
						req.Reply(false, nil)
						continue
					}

					logger.SetLogLevel(u)
					req.Reply(true, nil)
					log.Printf("DEBUG: Log-level request completed")

				case "log-to-file":
					log.Printf("DEBUG: Processing log-to-file request...")
					req.Reply(true, nil)

					if err := handlers.Console.ToFile(string(req.Payload)); err != nil {
						log.Println("Failed to direct log to file ", string(req.Payload), err)
					}
					log.Printf("DEBUG: Log-to-file request completed")

				case "tcpip-forward":
					log.Printf("DEBUG: Processing tcpip-forward request...")
					go handlers.StartRemoteForward(nil, req, sshConn)
					log.Printf("DEBUG: tcpip-forward request dispatched")

				case "query-tcpip-forwards":
					log.Printf("DEBUG: Processing query-tcpip-forwards request...")
					f := struct {
						RemoteForwards []string
					}{
						RemoteForwards: handlers.GetServerRemoteForwards(),
					}

					// Use ssh.Marshal instead of json.Marshal so that garble doesnt cook things
					req.Reply(true, ssh.Marshal(f))
					log.Printf("DEBUG: query-tcpip-forwards request completed")

				case "cancel-tcpip-forward":
					log.Printf("DEBUG: Processing cancel-tcpip-forward request...")
					var rf internal.RemoteForwardRequest

					err := ssh.Unmarshal(req.Payload, &rf)
					if err != nil {
						req.Reply(false, []byte(fmt.Sprintf("Unable to unmarshal remote forward request in order to stop it: %s", err.Error())))
						return
					}

					go func(r *ssh.Request) {

						err := handlers.StopRemoteForward(rf)
						if err != nil {
							r.Reply(false, []byte(err.Error()))
							return
						}

						r.Reply(true, nil)
					}(req)
					log.Printf("DEBUG: cancel-tcpip-forward request dispatched")

				default:
					log.Printf("DEBUG: Processing unknown request: %s", req.Type)
					if req.WantReply {
						req.Reply(false, nil)
					}
					log.Printf("DEBUG: Unknown request completed")
				}
				
				log.Printf("DEBUG: Request #%d (%s) processing completed", requestCount, req.Type)
			}
			log.Printf("DEBUG: Request handler loop terminated after processing %d requests", requestCount)
		}()

		// Monitor for hanging SSH operations
		go func() {
			ticker := time.NewTicker(30 * time.Second)
			defer ticker.Stop()
			
			for {
				select {
				case <-ticker.C:
					log.Printf("DEBUG: SSH connection health check - still alive")
					
					// Try to send a simple SSH request to test if connection is responsive
					go func() {
						start := time.Now()
						log.Printf("DEBUG: Testing SSH connection responsiveness...")
						
						// Create a dummy channel request to test connection
						ok, _, err := sshConn.SendRequest("test-connection", false, nil)
						elapsed := time.Since(start)
						
						if err != nil {
							log.Printf("DEBUG: SSH connection test failed after %v: %v", elapsed, err)
						} else {
							log.Printf("DEBUG: SSH connection test completed in %v (ok=%v)", elapsed, ok)
						}
					}()
				}
			}
		}()

		clientLog := logger.NewLog("client")

		//Do not register new client callbacks here, they are actually within the JumpHandler
		//session is handled here as a legacy hangerover from allowing a client who has directly connected to the servers console to run the connect command
		//Otherwise anything else should be done via jumphost syntax -J
		log.Printf("DEBUG: Starting channel callback registration")
		err = connection.RegisterChannelCallbacks(chans, clientLog, map[string]func(newChannel ssh.NewChannel, log logger.Logger){
			"session":        handlers.Session(connection.NewSession(sshConn)),
			"jump":           handlers.JumpHandler(sshPriv, sshConn),
			"log-to-console": handlers.LogToConsole,
		})

		log.Printf("DEBUG: Channel callbacks terminated, closing SSH connection")
		sshConn.Close()
		handlers.StopAllRemoteForwards()

		if err != nil {
			log.Printf("Server disconnected: %v\n", err)
			
			// Add detailed connection state debugging
			if sshConn != nil {
				log.Printf("DEBUG: SSH connection state - RemoteAddr: %s, ClientVersion: %s", sshConn.RemoteAddr(), sshConn.ClientVersion())
				
				// Test if underlying connection is still alive
				_, _, testErr := sshConn.SendRequest("test-connection", false, nil)
				if testErr != nil {
					log.Printf("DEBUG: Underlying SSH connection appears dead: %v", testErr)
				} else {
					log.Printf("DEBUG: Underlying SSH connection still responsive")
				}
			}
			
			// Check if it's the underlying transport that failed
			if realConn != nil {
				log.Printf("DEBUG: TimeoutConn timeout setting: %v", realConn.Timeout)
			}

			if scheme == "stdio" {
				return
			}

			// Exponential backoff for reconnection with indefinite retries
			log.Printf("Reconnecting in %v...", reconnectDelay)
			<-time.After(reconnectDelay)
			
			reconnectDelay *= 2
			if reconnectDelay > maxReconnectDelay {
				reconnectDelay = maxReconnectDelay
			}
			continue
		}

	}

}

var matchSchemeDefinition = regexp.MustCompile(`.*\:\/\/`)

func determineConnectionType(addr string) (resultingAddr, transport string) {

	if !matchSchemeDefinition.MatchString(addr) {
		return addr, "ssh"
	}

	u, err := url.ParseRequestURI(addr)
	if err != nil {
		// If the connection string is in the form of 1.1.1.1:4343
		return addr, "ssh"
	}

	if u.Scheme == "" {
		// If the connection string is just an ip address (no port)
		log.Println("no port specified: ", u.Path, "using port 22")
		return u.Path + ":22", "ssh"
	}

	if u.Port() == "" {
		// Set default port if none specified
		switch u.Scheme {
		case "tls", "wss":
			return u.Host + ":443", u.Scheme
		case "ws":
			return u.Host + ":80", u.Scheme
		case "stdio":
			return "stdio://nothing", u.Scheme
		}

		log.Println("url scheme ", u.Scheme, "not recognised falling back to ssh: ", u.Host+":22", "as no port specified")
		return u.Host + ":22", "ssh"
	}

	return u.Host, u.Scheme

}
