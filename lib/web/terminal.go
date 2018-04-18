/*
Copyright 2015 Gravitational, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package web

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"

	"golang.org/x/crypto/ssh"
	"golang.org/x/net/websocket"

	"github.com/gravitational/teleport/lib/client"
	"github.com/gravitational/teleport/lib/defaults"
	"github.com/gravitational/teleport/lib/services"
	"github.com/gravitational/teleport/lib/session"
	"github.com/gravitational/teleport/lib/sshutils"
	"github.com/gravitational/teleport/lib/utils"

	"github.com/gravitational/trace"

	"github.com/moby/moby/pkg/term"
	log "github.com/sirupsen/logrus"
)

// TerminalRequest describes a request to crate a web-based terminal
// to a remote SSH server
type TerminalRequest struct {
	// Server describes a server to connect to (serverId|hostname[:port])
	Server string `json:"server_id"`
	// User is linux username to connect as
	Login string `json:"login"`
	// Term sets PTY params like width and height
	Term session.TerminalParams `json:"term"`
	// SessionID is a teleport session ID to join as
	SessionID session.ID `json:"sid"`
	// Namespace is node namespace
	Namespace string `json:"namespace"`
	// Proxy server address
	ProxyHostPort string `json:"-"`
	// Remote cluster name
	Cluster string `json:"-"`
	// InteractiveCommand is a command to execute
	InteractiveCommand []string `json:"-"`
}

// NodeProvider is a provider of nodes for namespace
type NodeProvider interface {
	GetNodes(namespace string) ([]services.Server, error)
}

// newTerminal creates a web-based terminal based on WebSockets and returns a new
// TerminalHandler
func NewTerminal(req TerminalRequest, provider NodeProvider, ctx *SessionContext) (*TerminalHandler, error) {
	// make sure whatever session is requested is a valid session
	_, err := session.ParseID(string(req.SessionID))
	if err != nil {
		return nil, trace.BadParameter("sid: invalid session id")
	}

	if req.Server == "" {
		return nil, trace.BadParameter("server: missing server")
	}

	if req.Login == "" {
		return nil, trace.BadParameter("login: missing login")
	}
	if req.Term.W <= 0 || req.Term.H <= 0 {
		return nil, trace.BadParameter("term: bad term dimensions")
	}

	servers, err := provider.GetNodes(req.Namespace)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	hostName, hostPort, err := resolveHostPort(req.Server, servers)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	return &TerminalHandler{
		params:   req,
		ctx:      ctx,
		hostName: hostName,
		hostPort: hostPort,
	}, nil
}

// TerminalHandler connects together an SSH session with a web-based
// terminal via a web socket.
type TerminalHandler struct {
	// params describe the terminal configuration
	params TerminalRequest
	// ctx is a web session context for the currently logged in user
	ctx *SessionContext
	// ws is the websocket which is connected to stdin/out/err of the terminal shell
	ws *websocket.Conn
	// hostName we're connected to
	hostName string
	// hostPort we're connected to
	hostPort int
	// sshClient is initialized after an SSH connection to a node is established
	sshSession *ssh.Session
}

func (t *TerminalHandler) Close() error {
	if t.ws != nil {
		t.ws.Close()
	}
	if t.sshSession != nil {
		t.sshSession.Close()
	}
	return nil
}

// resizePTYWindow is called when a brower resizes its window. Now the node
// needs to be notified via SSH
func (t *TerminalHandler) resizePTYWindow(params session.TerminalParams) error {
	if t.sshSession == nil {
		return nil
	}
	_, err := t.sshSession.SendRequest(
		// send SSH "window resized" SSH request:
		sshutils.WindowChangeRequest,
		// no response needed
		false,
		ssh.Marshal(sshutils.WinChangeReqParams{
			W: uint32(params.W),
			H: uint32(params.H),
		}))
	if err != nil {
		log.Error(err)
	}
	return trace.Wrap(err)
}

// Run creates a new websocket connection to the SSH server and runs
// the "loop" piping the input/output of the SSH session into the
// js-based terminal.
func (t *TerminalHandler) Run(w http.ResponseWriter, r *http.Request) {
	errToTerm := func(err error, w io.Writer) {
		fmt.Fprintf(w, "%s\n\r", err.Error())
		log.Error(err)
	}
	webSocketLoop := func(ws *websocket.Conn) {
		agent, cert, err := t.ctx.GetAgent()
		if err != nil {
			log.Warningf("failed to get user credentials: %v", err)
			errToTerm(err, ws)
			return
		}

		signers, err := agent.Signers()
		if err != nil {
			log.Warningf("failed to get user credentials: %v", err)
			errToTerm(err, ws)
			return
		}

		tlsConfig, err := t.ctx.ClientTLSConfig()
		if err != nil {
			log.Warningf("failed to get client TLS config: %v", err)
			errToTerm(err, ws)
			return
		}

		webPTY := client.WebPTY{
			Conn:    ws,
			ID:      t.params.SessionID.String(),
			WinSize: &term.Winsize{Height: 40, Width: 80},
		}

		// create teleport client:
		output := utils.NewWebSockWrapper(ws, utils.WebSocketTextMode)
		clientConfig := &client.Config{
			SkipLocalAuth:    true,
			ForwardAgent:     true,
			Agent:            agent,
			TLS:              tlsConfig,
			AuthMethods:      []ssh.AuthMethod{ssh.PublicKeys(signers...)},
			DefaultPrincipal: cert.ValidPrincipals[0],
			HostLogin:        t.params.Login,
			Username:         t.ctx.user,
			Namespace:        t.params.Namespace,
			Stdout:           output,
			Stderr:           output,
			Stdin:            ws,
			SiteName:         t.params.Cluster,
			ProxyHostPort:    t.params.ProxyHostPort,
			Host:             t.hostName,
			HostPort:         t.hostPort,
			Env:              map[string]string{sshutils.SessionEnvVar: string(t.params.SessionID)},
			HostKeyCallback:  func(string, net.Addr, ssh.PublicKey) error { return nil },
			ClientAddr:       r.RemoteAddr,
			LocalPTY:         webPTY,
		}
		if len(t.params.InteractiveCommand) > 0 {
			clientConfig.Interactive = true
		}
		tc, err := client.NewClient(clientConfig)
		if err != nil {
			log.Warningf("failed to create client: %v", err)
			errToTerm(err, ws)
			return
		}

		// this callback will execute when a shell is created, it will give
		// us a reference to ssh.Client object
		tc.OnShellCreated = func(s *ssh.Session, c *ssh.Client, _ io.ReadWriteCloser) (bool, error) {
			t.sshSession = s
			t.resizePTYWindow(t.params.Term)
			return false, nil
		}
		if err = tc.SSH(context.TODO(), t.params.InteractiveCommand, false); err != nil {
			log.Warningf("failed to SSH: %v", err)
			errToTerm(err, ws)
			return
		}
	}

	// this is to make sure we close web socket connections once
	// sessionContext that owns them expires
	t.ctx.AddClosers(t)
	defer t.ctx.RemoveCloser(t)

	// TODO(klizhentas)
	// we instantiate a server explicitly here instead of using
	// websocket.HandlerFunc to set empty origin checker
	// make sure we check origin when in prod mode
	ws := &websocket.Server{Handler: webSocketLoop}
	ws.ServeHTTP(w, r)
}

// resolveHostPort parses an input value and attempts to resolve hostname and port of requested server
func resolveHostPort(value string, existingServers []services.Server) (string, int, error) {
	var hostName = ""
	// if port is 0, it means the client wants us to figure out which port to use
	var hostPort = 0

	// check if server exists by comparing its UUID or hostname
	for i := range existingServers {
		node := existingServers[i]
		if node.GetName() == value || strings.EqualFold(node.GetHostname(), value) {
			hostName = node.GetHostname()
			break
		}
	}

	// if server is not found, parse SSH connection string (for joining an unlisted SSH server)
	if hostName == "" {
		hostName = value
		host, port, err := net.SplitHostPort(value)
		if err != nil {
			hostPort = defaults.SSHDefaultPort
		} else {
			hostName = host
			hostPort, err = strconv.Atoi(port)
			if err != nil {
				return "", 0, trace.BadParameter("server: invalid port", err)
			}
		}
	}

	return hostName, hostPort, nil
}
