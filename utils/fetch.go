package utils

// DAS utils module
//
// Copyright (c) 2015-2016 - Valentin Kuznetsov <vkuznet AT gmail dot com>
//
// Some links: http://www.alexedwards.net/blog/golang-response-snippets
// http://blog.golang.org/json-and-go
// http://golang.org/pkg/html/template/
// https://labix.org/mgo

import (
	"bytes"
	"container/heap"
	"crypto/tls"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/http/httputil"
	"os"
	"os/user"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vkuznet/dcr"
	"github.com/vkuznet/x509proxy"
)

// TIMEOUT defines timeout for net/url request
var TIMEOUT int

// TotalGetCalls counts total number of GET requests made by the server
var TotalGetCalls uint64

// TotalPostCalls counts total number of POST requests made by the server
var TotalPostCalls uint64

// CLIENT_VERSION represents client version
var CLIENT_VERSION string

// DNSCacheMgr manager
var DNSCacheMgr *dcr.DNSManager

// UseDNSCache defines if we use DNS Cache resolver
var UseDNSCache bool

// TLSCertsRenewInterval controls interval to re-read TLS certs (in seconds)
var TLSCertsRenewInterval time.Duration

// TLSCerts holds TLS certificates for the server
type TLSCertsManager struct {
	Certs  []tls.Certificate
	Expire time.Time
}

// GetCerts return fresh copy of certificates
func (t *TLSCertsManager) GetCerts() ([]tls.Certificate, error) {
	var lock = sync.Mutex{}
	lock.Lock()
	defer lock.Unlock()
	// we'll use existing certs if our window is not expired
	if t.Certs == nil || time.Since(t.Expire) > TLSCertsRenewInterval {
		t.Expire = time.Now()
		if WEBSERVER > 0 {
			log.Println("read new certs, expire", t.Expire, "config.TLSCertsRenewInterval", TLSCertsRenewInterval)
		}
		certs, err := tlsCerts()
		if err == nil {
			t.Certs = certs
		} else {
			panic(err.Error())
		}
	}
	return t.Certs, nil
}

// global TLSCerts manager
var tlsManager TLSCertsManager

// client X509 certificates
func tlsCerts() ([]tls.Certificate, error) {
	uproxy := os.Getenv("X509_USER_PROXY")
	uckey := os.Getenv("X509_USER_KEY")
	ucert := os.Getenv("X509_USER_CERT")

	// check if /tmp/x509up_u$UID exists, if so setup X509_USER_PROXY env
	u, err := user.Current()
	if err == nil {
		fname := fmt.Sprintf("/tmp/x509up_u%s", u.Uid)
		if _, err := os.Stat(fname); err == nil {
			uproxy = fname
		}
	}
	if WEBSERVER == 1 && VERBOSE > 0 {
		log.Printf("tls certs, X509_USER_PROXY=%v, X509_USER_KEY=%v, X509_USER_CERT=%v\n", uproxy, uckey, ucert)
	}

	if uproxy == "" && uckey == "" { // user doesn't have neither proxy or user certs
		return nil, nil
	}
	if uproxy != "" {
		// use local implementation of LoadX409KeyPair instead of tls one
		x509cert, err := x509proxy.LoadX509Proxy(uproxy)
		if err != nil {
			return nil, fmt.Errorf("failed to parse X509 proxy: %v", err)
		}
		if WEBSERVER == 1 && VERBOSE > 0 {
			log.Println("use proxy", uproxy)
		}
		certs := []tls.Certificate{x509cert}
		return certs, nil
	}
	x509cert, err := tls.LoadX509KeyPair(ucert, uckey)
	if err != nil {
		return nil, fmt.Errorf("failed to parse user X509 certificate: %v", err)
	}
	if WEBSERVER == 1 && VERBOSE > 0 {
		log.Println("user key", uckey, "cert", ucert)
	}
	certs := []tls.Certificate{x509cert}
	return certs, nil
}

// HttpClient is HTTP client for urlfetch server
func HttpClient() *http.Client {
	// get X509 certs
	//     certs, err := tlsCerts()
	certs, err := tlsManager.GetCerts()
	if err != nil {
		panic(err.Error())
	}
	timeout := time.Duration(TIMEOUT) * time.Second
	if len(certs) == 0 {
		if TIMEOUT > 0 {
			return &http.Client{Timeout: time.Duration(timeout)}
		}
		return &http.Client{}
	}
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{Certificates: certs,
			InsecureSkipVerify: true},
	}
	if TIMEOUT > 0 {
		return &http.Client{Transport: tr, Timeout: timeout}
	}
	return &http.Client{Transport: tr}
}

// ResponseType structure is what we expect to get for our URL call.
// It contains a request URL, the data chunk and possible error from remote
type ResponseType struct {
	Url   string
	Data  []byte
	Error error
}

// UrlRequest structure holds details about url request's attributes
type UrlRequest struct {
	rurl string
	args string
	out  chan<- ResponseType
	ts   int64
}

// A UrlFetchQueue implements heap.Interface and holds UrlRequests
type UrlFetchQueue []*UrlRequest

// Len provides len implemenation for UrlFetchQueue
func (q UrlFetchQueue) Len() int { return len(q) }

// Less provides Less implemenation for UrlFetchQueue
func (q UrlFetchQueue) Less(i, j int) bool { return q[i].ts < q[j].ts }

// Swap provides swap implemenation for UrlFetchQueue
func (q UrlFetchQueue) Swap(i, j int) { q[i], q[j] = q[j], q[i] }

// Push provides push implemenation for UrlFetchQueue
func (q *UrlFetchQueue) Push(x interface{}) {
	item := x.(*UrlRequest)
	*q = append(*q, item)
}

// Pop provides Pop implemenation for UrlFetchQueue
func (q *UrlFetchQueue) Pop() interface{} {
	old := *q
	n := len(old)
	item := old[n-1]
	*q = old[0 : n-1]
	return item
}

var (
	// UrlQueueSize keeps track of running URL requests
	UrlQueueSize int32
	// UrlQueueLimit knows how many URL requests we can handle at a time, 0 means no limit
	UrlQueueLimit int32
	// UrlRetry knows  how many times we'll retry given url call
	UrlRetry int
	// UrlRequestChannel is a UrlRequest channel
	UrlRequestChannel = make(chan UrlRequest)
)

func init() {
	if WEBSERVER > 0 {
		log.Println("DAS URLFetchWorker")
	}
	go URLFetchWorker(UrlRequestChannel)
}

// URLFetchWorker has three channels: in channel for incoming requests
// (in a form of URL strings), out channel for outgoing responses in a form of
// ResponseType structure and quit channel
func URLFetchWorker(in <-chan UrlRequest) {
	urlRequests := &UrlFetchQueue{}
	heap.Init(urlRequests)
	// loop forever to accept url requests
	// a given request will be placed in internal Queue and we'll process it
	// only in a limited queueSize. Every request is processed via fetch
	// function which will decrement queueSize once it's done with request.
	for {
		select {
		case request := <-in:
			// put new request to urlRequests queue and increment queueSize
			heap.Push(urlRequests, &request)
		default:
			if urlRequests.Len() > 0 && UrlQueueSize < UrlQueueLimit {
				r := heap.Pop(urlRequests)
				request := r.(*UrlRequest)
				go fetch(request.rurl, request.args, request.out)
			}
			time.Sleep(time.Duration(10) * time.Millisecond)
		}
	}
}

// Problem with too many open files
// http://craigwickesser.com/2015/01/golang-http-to-many-open-files/

// FetchResponse fetches data for provided URL, args is a json dump of arguments
func FetchResponse(rurl, args string) ResponseType {
	startTime := time.Now()
	// increment UrlQueueSize since we'll process request
	atomic.AddInt32(&UrlQueueSize, 1)
	defer atomic.AddInt32(&UrlQueueSize, -1) // decrement UrlQueueSize since we done with this request
	if VERBOSE > 1 {
		log.Printf("http request, UrlQueueSize %v, UrlQueueLimit %v\n", UrlQueueSize, UrlQueueLimit)
	}
	var response ResponseType
	response.Url = rurl
	if validateUrl(rurl) == false {
		response.Error = errors.New("Invalid URL")
		return response
	}
	if UseDNSCache {
		if DNSCacheMgr == nil {
			DNSCacheMgr = dcr.NewDNSManager(300) // 300 seconds TTL
			log.Printf("init DNSCacheMgr %+v\n", DNSCacheMgr)
		}
		if strings.Contains(rurl, "cmsweb") {
			rurl = DNSCacheMgr.Resolve(rurl)
		}
	}
	var req *http.Request
	if len(args) > 0 {
		jsonStr := []byte(args)
		req, _ = http.NewRequest("POST", rurl, bytes.NewBuffer(jsonStr))
		req.Header.Set("Content-Type", "application/json")
		atomic.AddUint64(&TotalPostCalls, 1)
	} else {
		req, _ = http.NewRequest("GET", rurl, nil)
		req.Header.Add("Accept-Encoding", "identity")
		if strings.Contains(rurl, "sitedb") || strings.Contains(rurl, "reqmgr") || strings.Contains(rurl, "mcm") {
			req.Header.Add("Accept", "application/json")
		}
		if strings.Contains(rurl, "rucio") { // we need to fetch auth token
			token, err := RucioAuth.Token()
			if err == nil {
				req.Header.Add("X-Rucio-Auth-Token", token)
			}
			req.Header.Add("Accept", "application/x-json-stream")
			req.Header.Add("Connection", "Keep-Alive")
			if WEBSERVER > 0 {
				req.Header.Add("X-Rucio-Account", RucioAuth.Account())
			}
		}
		atomic.AddUint64(&TotalGetCalls, 1)
	}
	if CLIENT_VERSION != "" {
		req.Header.Set("User-Agent", fmt.Sprintf("dasgoclient/%s", CLIENT_VERSION))
	} else {
		req.Header.Set("User-Agent", "dasgoserver")
	}
	if VERBOSE > 2 {
		dump, err := httputil.DumpRequestOut(req, true)
		log.Printf("http request %+v, rurl %v, dump %v, error %v\n", req, rurl, string(dump), err)
	}
	client := HttpClient()
	resp, err := client.Do(req)
	if VERBOSE > 2 {
		if resp != nil {
			dump, err := httputil.DumpResponse(resp, true)
			log.Printf("http response rurl %v, dump %v, error %v\n", rurl, string(dump), err)
		}
	}
	if err != nil {
		response.Error = err
		return response
	}
	defer resp.Body.Close()
	response.Data, err = ioutil.ReadAll(resp.Body)
	if err != nil {
		response.Error = err
	}
	if VERBOSE > 0 {
		if args == "" {
			if WEBSERVER == 0 {
				fmt.Println(Color(CYAN, "DAS GET"), ColorUrl(rurl), time.Now().Sub(startTime))
			} else {
				log.Printf("DAS GET %s %v\n", rurl, time.Now().Sub(startTime))
			}
		} else {
			if WEBSERVER == 0 {
				a := fmt.Sprintf("args=%s", args)
				fmt.Println(Color(PURPLE, "DAS POST"), ColorUrl(rurl), a, time.Now().Sub(startTime))
			} else {
				log.Printf("DAS POST %s args %v, %v\n", rurl, args, time.Now().Sub(startTime))
			}
		}
	}
	return response
}

// Fetch data for provided URL and redirect results to given channel
// This wrapper function look-up UrlQueueLimit and either redirect to
// URULFetchWorker go-routine or pass the call to local fetch function
func Fetch(rurl string, args string, out chan<- ResponseType) {
	if UrlQueueLimit > 0 {
		request := UrlRequest{rurl: rurl, args: args, out: out, ts: time.Now().Unix()}
		UrlRequestChannel <- request
	} else {
		fetch(rurl, args, out)
	}
}

// local function which fetch response for given url/args and place it into response channel
// By defat
func fetch(rurl string, args string, ch chan<- ResponseType) {
	var resp, r ResponseType
	resp = FetchResponse(rurl, args)
	if resp.Error != nil {
		if WEBSERVER == 1 && VERBOSE > 0 {
			log.Printf("fail to fetch data %s, error %v\n", rurl, resp.Error)
		}
		for i := 1; i <= UrlRetry; i++ {
			sleep := time.Duration(i) * time.Second
			time.Sleep(sleep)
			r = FetchResponse(rurl, args)
			if r.Error == nil {
				break
			}
			if WEBSERVER == 1 && VERBOSE > 0 {
				log.Printf("fetch retry, url %s, retry %v, error %v\n", rurl, i, resp.Error)
			}
		}
		resp = r
	}
	if resp.Error != nil {
		if WEBSERVER == 1 && VERBOSE > 0 {
			log.Printf("ERROR: fail to fetch %s, retries %v, error %v\n", rurl, UrlRetry, resp.Error)
		}
	}
	ch <- resp
}

// Helper function which validates given URL
func validateUrl(rurl string) bool {
	if len(rurl) > 0 {
		if PatternUrl.MatchString(rurl) {
			return true
		}
		log.Println("ERROR, invalid URL", rurl)
	}
	return false
}

// Response represents final response in a form of JSON structure
// we use custorm representation
func Response(rurl string, data []byte) []byte {
	b := []byte(`{"url":`)
	u := []byte(rurl)
	c := []byte(",")
	d := []byte(`"data":`)
	e := []byte(`}`)
	a := [][]byte{b, u, c, d, data, e}
	s := []byte(" ")
	r := bytes.Join(a, s)
	return r

}
