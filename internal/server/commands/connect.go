package commands

import (
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/NHAS/reverse_ssh/internal"
	"github.com/NHAS/reverse_ssh/internal/server/users"
	"github.com/NHAS/reverse_ssh/internal/terminal"
	"github.com/NHAS/reverse_ssh/internal/terminal/autocomplete"
	"github.com/NHAS/reverse_ssh/pkg/logger"
	"golang.org/x/crypto/ssh"
)

type connect struct {
	log     logger.Logger
	user    *users.User
	session string
}

func (c *connect) ValidArgs() map[string]string {

	return map[string]string{
		"shell": "Set the shell (or program) to start on connection, this also takes an http, https or rssh url that be downloaded to disk and executed",
	}
}

func (c *connect) Run(user *users.User, tty io.ReadWriter, line terminal.ParsedLine) error {

	sess, err := c.user.Session(c.session)
	if err != nil {
		return err
	}

	if sess.Pty == nil {
		return fmt.Errorf("Connect requires a pty")
	}

	term, ok := tty.(*terminal.Terminal)
	if !ok {
		return fmt.Errorf("connect can only be called from the terminal, if you want to connect to your clients without connecting to the terminal use jumphost syntax -J")
	}

	if len(line.Arguments) < 1 {

		return fmt.Errorf("%s", c.Help(false))
	}

	shell, _ := line.GetArgString("shell")

	client := line.Arguments[len(line.Arguments)-1].Value()

	foundClients, err := user.SearchClients(client)
	if err != nil {
		return err
	}

	if len(foundClients) == 0 {
		return fmt.Errorf("No clients matched '%s'", client)
	}

	if len(foundClients) > 1 {
		return fmt.Errorf("'%s' matches multiple clients please choose a more specific identifier", client)
	}

	var target ssh.Conn
	//Horrible way of getting the first element of a map in go
	for k := range foundClients {
		target = foundClients[k]
		break
	}

	defer func() {
		c.log.Info("Disconnected from remote host %s (%s)", target.RemoteAddr(), target.ClientVersion())
		term.DisableRaw(true)
	}()

	//Attempt to connect to remote host and send inital pty request and screen size
	// If we cant, report and error to the clients terminal
	newSession, err := createSession(target, *sess.Pty, shell)
	if err != nil {

		c.log.Error("Creating session failed: %s", err)
		return err
	}

	c.log.Info("Connected to %s", target.RemoteAddr().String())

	term.EnableRaw()
	
	// Add debugging for session attachment
	c.log.Info("DEBUG: Starting session attachment...")
	err = attachSession(newSession, term, sess.ShellRequests)
	
	if err != nil {
		c.log.Error("Client tried to attach session and failed: %s", err)
		c.log.Info("DEBUG: Connection state - RemoteAddr: %s, ClientVersion: %s", target.RemoteAddr(), target.ClientVersion())
		
		// Check if the underlying connection is still alive
		if target != nil {
			// Try to send a simple request to test connection
			_, _, testErr := target.SendRequest("test-connection", false, nil)
			if testErr != nil {
				c.log.Error("DEBUG: Underlying SSH connection appears dead: %v", testErr)
			} else {
				c.log.Info("DEBUG: Underlying SSH connection still alive")
			}
		}
		
		return err
	}

	return fmt.Errorf("Session has terminated.") // Not really an error. But we can get the terminal to print out stuff

}

func (c *connect) Expect(line terminal.ParsedLine) []string {
	if len(line.Arguments) <= 1 {
		return []string{autocomplete.RemoteId}
	}
	return nil
}

func (c *connect) Help(explain bool) string {
	const description = "Start shell on remote controllable host."
	if explain {
		return "Start shell on remote controllable host."
	}

	return terminal.MakeHelpText(c.ValidArgs(),
		"connect "+autocomplete.RemoteId,
		description,
	)
}

func Connect(
	session string,
	user *users.User,
	log logger.Logger) *connect {
	return &connect{
		session: session,
		user:    user,
		log:     log,
	}
}

func createSession(sshConn ssh.Conn, ptyReq internal.PtyReq, shell string) (sc ssh.Channel, err error) {

	fmt.Printf("DEBUG: Creating session on connection %s (%s)\n", sshConn.RemoteAddr(), sshConn.ClientVersion())

	splice, newrequests, err := sshConn.OpenChannel("session", nil)
	if err != nil {
		return sc, fmt.Errorf("Unable to start remote session on host %s (%s) : %s", sshConn.RemoteAddr(), sshConn.ClientVersion(), err)
	}

	fmt.Printf("DEBUG: SSH channel opened successfully\n")

	//Send pty request, pty has been continuously updated with window-change sizes
	_, err = splice.SendRequest("pty-req", true, ssh.Marshal(ptyReq))
	if err != nil {
		splice.Close()
		return sc, fmt.Errorf("Unable to send PTY request: %s", err)
	}

	fmt.Printf("DEBUG: PTY request sent successfully\n")

	_, err = splice.SendRequest("shell", true, ssh.Marshal(internal.ShellStruct{Cmd: shell}))
	if err != nil {
		splice.Close()
		return sc, fmt.Errorf("Unable to start shell: %s", err)
	}

	fmt.Printf("DEBUG: Shell request sent successfully, shell: %s\n", shell)

	go ssh.DiscardRequests(newrequests)

	return splice, nil
}

func attachSession(newSession ssh.Channel, currentClientSession io.ReadWriter, currentClientRequests <-chan *ssh.Request) error {

	finished := make(chan bool, 1)
	sessionErrors := make(chan error, 3) // Buffer for multiple error sources
	// sessionErrors <- fmt.Errorf("TESTING")

	closeSession := func() {
		newSession.Close()
		select {
		case finished <- true:
		default:
		}
	}

	// Setup the pipes for stdin/stdout over the connections

	// Start copying output before we start copying user input, so we can get the responses to the rc files lines
	var once sync.Once
	defer once.Do(closeSession)

	// Copy from client to remote (stdin)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				sessionErrors <- fmt.Errorf("stdin copy goroutine panic: %v", r)
			}
		}()
		
		// Use a resilient copy that handles delays and EOF gracefully
		var totalBytes int64
		buffer := make([]byte, 32*1024) // 32KB buffer
		
		for {
			select {
			case <-finished:
				sessionErrors <- fmt.Errorf("stdin copy stopped due to session finish after %d bytes", totalBytes)
				return
			default:
			}
			
			n, err := currentClientSession.Read(buffer)
			if n > 0 {
				// We have data, write it to remote
				written, writeErr := newSession.Write(buffer[:n])
				totalBytes += int64(written)
				
				if writeErr != nil {
					sessionErrors <- fmt.Errorf("stdin write failed after %d bytes: %w", totalBytes, writeErr)
					once.Do(closeSession)
					return
				}
				
				if written != n {
					sessionErrors <- fmt.Errorf("stdin partial write: wrote %d of %d bytes", written, n)
					once.Do(closeSession)
					return
				}
			}
			
			if err != nil {
				if err == io.EOF {
					// EOF doesn't mean connection is dead in HTTP tunnels - just no data right now
					fmt.Printf("DEBUG: stdin EOF after %d bytes - continuing to wait for more data\n", totalBytes)
					time.Sleep(100 * time.Millisecond) // Brief pause before retrying
					continue
				}
				// Only real errors should terminate
				sessionErrors <- fmt.Errorf("stdin read failed after %d bytes: %w", totalBytes, err)
				once.Do(closeSession)
				return
			}
		}
	}()

	// Copy from remote to client (stdout)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				sessionErrors <- fmt.Errorf("stdout copy goroutine panic: %v", r)
			}
		}()
		
		// Use a resilient copy that handles delays and EOF gracefully
		var totalBytes int64
		buffer := make([]byte, 32*1024) // 32KB buffer
		
		for {
			select {
			case <-finished:
				sessionErrors <- fmt.Errorf("stdout copy stopped due to session finish after %d bytes", totalBytes)
				return
			default:
			}
			
			n, err := newSession.Read(buffer)
			if n > 0 {
				// We have data, write it to client
				written, writeErr := currentClientSession.Write(buffer[:n])
				totalBytes += int64(written)
				
				if writeErr != nil {
					sessionErrors <- fmt.Errorf("stdout write failed after %d bytes: %w", totalBytes, writeErr)
					once.Do(closeSession)
					return
				}
				
				if written != n {
					sessionErrors <- fmt.Errorf("stdout partial write: wrote %d of %d bytes", written, n)
					once.Do(closeSession)
					return
				}
			}
			
			if err != nil {
				if err == io.EOF {
					// EOF doesn't mean connection is dead in HTTP tunnels - just no data right now
					fmt.Printf("DEBUG: stdout EOF after %d bytes - continuing to wait for more data\n", totalBytes)
					time.Sleep(100 * time.Millisecond) // Brief pause before retrying
					continue
				}
				// Only real errors should terminate
				sessionErrors <- fmt.Errorf("stdout read failed after %d bytes: %w", totalBytes, err)
				once.Do(closeSession)
				return
			}
		}
	}()

RequestsProxyPasser:
	for {
		select {
		case r, ok := <-currentClientRequests:
			if !ok {
				sessionErrors <- fmt.Errorf("client request channel closed")
				break RequestsProxyPasser
			}
			if r == nil {
				sessionErrors <- fmt.Errorf("received nil request from client")
				break RequestsProxyPasser
			}

			response, err := internal.SendRequest(*r, newSession)
			if err != nil {
				sessionErrors <- fmt.Errorf("failed to send request '%s' to remote session: %w", r.Type, err)
				break RequestsProxyPasser
			}

			if r.WantReply {
				r.Reply(response, nil)
			}
		case <-finished:
			sessionErrors <- fmt.Errorf("session finished due to IO completion")
			break RequestsProxyPasser
		case err := <-sessionErrors:
			// Log the specific error that caused termination
			fmt.Printf("DEBUG: Session terminating due to: %v\n", err)
			break RequestsProxyPasser
		}
	}

	// Collect and log all errors that occurred
	var allErrors []error
	for {
		select {
		case err := <-sessionErrors:
			allErrors = append(allErrors, err)
		default:
			goto done
		}
	}
done:
	
	if len(allErrors) > 0 {
		fmt.Printf("DEBUG: All session errors collected:\n")
		for i, err := range allErrors {
			fmt.Printf("  %d: %v\n", i+1, err)
		}
		return fmt.Errorf("session terminated with %d errors, primary: %w", len(allErrors), allErrors[0])
	}

	return fmt.Errorf("session finished normally")
}
