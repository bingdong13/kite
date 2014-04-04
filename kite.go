// Package kite is a library for creating small micro-services.
// Two main types implemented by this package are
// Kite for creating a micro-service server called "Kite" and
// Client for communicating with another kites.
package kite

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"

	"code.google.com/p/go.net/websocket"
	"github.com/dgrijalva/jwt-go"
	"github.com/koding/kite/config"
	"github.com/koding/kite/protocol"
	"github.com/koding/logging"
	"github.com/nu7hatch/gouuid"
)

var hostname string

func init() {
	var err error
	hostname, err = os.Hostname()
	if err != nil {
		panic(fmt.Sprintf("kite: cannot get hostname: %s", err.Error()))
	}
}

// Kite defines a single process that enables distributed service messaging
// amongst the peers it is connected. A Kite process acts as a Client and as a
// Server. That means it can receive request, process them, but it also can
// make request to other kites.
//
// Do not use this struct directly. Use kite.New function, add your handlers
// with HandleFunc mehtod, then call Start or Run method.
type Kite struct {
	Config *config.Config

	// Prints logging messages to stderr.
	Log logging.Logger

	// Contains different functions for authenticating user from request.
	// Keys are the authentication types (options.authentication.type).
	Authenticators map[string]func(*Request) error

	// Kontrol keys to trust. Kontrol will issue access tokens for kites
	// that are signed with the private counterpart of these keys.
	// Key data must be PEM encoded.
	trustedKontrolKeys map[string]string

	// Handlers added with Kite.HandleFunc().
	handlers map[string]HandlerFunc // method map for exported methods

	// Websocket server for handling incoming connections.
	server *websocket.Server

	// Handlers to call when a new connection is received.
	onConnectHandlers []func(*websocket.Conn)

	// Handlers to call before the first request of connected kite.
	onFirstRequestHandlers []func(*Client)

	// Handlers to call when a client has disconnected.
	onDisconnectHandlers []func(*Client)

	name    string
	version string
	id      string // Unique kite instance id
}

// New creates, initialize and then returns a new Kite instance. Version must
// be in 3-digit semantic form. Name is important that it's also used to be
// searched by others.
func New(name, version string) *Kite {
	if name == "" {
		panic("kite: name cannot be empty")
	}

	if digits := strings.Split(version, "."); len(digits) != 3 {
		panic("kite: version must be 3-digits semantic version")
	}

	kiteID, err := uuid.NewV4()
	if err != nil {
		panic(fmt.Sprintf("kite: cannot generate unique ID: %s", err.Error()))
	}

	k := &Kite{
		Config:             config.New(),
		Log:                newLogger(name),
		Authenticators:     make(map[string]func(*Request) error),
		trustedKontrolKeys: make(map[string]string),
		handlers:           make(map[string]HandlerFunc),
		server:             &websocket.Server{},
		name:               name,
		version:            version,
		id:                 kiteID.String(),
	}

	k.server.Handler = k.handleWS

	k.OnConnect(func(conn *websocket.Conn) { k.Log.Info("New connection from: %s", conn.Request().RemoteAddr) })
	k.OnFirstRequest(func(c *Client) { k.Log.Info("Connection %q is identified as %q", c.RemoteAddr(), c.Kite) })
	k.OnDisconnect(func(c *Client) { k.Log.Info("Client has disconnected: %q", c.Kite) })

	// Every kite should be able to authenticate the user from token.
	// Tokens are granted by Kontrol Kite.
	k.Authenticators["token"] = k.AuthenticateFromToken

	// A kite accepts requests with the same username.
	k.Authenticators["kiteKey"] = k.AuthenticateFromKiteKey

	// Register default methods.
	k.addDefaultHandlers()

	return k
}

// Kite returns the definition of the kite.
func (k *Kite) Kite() *protocol.Kite {
	return &protocol.Kite{
		Username:    k.Config.Username,
		Environment: k.Config.Environment,
		Name:        k.name,
		Version:     k.version,
		Region:      k.Config.Region,
		Hostname:    hostname,
		ID:          k.id,
	}
}

// Trust a Kontrol key for validating tokens.
func (k *Kite) TrustKontrolKey(issuer, key string) {
	k.trustedKontrolKeys[issuer] = key
}

func (k *Kite) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	k.server.ServeHTTP(w, req)
}

// handleWS is the websocket connection handler.
func (k *Kite) handleWS(ws *websocket.Conn) {
	defer ws.Close()

	// This Client also handles the connected client.
	// Since both sides can send/receive messages the client code is reused here.
	c := k.NewClient(nil)
	c.conn = ws

	k.callOnConnectHandlers(ws)

	// Run after methods are registered and delegate is set
	c.readLoop()

	c.callOnConnectHandlers()
	k.callOnDisconnectHandlers(c)
}

// newLogger returns a new logger object for desired name and level.
func newLogger(name string) logging.Logger {
	logger := logging.NewLogger(name)

	switch strings.ToUpper(os.Getenv("KITE_LOG_LEVEL")) {
	case "DEBUG":
		logger.SetLevel(logging.DEBUG)
	case "NOTICE":
		logger.SetLevel(logging.NOTICE)
	case "WARNING":
		logger.SetLevel(logging.WARNING)
	case "ERROR":
		logger.SetLevel(logging.ERROR)
	case "CRITICAL":
		logger.SetLevel(logging.CRITICAL)
	default:
		logger.SetLevel(logging.INFO)
	}

	return logger
}

func (k *Kite) OnConnect(handler func(conn *websocket.Conn)) {
	k.onConnectHandlers = append(k.onConnectHandlers, handler)
}

// OnFirstRequest registers a function to run when a Kite connects to this Kite.
func (k *Kite) OnFirstRequest(handler func(*Client)) {
	k.onFirstRequestHandlers = append(k.onFirstRequestHandlers, handler)
}

// OnDisconnect registers a function to run when a connected Kite is disconnected.
func (k *Kite) OnDisconnect(handler func(*Client)) {
	k.onDisconnectHandlers = append(k.onDisconnectHandlers, handler)
}

func (k *Kite) callOnConnectHandlers(conn *websocket.Conn) {
	for _, handler := range k.onConnectHandlers {
		handler(conn)
	}
}

func (k *Kite) callOnFirstRequestHandlers(c *Client) {
	for _, handler := range k.onFirstRequestHandlers {
		handler(c)
	}
}

func (k *Kite) callOnDisconnectHandlers(c *Client) {
	for _, handler := range k.onDisconnectHandlers {
		handler(c)
	}
}

// RSAKey returns the corresponding public key for the issuer of the token.
// It is called by jwt-go package when validating the signature in the token.
func (k *Kite) RSAKey(token *jwt.Token) ([]byte, error) {
	if k.Config.KontrolKey == "" {
		panic("kontrol key is not set in config")
	}

	issuer, ok := token.Claims["iss"].(string)
	if !ok {
		return nil, errors.New("token does not contain a valid issuer claim")
	}

	if issuer != k.Config.KontrolUser {
		return nil, fmt.Errorf("issuer is not trusted: %s", issuer)
	}

	return []byte(k.Config.KontrolKey), nil
}
