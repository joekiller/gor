package main

import (
	"bufio"
	"bytes"
	es "github.com/buger/gor/elasticsearch"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type RedirectNotAllowed struct{}

func (e *RedirectNotAllowed) Error() string {
	return "Redirects not allowed"
}

// customCheckRedirect disables redirects https://github.com/buger/gor/pull/15
func customCheckRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 0 {
		return new(RedirectNotAllowed)
	}
	return nil
}

// ParseRequest in []byte returns a http request or an error
func ParseRequest(data []byte) (request *http.Request, err error) {
	buf := bytes.NewBuffer(data)
	reader := bufio.NewReader(buf)

	request, err = http.ReadRequest(reader)

	return
}

type HTTPOutput struct {
	address string
	limit   int
	buf     chan []byte
	need_worker chan int

	headers HTTPHeaders
	methods HTTPMethods

	elasticSearch *es.ESPlugin

	bufStats *GorStat
}

func NewHTTPOutput(options string, headers HTTPHeaders, methods HTTPMethods, elasticSearchAddr string) io.Writer {
	o := new(HTTPOutput)

	optionsArr := strings.Split(options, "|")
	address := optionsArr[0]

	if !strings.HasPrefix(address, "http") {
		address = "http://" + address
	}

	o.address = address
	o.headers = headers
	o.methods = methods

	o.buf = make(chan []byte, 100)
	o.bufStats = NewGorStat("output_http")
	o.need_worker = make(chan int)

	if elasticSearchAddr != "" {
		o.elasticSearch = new(es.ESPlugin)
		o.elasticSearch.Init(elasticSearchAddr)
	}

	if len(optionsArr) > 1 {
		o.limit, _ = strconv.Atoi(optionsArr[1])
	}

	go o.worker_master(10)

	if o.limit > 0 {
		return NewLimiter(o, o.limit)
	} else {
		return o
	}
}

func (o *HTTPOutput) worker_master(n int) {
	for i := 0; i < n; i++ {
		go o.worker()
	}

	for {
		new_workers := <- o.need_worker
		for i := 0; i < new_workers; i++ {
			go o.worker()
		}
	}
}

func (o *HTTPOutput) worker() {
	client := &http.Client{
		CheckRedirect: customCheckRedirect,
	}
	death_count := 0
	Loop:
		for {
			select {
				case data := <-o.buf:
				o.sendRequest(client, data)
				death_count = 0
			default:
				death_count += 1
				if death_count > 20 {
					break Loop
				} else {
					time.Sleep(time.Millisecond * 100)
				}

			}
		}
}

func (o *HTTPOutput) Write(data []byte) (n int, err error) {
	buf := make([]byte, len(data))
	copy(buf, data)

	o.buf <- buf
	buf_len := len(o.buf)
	o.bufStats.Write(len(o.buf))
	if buf_len > 20 {
		if len(o.need_worker) == 0 {
			o.need_worker <- buf_len
		}
	}
	return len(data), nil
}

func (o *HTTPOutput) sendRequest(client *http.Client, data []byte) {
	request, err := ParseRequest(data)

	if err != nil {
		log.Println("Cannot parse request", string(data), err)
		return
	}

	if len(o.methods) > 0 && !o.methods.Contains(request.Method) {
		return
	}

	// Change HOST of original request
	URL := o.address + request.URL.Path + "?" + request.URL.RawQuery

	request.RequestURI = ""
	request.URL, _ = url.ParseRequestURI(URL)

	for _, header := range o.headers {
		request.Header.Set(header.Name, header.Value)
	}

	start := time.Now()
	resp, err := client.Do(request)
	stop := time.Now()

	// We should not count Redirect as errors
	if urlErr, ok := err.(*url.Error); ok {
		if _, ok := urlErr.Err.(*RedirectNotAllowed); ok {
			err = nil
		}
	}

	if err == nil {
		defer resp.Body.Close()
	} else {
		log.Println("Request error:", err)
	}

	if o.elasticSearch != nil {
		o.elasticSearch.ResponseAnalyze(request, resp, start, stop)
	}
}

func (o *HTTPOutput) String() string {
	return "HTTP output: " + o.address
}
