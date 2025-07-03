package connection

import (
	"fmt"
	"sync"

	"github.com/NHAS/reverse_ssh/internal"
	"github.com/NHAS/reverse_ssh/pkg/logger"
	"golang.org/x/crypto/ssh"
)

type Session struct {
	sync.RWMutex

	// This is the users connection to the server itself, creates new channels and whatnot. NOT to get io.Copy'd
	ServerConnection ssh.Conn

	Pty *internal.PtyReq

	ShellRequests <-chan *ssh.Request

	// Remote forwards sent by user, used to just close user specific remote forwards
	SupportedRemoteForwards map[internal.RemoteForwardRequest]bool //(set)
}

func NewSession(connection ssh.Conn) *Session {

	return &Session{
		ServerConnection:        connection,
		SupportedRemoteForwards: make(map[internal.RemoteForwardRequest]bool),
	}
}

func RegisterChannelCallbacks(chans <-chan ssh.NewChannel, log logger.Logger, handlers map[string]func(newChannel ssh.NewChannel, log logger.Logger)) error {
	// Service the incoming Channel channel in go routine
	channelCount := 0
	
	log.Info("DEBUG: Starting channel callback registration loop")
	
	for newChannel := range chans {
		channelCount++
		t := newChannel.ChannelType()
		log.Info("DEBUG: Handling channel #%d: %s", channelCount, t)
		
		if callBack, ok := handlers[t]; ok {
			go callBack(newChannel, log)
			continue
		}

		newChannel.Reject(ssh.UnknownChannelType, fmt.Sprintf("unsupported channel type: %s", t))
		log.Warning("Sent an invalid channel type %q", t)
	}
	log.Error("DEBUG: Channel loop terminated after handling %d channels - SSH connection likely closed", channelCount)


	return fmt.Errorf("connection.go: SSH channel loop terminated after %d channels", channelCount)
}
