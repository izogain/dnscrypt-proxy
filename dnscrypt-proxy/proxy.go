package main

import (
	"io"
	"io/ioutil"
	"math/rand"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/jedisct1/dlog"
	"github.com/pquerna/cachecontrol/cacheobject"
	"golang.org/x/crypto/curve25519"
)

type Proxy struct {
	proxyPublicKey               [32]byte
	proxySecretKey               [32]byte
	questionSizeEstimator        QuestionSizeEstimator
	serversInfo                  ServersInfo
	timeout                      time.Duration
	certRefreshDelay             time.Duration
	certRefreshDelayAfterFailure time.Duration
	certIgnoreTimestamp          bool
	mainProto                    string
	listenAddresses              []string
	daemonize                    bool
	registeredServers            []RegisteredServer
	pluginBlockIPv6              bool
	cache                        bool
	cacheSize                    int
	cacheNegTTL                  uint32
	cacheMinTTL                  uint32
	cacheMaxTTL                  uint32
	queryLogFile                 string
	queryLogFormat               string
	queryLogIgnoredQtypes        []string
	nxLogFile                    string
	nxLogFormat                  string
	blockNameFile                string
	blockNameLogFile             string
	blockNameFormat              string
	blockIPFile                  string
	blockIPLogFile               string
	blockIPFormat                string
	forwardFile                  string
	cloakFile                    string
	pluginsGlobals               PluginsGlobals
	urlsToPrefetch               []URLToPrefetch
	clientsCount                 uint32
	maxClients                   uint32
	xTransport                   *XTransport
	allWeeklyRanges              *map[string]WeeklyRanges
}

func (proxy *Proxy) StartProxy() {
	proxy.questionSizeEstimator = NewQuestionSizeEstimator()
	if _, err := rand.Read(proxy.proxySecretKey[:]); err != nil {
		dlog.Fatal(err)
	}
	curve25519.ScalarBaseMult(&proxy.proxyPublicKey, &proxy.proxySecretKey)
	for _, registeredServer := range proxy.registeredServers {
		proxy.serversInfo.registerServer(proxy, registeredServer.name, registeredServer.stamp)
	}
	for _, listenAddrStr := range proxy.listenAddresses {
		listenUDPAddr, err := net.ResolveUDPAddr("udp", listenAddrStr)
		if err != nil {
			dlog.Fatal(err)
		}
		listenTCPAddr, err := net.ResolveTCPAddr("tcp", listenAddrStr)
		if err != nil {
			dlog.Fatal(err)
		}
		if err := proxy.udpListenerFromAddr(listenUDPAddr); err != nil {
			dlog.Fatal(err)
		}
		if err := proxy.tcpListenerFromAddr(listenTCPAddr); err != nil {
			dlog.Fatal(err)
		}
	}
	if err := proxy.SystemDListeners(); err != nil {
		dlog.Fatal(err)
	}
	liveServers, err := proxy.serversInfo.refresh(proxy)
	if liveServers > 0 {
		dlog.Noticef("dnscrypt-proxy is ready - live servers: %d", liveServers)
		SystemDNotify()
	} else if err != nil {
		dlog.Error(err)
		dlog.Notice("dnscrypt-proxy is waiting for at least one server to be reachable")
	}
	proxy.prefetcher(&proxy.urlsToPrefetch)
	go func() {
		for {
			delay := proxy.certRefreshDelay
			if proxy.serversInfo.liveServers() == 0 {
				delay = proxy.certRefreshDelayAfterFailure
			}
			time.Sleep(delay)
			proxy.serversInfo.refresh(proxy)
		}
	}()
}

func (proxy *Proxy) prefetcher(urlsToPrefetch *[]URLToPrefetch) {
	go func() {
		for {
			now := time.Now()
			for i := range *urlsToPrefetch {
				urlToPrefetch := &(*urlsToPrefetch)[i]
				if now.After(urlToPrefetch.when) {
					dlog.Debugf("Prefetching [%s]", urlToPrefetch.url)
					if err := PrefetchSourceURL(proxy.xTransport, urlToPrefetch); err != nil {
						dlog.Debugf("Prefetching [%s] failed: %s", urlToPrefetch.url, err)
					} else {
						dlog.Debugf("Prefetching [%s] succeeded. Next refresh scheduled for %v", urlToPrefetch.url, urlToPrefetch.when)
					}
				}
			}
			time.Sleep(60 * time.Second)
		}
	}()
}

func (proxy *Proxy) udpListener(clientPc *net.UDPConn) {
	defer clientPc.Close()
	for {
		buffer := make([]byte, MaxDNSPacketSize-1)
		length, clientAddr, err := clientPc.ReadFrom(buffer)
		if err != nil {
			return
		}
		packet := buffer[:length]
		go func() {
			if !proxy.clientsCountInc() {
				dlog.Warnf("Too many connections (max=%d)", proxy.maxClients)
				return
			}
			defer proxy.clientsCountDec()
			proxy.processIncomingQuery(proxy.serversInfo.getOne(), "udp", proxy.mainProto, packet, &clientAddr, clientPc)
		}()
	}
}

func (proxy *Proxy) udpListenerFromAddr(listenAddr *net.UDPAddr) error {
	clientPc, err := net.ListenUDP("udp", listenAddr)
	if err != nil {
		return err
	}
	dlog.Noticef("Now listening to %v [UDP]", listenAddr)
	go proxy.udpListener(clientPc)
	return nil
}

func (proxy *Proxy) tcpListener(acceptPc *net.TCPListener) {
	defer acceptPc.Close()
	for {
		clientPc, err := acceptPc.Accept()
		if err != nil {
			continue
		}
		go func() {
			defer clientPc.Close()
			if !proxy.clientsCountInc() {
				dlog.Warnf("Too many connections (max=%d)", proxy.maxClients)
				return
			}
			defer proxy.clientsCountDec()
			clientPc.SetDeadline(time.Now().Add(proxy.timeout))
			packet, err := ReadPrefixed(clientPc.(*net.TCPConn))
			if err != nil || len(packet) < MinDNSPacketSize {
				return
			}
			clientAddr := clientPc.RemoteAddr()
			proxy.processIncomingQuery(proxy.serversInfo.getOne(), "tcp", "tcp", packet, &clientAddr, clientPc)
		}()
	}
}

func (proxy *Proxy) tcpListenerFromAddr(listenAddr *net.TCPAddr) error {
	acceptPc, err := net.ListenTCP("tcp", listenAddr)
	if err != nil {
		return err
	}
	dlog.Noticef("Now listening to %v [TCP]", listenAddr)
	go proxy.tcpListener(acceptPc)
	return nil
}

func (proxy *Proxy) exchangeWithUDPServer(serverInfo *ServerInfo, encryptedQuery []byte, clientNonce []byte) ([]byte, error) {
	pc, err := net.DialUDP("udp", nil, serverInfo.UDPAddr)
	if err != nil {
		return nil, err
	}
	pc.SetDeadline(time.Now().Add(serverInfo.Timeout))
	pc.Write(encryptedQuery)
	encryptedResponse := make([]byte, MaxDNSPacketSize)
	length, err := pc.Read(encryptedResponse)
	pc.Close()
	if err != nil {
		return nil, err
	}
	encryptedResponse = encryptedResponse[:length]
	return proxy.Decrypt(serverInfo, encryptedResponse, clientNonce)
}

func (proxy *Proxy) exchangeWithTCPServer(serverInfo *ServerInfo, encryptedQuery []byte, clientNonce []byte) ([]byte, error) {
	pc, err := net.DialTCP("tcp", nil, serverInfo.TCPAddr)
	if err != nil {
		return nil, err
	}
	pc.SetDeadline(time.Now().Add(serverInfo.Timeout))
	encryptedQuery, err = PrefixWithSize(encryptedQuery)
	if err != nil {
		return nil, err
	}
	pc.Write(encryptedQuery)

	encryptedResponse, err := ReadPrefixed(pc)
	pc.Close()
	if err != nil {
		return nil, err
	}
	return proxy.Decrypt(serverInfo, encryptedResponse, clientNonce)
}

func (proxy *Proxy) clientsCountInc() bool {
	for {
		count := proxy.clientsCount
		if count >= proxy.maxClients {
			return false
		}
		if atomic.CompareAndSwapUint32(&proxy.clientsCount, count, count+1) {
			dlog.Debugf("clients count: %d", count+1)
			return true
		}
	}
}

func (proxy *Proxy) clientsCountDec() {
	for {
		if count := proxy.clientsCount; count == 0 || atomic.CompareAndSwapUint32(&proxy.clientsCount, count, count-1) {
			break
		}
	}
}

func (proxy *Proxy) processIncomingQuery(serverInfo *ServerInfo, clientProto string, serverProto string, query []byte, clientAddr *net.Addr, clientPc net.Conn) {
	if len(query) < MinDNSPacketSize || serverInfo == nil {
		return
	}
	pluginsState := NewPluginsState(proxy, clientProto, clientAddr)
	query, _ = pluginsState.ApplyQueryPlugins(&proxy.pluginsGlobals, query)
	var response []byte
	var err error
	if pluginsState.action != PluginsActionForward {
		if pluginsState.synthResponse != nil {
			response, err = pluginsState.synthResponse.PackBuffer(response)
			if err != nil {
				return
			}
		}
		if pluginsState.action == PluginsActionDrop {
			return
		}
	}
	if len(response) == 0 {
		var ttl *uint32
		if serverInfo.Proto == StampProtoTypeDNSCrypt {
			encryptedQuery, clientNonce, err := proxy.Encrypt(serverInfo, query, serverProto)
			if err != nil {
				return
			}
			serverInfo.noticeBegin(proxy)
			if serverProto == "udp" {
				response, err = proxy.exchangeWithUDPServer(serverInfo, encryptedQuery, clientNonce)
			} else {
				response, err = proxy.exchangeWithTCPServer(serverInfo, encryptedQuery, clientNonce)
			}
			if err != nil {
				serverInfo.noticeFailure(proxy)
				return
			}
		} else if serverInfo.Proto == StampProtoTypeDoH {
			tid := TransactionID(query)
			SetTransactionID(query, 0)
			resp, _, err := proxy.xTransport.DoHQuery(serverInfo.useGet, serverInfo.URL, query, proxy.timeout)
			SetTransactionID(query, tid)
			if err != nil {
				return
			}
			response, err = ioutil.ReadAll(io.LimitReader(resp.Body, int64(MaxDNSPacketSize)))
			if err != nil {
				return
			}
			if len(response) >= MinDNSPacketSize {
				SetTransactionID(response, tid)
			}
			ttl = ttlFromHTTPResponse(proxy, resp)
		} else {
			dlog.Fatal("Unsupported protocol")
		}
		if err != nil {
			serverInfo.noticeFailure(proxy)
			return
		}
		response, _ = pluginsState.ApplyResponsePlugins(&proxy.pluginsGlobals, response, ttl)
	}
	if len(response) < MinDNSPacketSize || len(response) > MaxDNSPacketSize {
		serverInfo.noticeFailure(proxy)
		return
	}
	if clientProto == "udp" {
		if len(response) > MaxDNSUDPPacketSize {
			response, err = TruncatedResponse(response)
			if err != nil {
				return
			}
		}
		clientPc.(net.PacketConn).WriteTo(response, *clientAddr)
		if HasTCFlag(response) {
			proxy.questionSizeEstimator.blindAdjust()
		} else {
			proxy.questionSizeEstimator.adjust(ResponseOverhead + len(response))
		}
	} else {
		response, err = PrefixWithSize(response)
		if err != nil {
			serverInfo.noticeFailure(proxy)
			return
		}
		clientPc.Write(response)
	}
	serverInfo.noticeSuccess(proxy)
}

func ttlFromHTTPResponse(proxy *Proxy, resp *http.Response) *uint32 {
	cacheControlStr := resp.Header.Get("Cache-Control")
	if len(cacheControlStr) == 0 {
		return nil
	}
	cacheControl, err := cacheobject.ParseResponseCacheControl(cacheControlStr)
	if err != nil {
		return nil
	}
	foundTTL := uint32(cacheControl.MaxAge)
	if foundTTL < proxy.cacheMinTTL {
		foundTTL = proxy.cacheMinTTL
	}
	if foundTTL > proxy.cacheMaxTTL {
		foundTTL = proxy.cacheMaxTTL
	}
	return &foundTTL
}

func NewProxy() Proxy {
	return Proxy{
		serversInfo: ServersInfo{lbStrategy: DefaultLBStrategy},
	}
}
