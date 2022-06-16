package proxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"strings"

	"github.com/lqqyt2423/go-mitmproxy/cert"
	"github.com/lqqyt2423/go-mitmproxy/flow"
)

// 模拟了标准库中 server 运行，目的是仅通过当前进程内存转发 socket 数据，不需要经过 tcp 或 unix socket

// mock net.Listener
type middleListener struct {
	connChan chan net.Conn
}

func (l *middleListener) Accept() (net.Conn, error) { return <-l.connChan, nil }
func (l *middleListener) Close() error              { return nil }
func (l *middleListener) Addr() net.Addr            { return nil }

type pipeAddr struct {
	remoteAddr string
}

func (pipeAddr) Network() string   { return "pipe" }
func (a *pipeAddr) String() string { return a.remoteAddr }

// 建立客户端和服务端通信的通道
func newPipes(req *http.Request) (net.Conn, *pipeConn) {
	client, srv := net.Pipe()
	server := newPipeConn(srv, req)
	return client, server
}

// add Peek method for conn
type pipeConn struct {
	net.Conn
	r           *bufio.Reader
	host        string
	remoteAddr  string
	connContext *flow.ConnContext
}

func newPipeConn(c net.Conn, req *http.Request) *pipeConn {
	return &pipeConn{
		Conn:        c,
		r:           bufio.NewReader(c),
		host:        req.Host,
		remoteAddr:  req.RemoteAddr,
		connContext: req.Context().Value(flow.ConnContextKey).(*flow.ConnContext),
	}
}

func (c *pipeConn) Peek(n int) ([]byte, error) {
	return c.r.Peek(n)
}

func (c *pipeConn) Read(data []byte) (int, error) {
	return c.r.Read(data)
}

func (c *pipeConn) RemoteAddr() net.Addr {
	return &pipeAddr{remoteAddr: c.remoteAddr}
}

// Middle: man-in-the-middle
type Middle struct {
	Proxy    *Proxy
	CA       *cert.CA
	Listener net.Listener
	Server   *http.Server
}

func NewMiddle(proxy *Proxy, caPath string) (Interceptor, error) {
	ca, err := cert.NewCA(caPath)
	if err != nil {
		return nil, err
	}

	m := &Middle{
		Proxy: proxy,
		CA:    ca,
	}

	server := &http.Server{
		Handler: m,
		// IdleTimeout: 5 * time.Second,

		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			return context.WithValue(ctx, flow.ConnContextKey, c.(*tls.Conn).NetConn().(*pipeConn).connContext)
		},

		TLSNextProto: make(map[string]func(*http.Server, *tls.Conn, http.Handler)), // disable http2
		TLSConfig: &tls.Config{
			GetCertificate: func(chi *tls.ClientHelloInfo) (*tls.Certificate, error) {
				log.Debugf("Middle GetCertificate ServerName: %v\n", chi.ServerName)
				return ca.GetCert(chi.ServerName)
			},
		},
	}

	m.Server = server
	m.Listener = &middleListener{make(chan net.Conn)}

	return m, nil
}

func (m *Middle) Start() error {
	return m.Server.ServeTLS(m.Listener, "", "")
}

// todo: should block until ServerConnected
func (m *Middle) Dial(req *http.Request) (net.Conn, error) {
	pipeClientConn, pipeServerConn := newPipes(req)
	go m.intercept(pipeServerConn)
	return pipeClientConn, nil
}

func (m *Middle) ServeHTTP(res http.ResponseWriter, req *http.Request) {
	if strings.EqualFold(req.Header.Get("Connection"), "Upgrade") && strings.EqualFold(req.Header.Get("Upgrade"), "websocket") {
		// wss
		DefaultWebSocket.WSS(res, req)
		return
	}

	if req.URL.Scheme == "" {
		req.URL.Scheme = "https"
	}
	if req.URL.Host == "" {
		req.URL.Host = req.Host
	}
	m.Proxy.ServeHTTP(res, req)
}

// 解析 connect 流量
// 如果是 tls 流量，则进入 listener.Accept => Middle.ServeHTTP
// 否则很可能是 ws 流量
func (m *Middle) intercept(pipeServerConn *pipeConn) {
	log := log.WithField("in", "Middle.intercept").WithField("host", pipeServerConn.host)

	buf, err := pipeServerConn.Peek(3)
	if err != nil {
		log.Errorf("Peek error: %v\n", err)
		pipeServerConn.Close()
		return
	}

	// https://github.com/mitmproxy/mitmproxy/blob/main/mitmproxy/net/tls.py is_tls_record_magic
	if buf[0] == 0x16 && buf[1] == 0x03 && buf[2] <= 0x03 {
		// tls
		pipeServerConn.connContext.Client.Tls = true
		pipeServerConn.connContext.InitHttpsServer(
			m.Proxy.Opts.SslInsecure,
			func(c net.Conn) net.Conn {
				return &serverConn{
					Conn:    c,
					proxy:   m.Proxy,
					connCtx: pipeServerConn.connContext,
				}
			},
			func() {
				for _, addon := range m.Proxy.Addons {
					addon.ServerConnected(pipeServerConn.connContext)
				}
			},
		)
		m.Listener.(*middleListener).connChan <- pipeServerConn
	} else {
		// ws
		DefaultWebSocket.WS(pipeServerConn, pipeServerConn.host)
	}
}
