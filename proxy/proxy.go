package proxy

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"

	"github.com/lqqyt2423/go-mitmproxy/addon"
	"github.com/lqqyt2423/go-mitmproxy/flow"
	_log "github.com/sirupsen/logrus"
)

var log = _log.WithField("at", "proxy")

type Options struct {
	Debug             int
	Addr              string
	StreamLargeBodies int64 // 当请求或响应体大于此字节时，转为 stream 模式
	SslInsecure       bool
	CaRootPath        string
}

type Proxy struct {
	Opts        *Options
	Version     string
	Server      *http.Server
	Interceptor Interceptor
	Addons      []addon.Addon
}

type proxyListener struct {
	net.Listener
	proxy *Proxy
}

func (l *proxyListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}

	return &proxyConn{
		Conn:  c,
		proxy: l.proxy,
	}, nil
}

type proxyConn struct {
	net.Conn
	proxy    *Proxy
	connCtx  *flow.ConnContext
	closed   bool
	closeErr error
}

func (c *proxyConn) Close() error {
	log.Debugln("in proxyConn close")
	if c.closed {
		return c.closeErr
	}

	c.closed = true
	c.closeErr = c.Conn.Close()

	for _, addon := range c.proxy.Addons {
		addon.ClientDisconnected(c.connCtx.Client)
	}

	if c.connCtx.Server != nil && c.connCtx.Server.Conn != nil {
		c.connCtx.Server.Conn.Close()
	}

	return c.closeErr
}

type serverConn struct {
	net.Conn
	proxy    *Proxy
	connCtx  *flow.ConnContext
	closed   bool
	closeErr error
}

func (c *serverConn) Close() error {
	log.Debugln("in http serverConn close")
	if c.closed {
		return c.closeErr
	}

	c.closed = true
	c.closeErr = c.Conn.Close()

	for _, addon := range c.proxy.Addons {
		addon.ServerDisconnected(c.connCtx)
	}

	c.connCtx.Client.Conn.Close()

	return c.closeErr
}

func NewProxy(opts *Options) (*Proxy, error) {
	proxy := new(Proxy)
	proxy.Opts = opts
	proxy.Version = "0.2.0"

	proxy.Server = &http.Server{
		Addr:    opts.Addr,
		Handler: proxy,
		// IdleTimeout: 5 * time.Second,

		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			connCtx := flow.NewConnContext(c)
			for _, addon := range proxy.Addons {
				addon.ClientConnected(connCtx.Client)
			}
			c.(*proxyConn).connCtx = connCtx
			return context.WithValue(ctx, flow.ConnContextKey, connCtx)
		},
	}

	interceptor, err := NewMiddle(proxy, opts.CaRootPath)
	if err != nil {
		return nil, err
	}
	proxy.Interceptor = interceptor

	if opts.StreamLargeBodies <= 0 {
		opts.StreamLargeBodies = 1024 * 1024 * 5 // default: 5mb
	}

	proxy.Addons = make([]addon.Addon, 0)

	return proxy, nil
}

func (proxy *Proxy) AddAddon(addon addon.Addon) {
	proxy.Addons = append(proxy.Addons, addon)
}

func (proxy *Proxy) Start() error {
	errChan := make(chan error)

	go func() {
		log.Infof("Proxy start listen at %v\n", proxy.Server.Addr)
		addr := proxy.Server.Addr
		if addr == "" {
			addr = ":http"
		}
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			errChan <- err
			return
		}
		pln := &proxyListener{
			Listener: ln,
			proxy:    proxy,
		}
		err = proxy.Server.Serve(pln)
		errChan <- err
	}()

	go func() {
		err := proxy.Interceptor.Start()
		errChan <- err
	}()

	err := <-errChan
	return err
}

func (proxy *Proxy) ServeHTTP(res http.ResponseWriter, req *http.Request) {
	if req.Method == "CONNECT" {
		proxy.handleConnect(res, req)
		return
	}

	log := log.WithFields(_log.Fields{
		"in":     "Proxy.ServeHTTP",
		"url":    req.URL,
		"method": req.Method,
	})

	log.Debug("receive request")

	if !req.URL.IsAbs() || req.URL.Host == "" {
		res.WriteHeader(400)
		_, err := io.WriteString(res, "此为代理服务器，不能直接发起请求")
		if err != nil {
			log.Error(err)
		}
		return
	}

	reply := func(response *flow.Response, body io.Reader) {
		if response.Header != nil {
			for key, value := range response.Header {
				for _, v := range value {
					res.Header().Add(key, v)
				}
			}
		}
		res.WriteHeader(response.StatusCode)

		if body != nil {
			_, err := io.Copy(res, body)
			if err != nil {
				LogErr(log, err)
			}
		} else if response.Body != nil && len(response.Body) > 0 {
			_, err := res.Write(response.Body)
			if err != nil {
				LogErr(log, err)
			}
		}
	}

	// when addons panic
	defer func() {
		if err := recover(); err != nil {
			log.Warnf("Recovered: %v\n", err)
		}
	}()

	f := flow.NewFlow()
	f.Request = flow.NewRequest(req)
	f.ConnContext = req.Context().Value(flow.ConnContextKey).(*flow.ConnContext)
	defer f.Finish()

	// trigger addon event Requestheaders
	for _, addon := range proxy.Addons {
		addon.Requestheaders(f)
		if f.Response != nil {
			reply(f.Response, nil)
			return
		}
	}

	// Read request body
	var reqBody io.Reader = req.Body
	if !f.Stream {
		reqBuf, r, err := ReaderToBuffer(req.Body, proxy.Opts.StreamLargeBodies)
		reqBody = r
		if err != nil {
			log.Error(err)
			res.WriteHeader(502)
			return
		}

		if reqBuf == nil {
			log.Warnf("request body size >= %v\n", proxy.Opts.StreamLargeBodies)
			f.Stream = true
		} else {
			f.Request.Body = reqBuf

			// trigger addon event Request
			for _, addon := range proxy.Addons {
				addon.Request(f)
				if f.Response != nil {
					reply(f.Response, nil)
					return
				}
			}
			reqBody = bytes.NewReader(f.Request.Body)
		}
	}

	proxyReq, err := http.NewRequest(f.Request.Method, f.Request.URL.String(), reqBody)
	if err != nil {
		log.Error(err)
		res.WriteHeader(502)
		return
	}

	for key, value := range f.Request.Header {
		for _, v := range value {
			proxyReq.Header.Add(key, v)
		}
	}

	f.ConnContext.InitHttpServer(
		proxy.Opts.SslInsecure,
		func(c net.Conn) net.Conn {
			return &serverConn{
				Conn:    c,
				proxy:   proxy,
				connCtx: f.ConnContext,
			}
		},
		func() {
			for _, addon := range proxy.Addons {
				addon.ServerConnected(f.ConnContext)
			}
		},
	)

	proxyRes, err := f.ConnContext.Server.Client.Do(proxyReq)
	if err != nil {
		LogErr(log, err)
		res.WriteHeader(502)
		return
	}
	defer proxyRes.Body.Close()

	f.Response = &flow.Response{
		StatusCode: proxyRes.StatusCode,
		Header:     proxyRes.Header,
	}

	// trigger addon event Responseheaders
	for _, addon := range proxy.Addons {
		addon.Responseheaders(f)
		if f.Response.Body != nil {
			reply(f.Response, nil)
			return
		}
	}

	// Read response body
	var resBody io.Reader = proxyRes.Body
	if !f.Stream {
		resBuf, r, err := ReaderToBuffer(proxyRes.Body, proxy.Opts.StreamLargeBodies)
		resBody = r
		if err != nil {
			log.Error(err)
			res.WriteHeader(502)
			return
		}
		if resBuf == nil {
			log.Warnf("response body size >= %v\n", proxy.Opts.StreamLargeBodies)
			f.Stream = true
		} else {
			f.Response.Body = resBuf

			// trigger addon event Response
			for _, addon := range proxy.Addons {
				addon.Response(f)
			}
		}
	}

	reply(f.Response, resBody)
}

func (proxy *Proxy) handleConnect(res http.ResponseWriter, req *http.Request) {
	log := log.WithFields(_log.Fields{
		"in":   "Proxy.handleConnect",
		"host": req.Host,
	})

	log.Debug("receive connect")

	conn, err := proxy.Interceptor.Dial(req)
	if err != nil {
		log.Error(err)
		res.WriteHeader(502)
		return
	}
	defer conn.Close()

	cconn, _, err := res.(http.Hijacker).Hijack()
	if err != nil {
		log.Error(err)
		res.WriteHeader(502)
		return
	}

	// cconn.(*net.TCPConn).SetLinger(0) // send RST other than FIN when finished, to avoid TIME_WAIT state
	// cconn.(*net.TCPConn).SetKeepAlive(false)
	defer cconn.Close()

	_, err = io.WriteString(cconn, "HTTP/1.1 200 Connection Established\r\n\r\n")
	if err != nil {
		log.Error(err)
		return
	}

	transfer(log, conn, cconn)
}
