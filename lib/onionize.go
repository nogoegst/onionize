// onionize.go - onionize things.
//
// To the extent possible under law, Ivan Markin waived all copyright
// and related or neighboring rights to this module of onionize, using the creative
// commons "cc0" public domain dedication. See LICENSE or
// <http://creativecommons.org/publicdomain/zero/1.0/> for full details.

package onionize

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"

	"github.com/nogoegst/bulb"
	"github.com/nogoegst/onionutil"
)

const slugLengthB32 = 16

type Parameters struct {
	Path            string
	Zip             bool
	Slug            bool
	ControlPath     string
	ControlPassword string
	Passphrase      string
	Debug           bool
}

func Onionize(p Parameters, linkChan chan<- url.URL) error {
	var handler http.Handler
	var link string
	target, err := url.Parse(p.Path)
	if err != nil {
		return fmt.Errorf("Unable to parse target URL: %v", err)
	}
	switch target.Scheme {
	case "http", "https":
		handler = OnionReverseHTTPProxy(target)
	case "":
		handler, link, err = FileServer(p.Path, p.Slug, p.Zip, p.Debug)
		if err != nil {
			return err
		}
	default:
		return fmt.Errorf("Unsupported target type: %s", target.Scheme)
	}
	server := &http.Server{Handler: handler}

	// Connect to a running tor instance
	if p.ControlPath == "" {
		p.ControlPath = "default://"
	}
	c, err := bulb.DialURL(p.ControlPath)
	if err != nil {
		return fmt.Errorf("Failed to connect to control socket: %v", err)
	}
	defer c.Close()

	// See what's really going on under the hood
	c.Debug(p.Debug)

	// Authenticate with the control port
	if err := c.Authenticate(p.ControlPassword); err != nil {
		return fmt.Errorf("Authentication failed: %v", err)
	}
	// Derive onion service keymaterial from passphrase or generate a new one
	nocfg := &bulb.NewOnionConfig{
		DiscardPK:      true,
		AwaitForUpload: true,
	}
	if p.Passphrase != "" {
		keyrd, err := onionutil.KeystreamReader([]byte(p.Passphrase), []byte("onionize-keygen"))
		if err != nil {
			return fmt.Errorf("Unable to create keystream: %v", err)
		}
		privOnionKey, err := onionutil.GenerateOnionKey(keyrd, "current")
		if err != nil {
			return fmt.Errorf("Unable to generate onion key: %v", err)
		}
		nocfg.PrivateKey = privOnionKey
	}
	const loopbackAddr = "127.0.0.1:0"

	// Listen on the loopback interface.
	tcpListener, err := net.Listen("tcp4", loopbackAddr)
	if err != nil {
		return err
	}
	tAddr, ok := tcpListener.Addr().(*net.TCPAddr)
	if !ok {
		tcpListener.Close()
		return fmt.Errorf("Failed to extract local port")
	}

	virtPort := uint16(80)
	targetPortStr := strconv.FormatUint((uint64)(tAddr.Port), 10)
	portSpec := bulb.OnionPortSpec{
		VirtPort: virtPort,
		Target:   targetPortStr,
	}
	nocfg.PortSpecs = []bulb.OnionPortSpec{portSpec}
	oi, err := c.NewOnion(nocfg)
	if err != nil {
		return fmt.Errorf("Error occured while creating an onion service: %v", err)
	}
	// Track if tor went down
	// TODO: Signal from here to perform graceful shutdown and display a message
	go func() {
		for {
			_, err := c.NextEvent()
			if err != nil {
				log.Fatalf("Lost connection to tor: %v", err)
			}
		}
	}()

	onionURL := url.URL{
		Scheme:	"http",
		Host: fmt.Sprintf("%s.onion", oi.OnionID),
		Path: link,
	}

	// Return the link to the service
	linkChan <- onionURL
	// Run a webservice
	err = server.Serve(tcpListener)
	if err != nil {
		return fmt.Errorf("Cannot serve HTTP: %v", err)
	}
	return nil
}
