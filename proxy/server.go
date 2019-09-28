package proxy

import (
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/alphabetY/common/logg"
	"github.com/alphabetY/common/lru"
	"github.com/alphabetY/tcpmux"
	"github.com/xtaci/kcp-go"
)

type KCPConfig struct {
	Enable bool
}

type ServerConfig struct {
	Throttling    int64
	ThrottlingMax int64
	LBindTimeout  int64
	LBindCap      int64
	DisableUDP    bool
	DisableLRP    bool
	HTTPS         *tls.Config
	ProxyPassAddr string
	Logger        *logg.Logger
	KCP           KCPConfig

	Users map[string]UserConfig

	*Cipher
}

// UserConfig is for multi-users server, not implemented yet
type UserConfig struct {
	Auth          string
	Throttling    int64
	ThrottlingMax int64
}

type localRPCtrlSrvReq struct {
	dst      string
	end      bool
	conn     net.Conn
	callback chan localRPCtrlSrvResp
	rawReq   []byte
}

type localRPCtrlSrvResp struct {
	err      error
	localrpr string
	req      localRPCtrlSrvReq
}

// ProxyServer is the main struct for upstream server
type ProxyServer struct {
	tp        *http.Transport
	rp        http.Handler
	blacklist *lru.Cache

	localRP struct {
		sync.Mutex
		downstreams []net.Conn
		downConns   []DummyConnWrapper
		requests    chan localRPCtrlSrvReq
		waiting     map[string]localRPCtrlSrvResp
	}

	Localaddr string
	Listener  net.Listener

	*ServerConfig
}

func (proxy *ProxyServer) auth(auth string) bool {
	if _, existed := proxy.Users[auth]; existed {
		// we don't have multi-user mode currently
		return true
	}

	return false
}

func (proxy *ProxyServer) getIOConfig(auth string) IOConfig {
	var ioc IOConfig
	if proxy.Throttling > 0 {
		ioc.Bucket = NewTokenBucket(proxy.Throttling, proxy.ThrottlingMax)
	}
	return ioc
}

func (proxy *ProxyServer) Write(w http.ResponseWriter, key *[ivLen]byte, p []byte, code int) (n int, err error) {
	if ctr := proxy.Cipher.getCipherStream(key); ctr != nil {
		ctr.XORKeyStream(p, p)
	}

	w.WriteHeader(code)
	return w.Write(p)
}

func (proxy *ProxyServer) hijack(w http.ResponseWriter) net.Conn {
	hij, ok := w.(http.Hijacker)
	if !ok {
		proxy.Logger.E("Server", "Hijack", "Not supported")
		return nil
	}

	conn, _, err := hij.Hijack()
	if err != nil {
		proxy.Logger.E("Server", "Hijack", err.Error())
		return nil
	}

	return conn
}

func (proxy *ProxyServer) replyGood(downstreamConn net.Conn, cr *clientRequest, ioc *IOConfig, r *http.Request) {
	var p buffer
	if cr.Opt.IsSet(doWebSocket) {
		ioc.WSCtrl = wsServer

		var accept buffer
		accept.Writes(r.Header.Get("Sec-WebSocket-Key"), "258EAFA5-E914-47DA-95CA-C5AB0DC85B11")

		ans := sha1.Sum(accept.Bytes())
		p.Writes("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: upgrade\r\nSec-WebSocket-Accept: ",
			base64.StdEncoding.EncodeToString(ans[:]), "\r\n\r\n")
	} else {
		p.Writes("HTTP/1.1 200 OK\r\nContent-Type: application/octet-stream\r\nDate: ", time.Now().UTC().Format(time.RFC1123), "\r\n\r\n")
	}

	downstreamConn.Write(p.Bytes())
}

func (proxy *ProxyServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	replySomething := func() {
		if proxy.rp == nil {
			w.WriteHeader(404)
			w.Write([]byte(`<html>
<head><title>404 Not Found</title></head>
<body bgcolor="white">
<center><h1>404 Not Found</h1></center>
<hr><center>nginx</center>
</body>
</html>`))
		} else {
			proxy.rp.ServeHTTP(w, r)
		}
	}

	addr, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		proxy.Logger.W("Server", "Unknown address", r.RemoteAddr)
		replySomething()
		return
	}

	var rawReq []byte
	if proxy.localRP.waiting != nil {
		rawReq, _ = httputil.DumpRequest(r, true)
	}

	dst, cr := proxy.decryptHost(proxy.stripURI(r.RequestURI))

	if dst == "" || cr == nil {
		if proxy.localRP.waiting != nil {
			userConn := proxy.hijack(w)
			cb := make(chan localRPCtrlSrvResp, 1)

			proxy.localRP.requests <- localRPCtrlSrvReq{
				dst:      dst,
				conn:     userConn,
				callback: cb,
				rawReq:   rawReq,
			}

			proxy.Logger.D("Local RP", "Client", "Wait client to response")
			select {
			case resp := <-cb:
				if resp.err != nil {
					userConn.Write([]byte("HTTP/1.1 400 Bad Request\r\n\r\nError: " + resp.err.Error()))
					return
				}
			case <-time.After(time.Duration(proxy.LBindTimeout) * time.Second):
				proxy.Logger.E("Local RP", "Client", "client didn't response")
				userConn.Write([]byte("HTTP/1.1 502 Bad Gateway\r\n\r\nError: localrp timed out"))
			}
			proxy.Logger.D("Local RP", "Client", "OK")

			proxy.localRP.Lock()
			for k, resp := range proxy.localRP.waiting {
				if resp.req.conn == userConn {
					delete(proxy.localRP.waiting, k)
					break
				}
			}
			proxy.localRP.Unlock()
			return
		}
		proxy.Logger.D("Server", "Invalid request", addr, proxy.stripURI(r.RequestURI))
		proxy.blacklist.Add(addr, nil)
		replySomething()
		return
	}

	if proxy.Users != nil {
		if !proxy.auth(cr.Auth) {
			proxy.Logger.W("Server", "User auth failed", addr)
			return
		}
	}

	if h, _, _ := proxy.blacklist.GetEx(addr); h > invalidRequestRetry {
		proxy.Logger.D("Server", "Repeated access using invalid key", addr)
		// replySomething()
		// return
	}

	if cr.Opt.IsSet(doDNS) {
		host := cr.Query
		ip, err := net.ResolveIPAddr("ip4", host)
		if err != nil {
			proxy.Logger.W("Dial", "Error", err)
			ip = &net.IPAddr{IP: net.IP{127, 0, 0, 1}}
		}

		proxy.Logger.D("Server", "DNS query", host, ip.String())
		w.Header().Add(dnsRespHeader, base64.StdEncoding.EncodeToString([]byte(ip.IP.To4())))
		w.WriteHeader(200)
	} else if cr.Opt.IsSet(doLocalRP) {
		ioc := proxy.getIOConfig(cr.Auth)
		ioc.Partial = cr.Opt.IsSet(doPartial)

		if dst == "localrp" {
			proxy.startLocalRPControlServer(proxy.hijack(w), cr, ioc)
		} else if proxy.localRP.waiting != nil {
			proxy.localRP.Lock()
			resp, ok := proxy.localRP.waiting[dst]
			if !ok {
				proxy.localRP.Unlock()
				return
			}
			proxy.localRP.Unlock()

			downstreamConn := proxy.hijack(w)
			proxy.replyGood(downstreamConn, cr, &ioc, r)
			go proxy.Cipher.IO.Bridge(downstreamConn, resp.req.conn, &cr.iv, ioc)
			resp.req.callback <- resp
			return
		}
	} else if cr.Opt.IsSet(doConnect) {
		host := dst
		if host == "" {
			proxy.Logger.W("Server", "Valid rkey invalid host", addr)
			replySomething()
			return
		}

		proxy.Logger.D("Dial", "Host", host)
		downstreamConn := proxy.hijack(w)
		if downstreamConn == nil {
			return
		}

		ioc := proxy.getIOConfig(cr.Auth)
		ioc.Partial = cr.Opt.IsSet(doPartial)

		var targetSiteConn net.Conn
		var err error

		if cr.Opt.IsSet(doUDPRelay) {
			if proxy.DisableUDP {
				proxy.Logger.W("Server", "UDP disabled")
				downstreamConn.Close()
				return
			}

			uaddr, _ := net.ResolveUDPAddr("udp", host)

			var rconn *net.UDPConn
			rconn, err = net.DialUDP("udp", nil, uaddr)
			targetSiteConn = &udpBridgeConn{
				UDPConn: rconn,
				udpSrc:  uaddr,
				logger:  proxy.Logger,
			}
			// rconn.Write([]byte{6, 7, 0, 0, 0, 1, 0, 0, 0, 0, 0, 0, 5, 98, 97, 105, 100, 117, 3, 99, 111, 109, 0, 0, 1, 0, 1})
		} else {
			targetSiteConn, err = net.Dial("tcp", host)
		}

		if err != nil {
			proxy.Logger.E("Dial", "Error", err)
			downstreamConn.Close()
			return
		}

		proxy.replyGood(downstreamConn, cr, &ioc, r)

		if cr.Opt.IsSet(doMuxWS) {
			proxy.Listener.(*tcpmux.ListenPool).Upgrade(downstreamConn)
			proxy.Logger.D("Server", "Downstream connection has been upgraded to multiplexer stream")
		} else {
			go proxy.Cipher.IO.Bridge(downstreamConn, targetSiteConn, &cr.iv, ioc)
		}
	} else if cr.Opt.IsSet(doForward) {
		var err error

		r.URL, err = url.Parse(dst)
		if err != nil {
			replySomething()
			return
		}

		r.Host = r.URL.Host
		proxy.decryptRequest(r, cr)

		proxy.Logger.D("Server", r.Method, r.URL.String())

		resp, err := proxy.tp.RoundTrip(r)
		if err != nil {
			proxy.Logger.E("Server", "HTTP forward", r.URL, err)
			proxy.Write(w, &cr.iv, []byte(err.Error()), http.StatusInternalServerError)
			return
		}

		if resp.StatusCode >= 400 {
			proxy.Logger.D("Server", "HTTP forward", resp.Status, r.URL)
		}

		copyHeaders(w.Header(), resp.Header, proxy.Cipher, true, &cr.iv)
		w.WriteHeader(resp.StatusCode)

		if nr, err := proxy.Cipher.IO.Copy(w, resp.Body, &cr.iv, proxy.getIOConfig(cr.Auth)); err != nil {
			proxy.Logger.E("Server", "Copy bytes", nr, err)
		}

		tryClose(resp.Body)
	} else {
		proxy.blacklist.Add(addr, nil)
		replySomething()
	}
}

func (proxy *ProxyServer) Start() (err error) {
	switch {
	case proxy.KCP.Enable:
		ln, err := kcp.Listen(proxy.Localaddr)
		if err != nil {
			return err
		}
		proxy.Listener = tcpmux.Wrap(ln)
	case proxy.HTTPS != nil:
		ln, err := tls.Listen("tcp", proxy.Localaddr, proxy.HTTPS)
		if err != nil {
			return err
		}
		proxy.Listener = tcpmux.Wrap(ln)
	default:
		proxy.Listener, err = tcpmux.Listen(proxy.Localaddr, true)
		if err != nil {
			return
		}
	}
	proxy.Cipher.IO.Ob = proxy.Listener.(*tcpmux.ListenPool)

	if proxy.Logger.GetLevel() == logg.LvDebug {
		go func() {
			for range time.Tick(time.Minute) {
				proxy.localRP.Lock()
				if proxy.localRP.waiting != nil {
					proxy.Logger.D("Local RP", "Queue", len(proxy.localRP.waiting), len(proxy.localRP.downConns))
				}
				proxy.localRP.Unlock()
			}
		}()
	}

	return http.Serve(proxy.Listener, proxy)
}

func NewServer(addr string, config *ServerConfig) *ProxyServer {
	proxy := &ProxyServer{
		tp: &http.Transport{TLSClientConfig: tlsSkip},

		ServerConfig: config,
		blacklist:    lru.NewCache(128),
	}

	// tcpmux.HashSeed = config.Cipher.keyBuf

	if config.ProxyPassAddr != "" {
		if strings.HasPrefix(config.ProxyPassAddr, "http") {
			u, err := url.Parse(config.ProxyPassAddr)
			if err != nil {
				proxy.Logger.F("Server", "Error", err)
				return nil
			}

			proxy.rp = httputil.NewSingleHostReverseProxy(u)
		} else {
			proxy.rp = http.FileServer(http.Dir(config.ProxyPassAddr))
		}
	}

	if port, lerr := strconv.Atoi(addr); lerr == nil {
		addr = (&net.TCPAddr{IP: net.IPv4zero, Port: port}).String()
	}

	proxy.Localaddr = addr
	return proxy
}

func (proxy *ProxyServer) pickAControlConn() DummyConnWrapper {
	proxy.localRP.Lock()
	defer proxy.localRP.Unlock()
	return proxy.localRP.downConns[proxy.Cipher.Rand.Intn(len(proxy.localRP.downConns))]
}

func (proxy *ProxyServer) startLocalRPControlServer(downstream net.Conn, cr *clientRequest, ioc IOConfig) {
	if _, err := downstream.Write([]byte("HTTP/1.1 200 OK\r\n\r\n")); err != nil {
		proxy.Logger.E("Local RP", "Error", err)
		downstream.Close()
		return
	}

	if proxy.DisableLRP {
		proxy.Logger.W("Local RP", "Reject client request")
		downstream.Close()
		return
	}

	proxy.localRP.Lock()
	if proxy.localRP.downstreams == nil {
		proxy.localRP.downstreams = make([]net.Conn, 0)
		proxy.localRP.downConns = make([]DummyConnWrapper, 0)
	}
	if proxy.localRP.requests == nil {
		proxy.localRP.requests = make(chan localRPCtrlSrvReq, proxy.LBindCap)
	}
	if proxy.localRP.waiting == nil {
		proxy.localRP.waiting = make(map[string]localRPCtrlSrvResp)
	}

	conn := &DummyConn{}
	conn.Init()
	connw := DummyConnWrapper{conn}

	proxy.localRP.downstreams = append(proxy.localRP.downstreams, downstream)
	proxy.localRP.downConns = append(proxy.localRP.downConns, connw)

	if len(proxy.localRP.downConns) == 1 {
		go func() {
			for {
				select {
				case req := <-proxy.localRP.requests:
					if req.end {
						proxy.Logger.D("Local RP", "Request listening loop has ended")
						return
					}
					if len(req.dst) >= 65535 {
						req.callback <- localRPCtrlSrvResp{
							err: fmt.Errorf("request too long"),
						}
						continue
					}

					buf := make([]byte, 16+4+len(req.rawReq))
					proxy.Cipher.Rand.Read(buf[:16])

					localrpr := fmt.Sprintf("%x", buf[:16])
					binary.BigEndian.PutUint32(buf[16:20], uint32(len(req.rawReq)))
					copy(buf[20:], req.rawReq)

					proxy.localRP.Lock()
					proxy.localRP.waiting[localrpr] = localRPCtrlSrvResp{
						localrpr: localrpr,
						req:      req,
					}
					proxy.localRP.Unlock()

					connw := proxy.pickAControlConn()
					go connw.Write(buf)
				}
			}
		}()
	}

	proxy.localRP.Unlock()

	go proxy.Cipher.IO.Bridge(downstream, conn, &cr.iv, ioc)

	go func() {

		for {
			buf := make([]byte, 16)
			if _, err := connw.Write(buf); err != nil {
				break
			}
			if _, err := connw.Read(buf); err != nil {
				break
			}
			// proxy.Logger.D("Server","LocalRP: pong")
			time.Sleep(localRPPingInterval)
		}

		proxy.localRP.Lock()
		for i, d := range proxy.localRP.downConns {
			if d == connw {
				proxy.localRP.downstreams = append(proxy.localRP.downstreams[:i], proxy.localRP.downstreams[i+1:]...)
				proxy.localRP.downConns = append(proxy.localRP.downConns[:i], proxy.localRP.downConns[i+1:]...)
				break
			}
		}
		if len(proxy.localRP.downstreams) == 0 {
			proxy.localRP.requests <- localRPCtrlSrvReq{end: true}
			proxy.localRP.waiting = nil
			proxy.localRP.requests = nil
		}
		proxy.localRP.Unlock()
		downstream.Close()
	}()
}
