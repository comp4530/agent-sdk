// +build js,wasm

/*
Copyright SecureKey Technologies Inc. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"syscall/js"
	"time"

	"github.com/google/tink/go/keyset"
	"github.com/google/tink/go/mac"
	"github.com/google/uuid"
	"github.com/hyperledger/aries-framework-go/component/storage/jsindexeddb"
	ariesctrl "github.com/hyperledger/aries-framework-go/pkg/controller"
	controllercmd "github.com/hyperledger/aries-framework-go/pkg/controller/command"
	cryptoapi "github.com/hyperledger/aries-framework-go/pkg/crypto"
	"github.com/hyperledger/aries-framework-go/pkg/crypto/tinkcrypto"
	"github.com/hyperledger/aries-framework-go/pkg/crypto/tinkcrypto/primitive/composite/ecdh"
	"github.com/hyperledger/aries-framework-go/pkg/crypto/tinkcrypto/primitive/composite/keyio"
	"github.com/hyperledger/aries-framework-go/pkg/didcomm/messaging/msghandler"
	arieshttp "github.com/hyperledger/aries-framework-go/pkg/didcomm/transport/http"
	"github.com/hyperledger/aries-framework-go/pkg/didcomm/transport/ws"
	"github.com/hyperledger/aries-framework-go/pkg/doc/jose"
	"github.com/hyperledger/aries-framework-go/pkg/framework/aries"
	"github.com/hyperledger/aries-framework-go/pkg/framework/aries/api/vdr"
	"github.com/hyperledger/aries-framework-go/pkg/framework/context"
	"github.com/hyperledger/aries-framework-go/pkg/storage"
	"github.com/hyperledger/aries-framework-go/pkg/storage/edv"
	"github.com/hyperledger/aries-framework-go/pkg/vdr/httpbinding"
	"github.com/mitchellh/mapstructure"
	"github.com/trustbloc/edge-core/pkg/log"
	"github.com/trustbloc/trustbloc-did-method/pkg/vdri/trustbloc"

	agentctrl "github.com/trustbloc/agent-sdk/pkg/controller"
)

var logger = log.New("agent-js-worker")

const (
	wasmStartupTopic         = "asset-ready"
	handleResultFn           = "handleResult"
	commandPkg               = "agent"
	startFn                  = "Start"
	stopFn                   = "Stop"
	workers                  = 2
	storageTypeIndexedDB     = "indexedDB"
	storageTypeSDS           = "sds"
	invalidStorageTypeErrMsg = "%s is not a valid storage type. " +
		"Valid storage types: " + storageTypeSDS + ", " + storageTypeIndexedDB
)

// TODO Signal JS when WASM is loaded and ready.
//      This is being used in tests for now.
var (
	ready  = make(chan struct{}) //nolint:gochecknoglobals
	isTest = false               //nolint:gochecknoglobals
)

// command is received from JS.
type command struct {
	ID      string                 `json:"id"`
	Pkg     string                 `json:"pkg"`
	Fn      string                 `json:"fn"`
	Payload map[string]interface{} `json:"payload"`
}

// result is sent back to JS.
type result struct {
	ID      string                 `json:"id"`
	IsErr   bool                   `json:"isErr"`
	ErrMsg  string                 `json:"errMsg"`
	Payload map[string]interface{} `json:"payload"`
	Topic   string                 `json:"topic"`
}

// agentStartOpts contains opts for starting agent.
type agentStartOpts struct {
	Label                string   `json:"agent-default-label"`
	HTTPResolvers        []string `json:"http-resolver-url"`
	AutoAccept           bool     `json:"auto-accept"`
	OutboundTransport    []string `json:"outbound-transport"`
	TransportReturnRoute string   `json:"transport-return-route"`
	LogLevel             string   `json:"log-level"`
	StorageType          string   `json:"storageType"`
	IndexedDBNamespace   string   `json:"indexedDB-namespace"`
	SDSServerURL         string   `json:"sdsServerURL"`
	SDSVaultID           string   `json:"sdsVaultID"`
	BlocDomain           string   `json:"blocDomain"`
	TrustblocResolver    string   `json:"trustbloc-resolver"`
}

// main registers the 'handleMsg' function in the JS context's global scope to receive commands.
// Results are posted back to the 'handleResult' JS function.
func main() {
	// TODO: capacity was added due to deadlock. Looks like js worker are not able to pick up 'output chan *result'.
	//  Another fix for that is to wrap 'in <- cmd' in a goroutine. e.g go func() { in <- cmd }()
	//  We need to figure out what is the root cause of deadlock and fix it properly.
	input := make(chan *command, 10) // nolint: gomnd
	output := make(chan *result)

	go pipe(input, output)

	go sendTo(output)

	js.Global().Set("handleMsg", js.FuncOf(takeFrom(input)))

	postInitMsg()

	if isTest {
		ready <- struct{}{}
	}

	select {}
}

func takeFrom(in chan *command) func(js.Value, []js.Value) interface{} {
	return func(_ js.Value, args []js.Value) interface{} {
		cmd := &command{}
		if err := json.Unmarshal([]byte(args[0].String()), cmd); err != nil {
			logger.Errorf("agent wasm: unable to unmarshal input=%s. err=%s", args[0].String(), err)

			return nil
		}

		in <- cmd

		return nil
	}
}

func pipe(input chan *command, output chan *result) {
	handlers := testHandlers()

	addAgentHandlers(handlers)

	for w := 0; w < workers; w++ {
		go worker(input, output, handlers)
	}
}

func worker(input chan *command, output chan *result, handlers map[string]map[string]func(*command) *result) {
	for c := range input {
		if c.ID == "" {
			logger.Warnf("agent wasm: missing ID for input: %v", c)
		}

		if pkg, found := handlers[c.Pkg]; found {
			if fn, found := pkg[c.Fn]; found {
				output <- fn(c)

				continue
			}
		}

		output <- handlerNotFoundErr(c)
	}
}

func sendTo(out chan *result) {
	for r := range out {
		out, err := json.Marshal(r)
		if err != nil {
			logger.Errorf("agent wasm: failed to marshal response for id=%s err=%s ", r.ID, err)
		}

		js.Global().Call(handleResultFn, string(out))
	}
}

func testHandlers() map[string]map[string]func(*command) *result {
	return map[string]map[string]func(*command) *result{
		"test": {
			"echo": func(c *command) *result {
				return &result{
					ID:      c.ID,
					Payload: map[string]interface{}{"echo": c.Payload},
				}
			},
			"throwError": func(c *command) *result {
				return newErrResult(c.ID, "an error !!")
			},
			"timeout": func(c *command) *result {
				const echoTimeout = 10 * time.Second

				time.Sleep(echoTimeout)

				return &result{
					ID:      c.ID,
					Payload: map[string]interface{}{"echo": c.Payload},
				}
			},
		},
	}
}

func isStartCommand(c *command) bool {
	return c.Pkg == commandPkg && c.Fn == startFn
}

func isStopCommand(c *command) bool {
	return c.Pkg == commandPkg && c.Fn == stopFn
}

func handlerNotFoundErr(c *command) *result {
	if isStartCommand(c) {
		return newErrResult(c.ID, "Agent already started")
	} else if isStopCommand(c) {
		return newErrResult(c.ID, "Agent not running")
	}

	return newErrResult(c.ID, fmt.Sprintf("invalid pkg/fn: %s/%s, make sure agent is started", c.Pkg, c.Fn))
}

func addAgentHandlers(pkgMap map[string]map[string]func(*command) *result) {
	fnMap := make(map[string]func(*command) *result)
	fnMap[startFn] = func(c *command) *result {
		cOpts, err := startOpts(c.Payload)
		if err != nil {
			return newErrResult(c.ID, err.Error())
		}

		err = setLogLevel(cOpts.LogLevel)
		if err != nil {
			return newErrResult(c.ID, err.Error())
		}

		options, err := agentOpts(cOpts)
		if err != nil {
			return newErrResult(c.ID, err.Error())
		}

		msgHandler := msghandler.NewRegistrar()
		options = append(options, aries.WithMessageServiceProvider(msgHandler))

		a, err := aries.New(options...)
		if err != nil {
			return newErrResult(c.ID, err.Error())
		}

		ctx, err := a.Context()
		if err != nil {
			return newErrResult(c.ID, err.Error())
		}

		handlers, err := getAriesHandlers(ctx, msgHandler, cOpts)
		if err != nil {
			return newErrResult(c.ID, err.Error())
		}

		agentHandlers, err := getAgentHandlers(ctx, cOpts)
		if err != nil {
			return newErrResult(c.ID, err.Error())
		}

		handlers = append(handlers, agentHandlers...)

		// add command handlers
		addCommandHandlers(handlers, pkgMap)

		// add stop agent handler
		addStopAgentHandler(a, pkgMap)

		return &result{
			ID:      c.ID,
			Payload: map[string]interface{}{"message": "agent started successfully"},
		}
	}

	pkgMap[commandPkg] = fnMap
}

type execFn func(rw io.Writer, req io.Reader) error

type commandHandler struct {
	name   string
	method string
	exec   execFn
}

func getAriesHandlers(ctx *context.Provider, r controllercmd.MessageHandler,
	opts *agentStartOpts) ([]commandHandler, error) {
	handlers, err := ariesctrl.GetCommandHandlers(ctx, ariesctrl.WithMessageHandler(r),
		ariesctrl.WithDefaultLabel(opts.Label), ariesctrl.WithNotifier(&jsNotifier{}))
	if err != nil {
		return nil, err
	}

	var hh []commandHandler

	for _, h := range handlers {
		handle := h.Handle()

		hh = append(hh, commandHandler{
			name:   h.Name(),
			method: h.Method(),
			exec: func(rw io.Writer, req io.Reader) error {
				e := handle(rw, req)
				if e != nil {
					return fmt.Errorf("code: %+v, message: %s", e.Code(), e.Error())
				}

				return nil
			},
		})
	}

	return hh, nil
}

func getAgentHandlers(ctx *context.Provider, opts *agentStartOpts) ([]commandHandler, error) {
	handlers, err := agentctrl.GetCommandHandlers(ctx, agentctrl.WithBlocDomain(opts.BlocDomain))
	if err != nil {
		return nil, err
	}

	var hh []commandHandler

	for _, h := range handlers {
		handle := h.Handle()

		hh = append(hh, commandHandler{
			name:   h.Name(),
			method: h.Method(),
			exec: func(rw io.Writer, req io.Reader) error {
				e := handle(rw, req)
				if e != nil {
					return fmt.Errorf("code: %+v, message: %s", e.Code(), e.Error())
				}

				return nil
			},
		})
	}

	return hh, nil
}

func addCommandHandlers(handlers []commandHandler, pkgMap map[string]map[string]func(*command) *result) {
	for _, h := range handlers {
		fnMap, ok := pkgMap[h.name]
		if !ok {
			fnMap = make(map[string]func(*command) *result)
		}

		fnMap[h.method] = cmdExecToFn(h.exec)
		pkgMap[h.name] = fnMap
	}
}

func cmdExecToFn(exec execFn) func(*command) *result {
	return func(c *command) *result {
		b, er := json.Marshal(c.Payload)
		if er != nil {
			return &result{
				ID:     c.ID,
				IsErr:  true,
				ErrMsg: fmt.Sprintf("agent wasm: failed to unmarshal payload. err=%s", er),
			}
		}

		req := bytes.NewBuffer(b)

		var buf bytes.Buffer

		err := exec(&buf, req)
		if err != nil {
			return newErrResult(c.ID, err.Error())
		}

		payload := make(map[string]interface{})

		if len(buf.Bytes()) > 0 {
			if err := json.Unmarshal(buf.Bytes(), &payload); err != nil {
				return &result{
					ID:     c.ID,
					IsErr:  true,
					ErrMsg: fmt.Sprintf("agent wasm: failed to unmarshal command result=%+v err=%s", buf.String(), err),
				}
			}
		}

		return &result{
			ID:      c.ID,
			Payload: payload,
		}
	}
}

func addStopAgentHandler(a io.Closer, pkgMap map[string]map[string]func(*command) *result) {
	fnMap := make(map[string]func(*command) *result)
	fnMap[stopFn] = func(c *command) *result {
		err := a.Close()
		if err != nil {
			return newErrResult(c.ID, err.Error())
		}

		// reset handlers when stopped
		for k := range pkgMap {
			delete(pkgMap, k)
		}

		// put back start command once stopped
		addAgentHandlers(pkgMap)

		return &result{
			ID:      c.ID,
			Payload: map[string]interface{}{"message": "agent stopped"},
		}
	}
	pkgMap[commandPkg] = fnMap
}

func newErrResult(id, msg string) *result {
	return &result{
		ID:     id,
		IsErr:  true,
		ErrMsg: "agent wasm: " + msg,
	}
}

func startOpts(payload map[string]interface{}) (*agentStartOpts, error) {
	opts := &agentStartOpts{}

	decoder, err := mapstructure.NewDecoder(&mapstructure.DecoderConfig{
		TagName: "json",
		Result:  opts,
	})
	if err != nil {
		return nil, err
	}

	err = decoder.Decode(payload)
	if err != nil {
		return nil, err
	}

	return opts, nil
}

func createVDRs(resolvers []string, trustblocDomain, trustblocResolver string) ([]vdr.VDR, error) {
	const numPartsResolverOption = 2
	// set maps resolver to its methods
	// e.g the set of ["trustbloc@http://resolver.com", "v1@http://resolver.com"] will be
	// {"http://resolver.com": {"trustbloc":{}, "v1":{} }}
	set := make(map[string]map[string]struct{})
	// order maps URL to its initial index
	order := make(map[string]int)

	idx := -1

	for _, resolver := range resolvers {
		r := strings.Split(resolver, "@")
		if len(r) != numPartsResolverOption {
			return nil, fmt.Errorf("invalid http resolver options found: %s", resolver)
		}

		if set[r[1]] == nil {
			set[r[1]] = map[string]struct{}{}
			idx++
		}

		order[r[1]] = idx

		set[r[1]][r[0]] = struct{}{}
	}

	VDRs := make([]vdr.VDR, len(set), len(set)+1)

	for url := range set {
		methods := set[url]

		resolverVDR, err := httpbinding.New(url, httpbinding.WithAccept(func(method string) bool {
			_, ok := methods[method]

			return ok
		}))
		if err != nil {
			return nil, fmt.Errorf("failed to create new universal resolver vdr: %w", err)
		}

		VDRs[order[url]] = resolverVDR
	}

	VDRs = append(VDRs, trustbloc.New(
		trustbloc.WithDomain(trustblocDomain),
		trustbloc.WithResolverURL(trustblocResolver),
	))

	return VDRs, nil
}

func agentOpts(opts *agentStartOpts) ([]aries.Option, error) {
	msgHandler := msghandler.NewRegistrar()

	var options []aries.Option
	options = append(options, aries.WithMessageServiceProvider(msgHandler))

	if opts.TransportReturnRoute != "" {
		options = append(options, aries.WithTransportReturnRoute(opts.TransportReturnRoute))
	}

	store, err := createStore(opts)
	if err != nil {
		return nil, fmt.Errorf("failed to create storage provider: %w", err)
	}

	VDRs, err := createVDRs(opts.HTTPResolvers, opts.BlocDomain, opts.TrustblocResolver)
	if err != nil {
		return nil, err
	}

	for i := range VDRs {
		options = append(options, aries.WithVDR(VDRs[i]))
	}

	options = append(options, aries.WithStoreProvider(store))

	for _, transport := range opts.OutboundTransport {
		switch transport {
		case "http":
			outbound, err := arieshttp.NewOutbound(arieshttp.WithOutboundHTTPClient(&http.Client{}))
			if err != nil {
				return nil, err
			}

			options = append(options, aries.WithOutboundTransports(outbound))
		case "ws":
			options = append(options, aries.WithOutboundTransports(ws.NewOutbound()))
		default:
			return nil, fmt.Errorf("unsupported transport : %s", transport)
		}
	}

	return options, nil
}

func createStore(opts *agentStartOpts) (storage.Provider, error) {
	switch opts.StorageType {
	case storageTypeSDS:
		store, err := createEDVProvider(opts.SDSServerURL, opts.SDSVaultID)
		if err != nil {
			return nil, fmt.Errorf("failed to create EDV provider: %w", err)
		}

		return store, nil
	case storageTypeIndexedDB:
		store, err := jsindexeddb.NewProvider(opts.IndexedDBNamespace)
		if err != nil {
			return nil, err
		}

		return store, nil
	default:
		return nil, fmt.Errorf(invalidStorageTypeErrMsg, opts.StorageType)
	}
}

// TODO (#43): Use a persistent key instead of creating a new one every time this starts.

// TODO (#44): Use the KMS from the Aries framework for the EDV provider. Right now this isn't possible since
//  the KMS in the Aries framework uses the provider passed in via WithStorageProvider for KMS storage.
//  An EDV server can't use itself for KMS storage as this causes a chicken and egg problem.
//  aries-framework-go will need to be updated to allow a different provider for just the KMS to resolve this.

// TODO (#45): Use the Aries Crypto object instantiated in the framework for the EDV provider instead
//  of creating a new one here. This will require some sort of change to aries-framework-go, since the
//  Aries Crypto object isn't available until the framework has been initialized.
func createEDVProvider(edvServerURL, vaultID string) (storage.Provider, error) {
	macCrypto, err := newMACCrypto()
	if err != nil {
		return nil, fmt.Errorf("failed to create new MAC Crypto for EDV REST provider: %w", err)
	}

	edvRESTProvider, err := edv.NewRESTProvider(edvServerURL, vaultID, macCrypto)
	if err != nil {
		return nil, fmt.Errorf("failed to create new EDV REST provider: %w", err)
	}

	encryptedFormatter, err := createEncryptedFormatter()
	if err != nil {
		return nil, fmt.Errorf("failed to create new encrypted formatter: %w", err)
	}

	return storage.NewFormattedProvider(edvRESTProvider, encryptedFormatter), nil
}

func setLogLevel(logLevel string) error {
	if logLevel != "" {
		level, err := log.ParseLevel(logLevel)
		if err != nil {
			return err
		}

		log.SetLevel("", level)
		logger.Infof("log level set to `%s`", logLevel)
	}

	return nil
}

// jsNotifier notifies about all incoming events.
type jsNotifier struct {
}

// Notify is mock implementation of webhook notifier Notify().
func (n *jsNotifier) Notify(topic string, message []byte) error {
	payload := make(map[string]interface{})
	if err := json.Unmarshal(message, &payload); err != nil {
		return err
	}

	out, err := json.Marshal(&result{
		ID:      uuid.New().String(),
		Topic:   topic,
		Payload: payload,
	})
	if err != nil {
		return err
	}

	js.Global().Call(handleResultFn, string(out))

	return nil
}

func postInitMsg() {
	if isTest {
		return
	}

	out, err := json.Marshal(&result{
		ID:    uuid.New().String(),
		Topic: wasmStartupTopic,
	})
	if err != nil {
		panic(err)
	}

	js.Global().Call(handleResultFn, string(out))
}

func newMACCrypto() (*edv.MACCrypto, error) {
	kh, err := keyset.NewHandle(mac.HMACSHA256Tag256KeyTemplate())
	if err != nil {
		return nil, fmt.Errorf("failed to create new HMAC key handle: %w", err)
	}

	crypto, err := tinkcrypto.New()
	if err != nil {
		return nil, fmt.Errorf("failed to create new tink crypto: %w", err)
	}

	return edv.NewMACCrypto(kh, crypto), nil
}

func createEncryptedFormatter() (*edv.EncryptedFormatter, error) {
	encrypter, decrypter, err := createEncrypterAndDecrypter()
	if err != nil {
		return nil, fmt.Errorf("failed to create encrypter and decrypter: %w", err)
	}

	return edv.NewEncryptedFormatter(encrypter, decrypter), nil
}

//nolint: unparam
func createEncrypterAndDecrypter() (*jose.JWEEncrypt, *jose.JWEDecrypt, error) {
	keyHandle, err := keyset.NewHandle(ecdh.ECDH256KWAES256GCMKeyTemplate())
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create new ECDHES key handle: %w", err)
	}

	pubKH, err := keyHandle.Public()
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get public key handle from ECDHES key handle: %w", err)
	}

	buf := new(bytes.Buffer)
	pubKeyWriter := keyio.NewWriter(buf)

	err = pubKH.WriteWithNoSecrets(pubKeyWriter)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to write keyset: %w", err)
	}

	ecPubKey := new(cryptoapi.PublicKey)

	// TODO how to get kid
	// ecPubKey.KID = kid

	err = json.Unmarshal(buf.Bytes(), ecPubKey)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to unmarshal bytes to a public key: %w", err)
	}

	// TODO how to get crypto
	// encrypter, err := jose.NewJWEEncrypt(jose.A256GCM, "EDVEncryptedDocument", "", nil,
	//	[]*cryptoapi.PublicKey{ecPubKey})
	// if err != nil {
	//	return nil, nil, fmt.Errorf("failed to create a new JWE encrypter: %w", err)
	// }

	// TODO how to get crypto and KeyManager
	// return encrypter,jose.NewJWEDecrypt(nil, keyHandle), nil
	return nil, nil, nil
}
