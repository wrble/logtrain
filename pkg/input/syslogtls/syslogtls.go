package syslogtls

import (
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"errors"
	server "github.com/mcuadros/go-syslog"
	syslog "github.com/trevorlinton/remote_syslog2/syslog"
	"time"
)

/* Handles Syslog TLS inputs */
type HandlerSyslogTLS struct {
	errors     chan error
	packets    chan syslog.Packet
	stop       chan struct{}
	channel    server.LogPartsChannel
	server     *server.Server
	serverName string
	keyPem     string
	certPem    string
	caPem      string
	address    string
}

func (handler *HandlerSyslogTLS) getServerConfig() (*tls.Config, error) {
	capool, err := x509.SystemCertPool()
	if err != nil {
		capool = x509.NewCertPool()
	}
	if handler.caPem != "" {
		if ok := capool.AppendCertsFromPEM([]byte(handler.caPem)); !ok {
			return nil, errors.New("unable to parse pem")
		}
	}

	cert, err := tls.X509KeyPair([]byte(handler.certPem), []byte(handler.keyPem))
	if err != nil {
		return nil, err
	}

	config := tls.Config{
		Certificates: []tls.Certificate{cert},
		ServerName:   handler.serverName,
		RootCAs:      capool,
	}
	config.Rand = rand.Reader
	return &config, nil
}

// Close input handler.
func (handler *HandlerSyslogTLS) Close() error {
	handler.stop <- struct{}{}
	handler.server.Kill()
	close(handler.packets)
	close(handler.errors)
	close(handler.channel)
	close(handler.stop)
	return nil
}

func defaultTlsPeerName(tlsConn *tls.Conn) (tlsPeer string, ok bool) {
	state := tlsConn.ConnectionState()
	if len(state.PeerCertificates) <= 0 {
		return "", true
	}
	cn := state.PeerCertificates[0].Subject.CommonName
	return cn, true
}

// Dial input handler.
func (handler *HandlerSyslogTLS) Dial() error {
	if handler.server != nil {
		return errors.New("dial may only be called once")
	}
	config, err := handler.getServerConfig()
	if err != nil {
		return err
	}
	handler.server = server.NewServer()
	handler.server.SetTlsPeerNameFunc(defaultTlsPeerName)
	handler.server.SetFormat(server.RFC6587)
	handler.server.SetHandler(server.NewChannelHandler(handler.channel))
	if err := handler.server.ListenTCPTLS(handler.address, config); err != nil {
		return err
	}
	if err := handler.server.Boot(); err != nil {
		return err
	}
	go func() {
		for {
			select {
			case message, ok := <-handler.channel:
				if !ok {
					return
				}
				var severity int
				var facility int
				var hostname string
				var tag string
				var timestamp time.Time = time.Now()
				var msg string
				if s, ok := message["severity"].(int); ok {
					severity = s
				}
				if f, ok := message["facility"].(int); ok {
					facility = f
				}
				if h, ok := message["hostname"].(string); ok {
					hostname = h
				}
				if m, ok := message["message"].(string); ok {
					msg = m
				}
				if t, ok := message["app_name"].(string); ok {
					tag = t
				}
				if i, ok := message["time"].(time.Time); ok {
					timestamp = i
				}
				handler.Packets() <- syslog.Packet{
					Severity: syslog.Priority(severity),
					Facility: syslog.Priority(facility),
					Hostname: hostname,
					Tag:      tag,
					Time:     timestamp,
					Message:  msg,
				}
			case <-handler.stop:
				return
			}
		}
	}()
	return nil
}

// Errors channel that sends errors occuring from input
func (handler *HandlerSyslogTLS) Errors() chan error {
	return handler.errors
}

// Packets channel that sends incoming packets from input
func (handler *HandlerSyslogTLS) Packets() chan syslog.Packet {
	return handler.packets
}

// Pools returns whether the connection pools connections
func (handler *HandlerSyslogTLS) Pools() bool {
	return true
}

// Create a new syslog tls input
func Create(serverName string, keyPem string, certPem string, caPem string, address string) (*HandlerSyslogTLS, error) {
	return &HandlerSyslogTLS{
		errors:     make(chan error, 1),
		packets:    make(chan syslog.Packet, 100),
		stop:       make(chan struct{}, 1),
		channel:    make(server.LogPartsChannel),
		server:     nil,
		serverName: serverName,
		keyPem:     keyPem,
		certPem:    certPem,
		caPem:      caPem,
		address:    address,
	}, nil
}
